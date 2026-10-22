// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const (
	// testJobUID is the lookup Job's UID. The adopt path splits on the owner
	// reference, so the holder either carries this or it does not.
	testJobUID = "job-uid"
	// testJobWorkloadName is the workload name the Job derives from its own,
	// matching getWorkloadName.
	testJobWorkloadName = testJobName + "-workload"
	// testTrainJobsResource is the resource name a TrainJob NotFound carries.
	testTrainJobsResource = "trainjobs"

	// holderOwn, holderForeign and holderForeignTerminating are the three
	// things that can already occupy the workload name.
	holderOwn                = "own"
	holderForeign            = "foreign"
	holderForeignTerminating = "foreign-terminating"
)

// newJobLookupWorkloadScheme adds the TrainJob types the Job tier creates.
func newJobLookupWorkloadScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newFallbackScheme(t)
	require.NoError(t, trainerv1alpha1.AddToScheme(s))
	return s
}

// newLookupJob returns a Job whose workload is a TrainJob, in the state the
// fixtures need: a recorded WorkloadRef for the lookup path, or none for the
// create path.
func newLookupJob(withRef bool) *nvcrev1alpha1.Job {
	job := &nvcrev1alpha1.Job{
		Name: testJobName, Namespace: testNS, UID: testJobUID, Generation: 1,
		Spec: nvcrev1alpha1.JobSpec{
			Workload: nvcrev1alpha1.WorkloadSpec{
				TrainJob: &trainerv1alpha1.TrainJobSpec{
					RuntimeRef: trainerv1alpha1.RuntimeRef{
						Name: "runtime", Kind: new("TrainingRuntime"),
					},
				},
			},
		},
	}
	if withRef {
		job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{
			APIVersion: trainerv1alpha1.GroupVersion.String(),
			Kind:       testKindTrainJob,
			Name:       testJobWorkloadName,
			Namespace:  testNS,
		}
		job.Status.Conditions = []metav1.Condition{{
			Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
			Reason: ReasonWorkloadCreated, Message: "Workload created",
			ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
		}}
	}
	return job
}

// newLookupTrainJob returns the Job's workload, owned by the Job unless the
// case asks for a stranger's. A "foreign-terminating" holder is a stranger
// that is already going away, which is what a predecessor Job of the same
// name leaves behind: the workload name is derived from the Job name, and the
// Job tier's finalizer deletes the workload and then waits out pod drain.
func newLookupTrainJob(holder string) *trainerv1alpha1.TrainJob {
	tj := &trainerv1alpha1.TrainJob{
		Name: testJobWorkloadName, Namespace: testNS, UID: "trainjob-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: nvcrev1alpha1.GroupVersion.String(),
			Kind:       kindJob,
			Name:       testJobName,
			UID:        testJobUID,
			Controller: new(true),
		}},
		Spec: trainerv1alpha1.TrainJobSpec{
			RuntimeRef: trainerv1alpha1.RuntimeRef{
				Name: "runtime", Kind: new("TrainingRuntime"),
			},
		},
	}
	if holder == holderForeign || holder == holderForeignTerminating {
		tj.OwnerReferences[0].Name = "some-other-job"
		tj.OwnerReferences[0].UID = "an-unrelated-uid"
	}
	if holder == holderForeignTerminating {
		// The fake client rejects a deletionTimestamp with no finalizer, and a
		// real terminating workload holds the name for the same reason: something
		// is still keeping it alive.
		now := metav1.Now()
		tj.DeletionTimestamp = &now
		tj.Finalizers = []string{"nvcre.nvidia.com/test-hold"}
	}
	return tj
}

// newWorkloadHidingClient wraps store so every TrainJob Get answers NotFound
// while the store itself still serves it, which is what a lagging informer
// does after a Create that has not landed in the cache yet. Only a read
// through APIReader can then see the workload, so a fixture using it fails if
// a confirming read is changed back to r.Get. When hide is false the store is
// returned untouched.
func newWorkloadHidingClient(store client.WithWatch, hide bool) client.WithWatch {
	if !hide {
		return store
	}
	return interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, isTrainJob := obj.(*trainerv1alpha1.TrainJob); isTrainJob {
				return apierrors.NewNotFound(schema.GroupResource{
					Group:    trainerv1alpha1.GroupVersion.Group,
					Resource: testTrainJobsResource,
				}, key.Name)
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})
}

// newWorkloadFailingReader wraps store so every TrainJob Get fails with msg,
// which is how a live read that failed for its own reasons is kept distinct
// from one that found nothing. An empty msg returns the store untouched.
func newWorkloadFailingReader(store client.WithWatch, msg string) client.Reader {
	if msg == "" {
		return store
	}
	return interceptor.NewClient(store, interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey,
			obj client.Object, opts ...client.GetOption,
		) error {
			if _, isTrainJob := obj.(*trainerv1alpha1.TrainJob); isTrainJob {
				return errors.New(msg)
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	})
}

// jobLookupOutput is what both suites record. Every field is a decision the
// reconciler made, so a branch cannot change without moving a golden.
type jobLookupOutput struct {
	RequeueAfter string `json:"requeueAfter"`
	Error        string `json:"error,omitempty"`
	// Phase is the condition type currently True, which is how a Job failed
	// over a workload that is still running becomes visible.
	Phase        string `json:"phase"`
	Reason       string `json:"reason"`
	WorkloadRef  string `json:"workloadRef"`
	RestartCount int32  `json:"restartCount"`
	// WorkloadSurvives reports whether the workload is still on the API
	// server. restartFromCheckpoint deletes it, so this is the destructive
	// half of the lookup branch.
	WorkloadSurvives bool `json:"workloadSurvives"`
}

// readJobOutput reads the persisted Job back through the API server and
// summarises it, so the goldens record what was written rather than what the
// caller's in-memory copy happens to hold.
func readJobOutput(
	ctx context.Context, apiServer client.Client, result ctrl.Result, reconcileErr error,
) (string, error) {
	out := jobLookupOutput{
		RequeueAfter: result.RequeueAfter.String(),
		Phase:        "None",
		Reason:       "None",
		WorkloadRef:  testNilRef,
	}
	if reconcileErr != nil {
		out.Error = reconcileErr.Error()
	}

	persisted := &nvcrev1alpha1.Job{}
	if err := apiServer.Get(ctx,
		client.ObjectKey{Namespace: testNS, Name: testJobName}, persisted); err != nil {
		return "", err
	}
	for _, t := range []string{
		nvcrev1alpha1.JobInProgress, nvcrev1alpha1.JobSucceeded, nvcrev1alpha1.JobFailed,
	} {
		if c := meta.FindStatusCondition(persisted.Status.Conditions, t); c != nil &&
			c.Status == metav1.ConditionTrue {
			out.Phase, out.Reason = t, c.Reason
		}
	}
	if persisted.Status.WorkloadRef != nil {
		out.WorkloadRef = persisted.Status.WorkloadRef.Name
	}
	out.RestartCount = persisted.Status.RestartCount

	tj := &trainerv1alpha1.TrainJob{}
	out.WorkloadSurvives = apiServer.Get(ctx,
		client.ObjectKey{Namespace: testNS, Name: testJobWorkloadName}, tj) == nil

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// A manager-backed reconciler must never confirm a cache miss against the same
// cache, so SetupWithManager supplies the uncached reader when the caller
// leaves it unset.
func TestJobSetupDefaultsAPIReader(t *testing.T) {
	mgr, err := ctrl.NewManager(&rest.Config{Host: testFakeAPIServerHost}, ctrl.Options{
		Scheme:     newJobLookupWorkloadScheme(t),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	require.NoError(t, err)
	r := &JobReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	require.Same(t, mgr.GetAPIReader(), r.APIReader)
}

// TestJobWorkloadLookup pins what updateStatusFromWorkload does when the
// workload named by status.WorkloadRef is missing from the cache.
//
// Nothing on that branch is reversible. With a restart budget it goes to
// restartFromCheckpoint, which spends one of maxRestarts and resets the stall
// state, and whose own cached read then skips the Delete, so the workload it
// believes it replaced keeps running and holding GPUs while a second one is
// created under the same name. Without a budget the Job is failed outright
// over a workload that never stopped.
//
// The reconciler's Client plays the informer cache and APIReader plays the API
// server, so each case sets exactly what each side can see.
func TestJobWorkloadLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "job-workload-lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// Job-scoped metrics are process-global and keyed by job name, which
		// every fixture here shares. Reconciling leaves label sets behind, and
		// the cardinality test counts whole collectors, so drop this Job's
		// series the way the Job tier itself does on deletion.
		defer cleanupJobMetrics(testNS, testJobName)

		var input struct {
			// WorkloadExists places the workload in the object store.
			WorkloadExists bool `yaml:"workloadExists"`
			// CacheHidesWorkload makes every cached TrainJob Get answer
			// NotFound while the store still serves it, which is what a
			// lagging informer looks like from inside the reconciler.
			CacheHidesWorkload bool `yaml:"cacheHidesWorkload"`
			// APIError, when set, fails the live read with this message, so a
			// read that failed for its own reasons cannot pass for an absence.
			APIError string `yaml:"apiError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupWorkloadScheme(t)

		// No checkpoint budget, so a confirmed deletion fails the Job outright
		// rather than routing to restartFromCheckpoint. Both are downstream of
		// the same branch; this is the one that needs no GoodputMeasurement
		// index to reach.
		job := newLookupJob(true)

		objects := []client.Object{job.DeepCopy()}
		if input.WorkloadExists {
			objects = append(objects, newLookupTrainJob(holderOwn))
		}
		// One store, served two ways. The reconciler reads and writes through
		// cached; the assertions read through apiServer. The only difference
		// between them is what the case asks cached to withhold, so a status
		// write cannot go missing between the two.
		apiServer := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&nvcrev1alpha1.Job{}).Build()
		if err := apiServer.Get(ctx,
			client.ObjectKey{Namespace: testNS, Name: testJobName}, job); err != nil {
			return err
		}

		cached := newWorkloadHidingClient(apiServer, input.CacheHidesWorkload)
		apiReader := newWorkloadFailingReader(apiServer, input.APIError)

		r := &JobReconciler{
			Client: cached, APIReader: apiReader, Scheme: scheme,
			Recorder: events.NewFakeRecorder(20),
		}
		result, reconcileErr := r.updateStatusFromWorkload(ctx, job)

		out, err := readJobOutput(ctx, apiServer, result, reconcileErr)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}

// TestJobCreateOrAdoptWorkload pins the create path.
//
// Creating the workload and recording status.WorkloadRef are two separate
// writes, and the reference is what routes every later reconcile away from
// this function. When the second write is lost the reference stays nil, the
// next pass lands back here, and the name is already taken. Before this
// change that was permanent: the Create failed identically on every pass,
// backing off forever over a workload this Job owns and is running.
func TestJobCreateOrAdoptWorkload(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "job-create-adopt-workload",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// Same reason as TestJobWorkloadLookup: this suite records job-scoped
		// series under a shared job name, so clear them once the case is done.
		defer cleanupJobMetrics(testNS, testJobName)

		var input struct {
			// Holder says who owns the workload already occupying the name:
			// "own" for this Job, "foreign" for an unrelated one,
			// "foreign-terminating" for an unrelated one already going away,
			// empty to leave the name free.
			Holder string `yaml:"holder"`
			// CacheHidesWorkload makes every cached TrainJob Get answer
			// NotFound while the API server still serves it, which is what a
			// lagging informer does after a Create. Only a read through
			// APIReader can then see the holder, so a fixture using it fails
			// if the confirming read is changed back to r.Get.
			CacheHidesWorkload bool `yaml:"cacheHidesWorkload"`
			// APIError, when set, fails the live read of the holder.
			APIError string `yaml:"apiError"`
			// StatusWriteFailures makes the first N status writes conflict.
			// updateStatusWithRetry refetches the Job in place on a 409, so a
			// WorkloadRef set on the caller's copy beforehand is dropped while
			// the write still reports success. That is how a Job ends up with
			// a live workload and no reference, which is the state the
			// AlreadyExists branch exists to dig out of.
			StatusWriteFailures int `yaml:"statusWriteFailures"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupWorkloadScheme(t)

		job := newLookupJob(false)
		objects := []client.Object{job.DeepCopy()}
		if input.Holder != "" {
			objects = append(objects, newLookupTrainJob(input.Holder))
		}
		// One store, served two ways: the reconciler reads through cached and
		// the assertions read through apiServer, so the only difference is
		// what the case asks cached to withhold.
		apiServer := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&nvcrev1alpha1.Job{}).Build()
		// Take the stored resourceVersion, so the only conflicts a case sees
		// are the ones it asks for. Reconciling a copy the builder has already
		// moved on from would conflict on every fixture and blur what
		// status-conflict-keeps-ref is pinning.
		if err := apiServer.Get(ctx,
			client.ObjectKey{Namespace: testNS, Name: testJobName}, job); err != nil {
			return err
		}

		cached := newWorkloadHidingClient(apiServer, input.CacheHidesWorkload)
		apiReader := newWorkloadFailingReader(apiServer, input.APIError)

		remaining := input.StatusWriteFailures
		if remaining > 0 {
			cached = interceptor.NewClient(cached, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, inner client.Client, sub string,
					obj client.Object, opts ...client.SubResourceUpdateOption,
				) error {
					if _, isJob := obj.(*nvcrev1alpha1.Job); isJob && remaining > 0 {
						remaining--
						return apierrors.NewConflict(schema.GroupResource{
							Group:    nvcrev1alpha1.GroupVersion.Group,
							Resource: testJobsResource,
						}, obj.GetName(), errors.New("simulated conflict"))
					}
					return inner.Status().Update(ctx, obj, opts...)
				},
			})
		}

		r := &JobReconciler{
			Client: cached, APIReader: apiReader, Scheme: scheme,
			Recorder: events.NewFakeRecorder(20),
		}
		result, reconcileErr := r.createWorkloadFromSpec(ctx, job)

		out, err := readJobOutput(ctx, apiServer, result, reconcileErr)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}
