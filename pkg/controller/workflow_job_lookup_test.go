// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	resourcev1 "k8s.io/api/resource/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const (
	// testWorkflowName is the Workflow name used by the job-lookup fixtures.
	testWorkflowName = "wf"
	// testGroupName is the orchestration group name used by those fixtures.
	testGroupName = "g0"
	// testGroupZero is the controller's default first group name, used by the
	// fixtures that run the real naming.
	testGroupZero = "group-0"
	// testGroupJobName is the Job that group runs.
	testGroupJobName = "g0-job"
	// testClaimTemplateName is the group's job-scoped DRA dependency, the one
	// cleanupScopedDependencies revokes when the group is failed.
	testClaimTemplateName = "g0-roce-channel"
)

// newJobLookupScheme adds the DRA group so the job-scoped dependency can be
// fetched and deleted as an unstructured object, the way the controller does.
func newJobLookupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newFallbackScheme(t)
	require.NoError(t, resourcev1.AddToScheme(s))
	return s
}

// A manager-backed reconciler must never confirm a cache miss against the same
// cache, so SetupWithManager supplies the uncached reader when the caller
// leaves it unset (issue #385).
func TestWorkflowSetupDefaultsAPIReader(t *testing.T) {
	mgr, err := ctrl.NewManager(&rest.Config{Host: testFakeAPIServerHost}, ctrl.Options{
		Scheme:     newFallbackScheme(t),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	require.NoError(t, err)
	r := &WorkflowReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	require.Same(t, mgr.GetAPIReader(), r.APIReader)
}

// TestWorkflowJobLookup pins what updateStatusFromJobs does when a running
// group's Job is missing from the cache (issue #385).
//
// Nothing on that branch is reversible: it revokes the group's job-scoped DRA
// dependency out from under whatever pods are running, fails the group, and
// clears JobRef so the Job is never re-adopted and never cleaned up. So the
// cache must not be the only witness to the Job's absence.
//
// The reconciler's Client plays the informer cache and APIReader plays the API
// server, so each case sets exactly what each side can see. The Workflow is on
// both sides, as it is in reality: once the Job's absence is confirmed, the
// branch also confirms against the live Workflow that the group is still
// Running on that Job (groupViewIsStale), and that read is counted too.
func TestWorkflowJobLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workflow-job-lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// InCache and InAPI place the group's Job in the cache and on the
			// API server respectively.
			InCache bool `yaml:"inCache"`
			InAPI   bool `yaml:"inAPI"`
			// APIError, when set, fails every live read with this message.
			APIError string `yaml:"apiError"`
			// CacheError, when set, fails every cached Job read with this
			// message, so the two wrappers stay distinguishable.
			CacheError string `yaml:"cacheError"`
			// Passes is how many times to call updateStatusFromJobs.
			Passes int `yaml:"passes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupScheme(t)

		workflow := &nvcrev1alpha1.Workflow{
			Name: testWorkflowName, Namespace: testNS, Generation: 1,
			Spec: nvcrev1alpha1.WorkflowSpec{
				Orchestration: nvcrev1alpha1.OrchestrationSpec{Iterations: 1},
			},
			Status: nvcrev1alpha1.WorkflowStatus{
				// The state the Workflow is in when #385 bites: it has just
				// created the Job and has not yet observed it running. Seeding
				// the pre-create reason keeps the InProgress write this function
				// makes visible in the golden, so a group left running by the
				// confirming read cannot be confused with one silently skipped.
				Conditions: []metav1.Condition{{
					Type: nvcrev1alpha1.WorkflowInProgress, Status: metav1.ConditionTrue,
					Reason: ReasonJobCreated, Message: "Created Job for group " + testGroupName,
					ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
				}},
				DependencyRefs: []nvcrev1alpha1.DependencyResourceRef{{
					APIVersion: resourcev1.SchemeGroupVersion.String(),
					Kind:       "ResourceClaimTemplate",
					Name:       testClaimTemplateName,
					Namespace:  testNS,
					Scope:      "job",
					GroupName:  testGroupName,
					Iteration:  1,
				}},
				Orchestration: &nvcrev1alpha1.OrchestrationStatus{
					CurrentIteration: 1,
					Groups: []nvcrev1alpha1.GroupStatus{{
						Name:   testGroupName,
						Phase:  nvcrev1alpha1.GroupRunning,
						Nodes:  []string{testNodeA},
						JobRef: &nvcrev1alpha1.WorkloadReference{Name: testGroupJobName, Namespace: testNS},
					}},
				},
			},
		}
		// The dependency carries this Workflow's tracking label, so the
		// ownership gate in cleanupScopedDependencies lets the delete through.
		// Without that the destructive half of the branch would be invisible.
		claim := &unstructured.Unstructured{}
		claim.SetAPIVersion(resourcev1.SchemeGroupVersion.String())
		claim.SetKind("ResourceClaimTemplate")
		claim.SetName(testClaimTemplateName)
		claim.SetNamespace(testNS)
		claim.SetLabels(map[string]string{labelWorkflowTracking: testWorkflowName})
		newJob := func() *nvcrev1alpha1.Job {
			return &nvcrev1alpha1.Job{Name: testGroupJobName, Namespace: testNS}
		}

		cacheObjects := []client.Object{workflow, claim}
		if input.InCache {
			cacheObjects = append(cacheObjects, newJob())
		}
		cache := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cacheObjects...).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{}, &nvcrev1alpha1.Job{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, ok := obj.(*nvcrev1alpha1.Job); ok && input.CacheError != "" {
						return errors.New(input.CacheError)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).
			Build()

		apiReads := 0
		apiBuilder := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(workflow.DeepCopy()).
			WithStatusSubresource(&nvcrev1alpha1.Workflow{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					apiReads++
					if input.APIError != "" {
						return errors.New(input.APIError)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
		if input.InAPI {
			apiBuilder = apiBuilder.WithObjects(newJob()).
				WithStatusSubresource(&nvcrev1alpha1.Job{})
		}

		r := &WorkflowReconciler{
			Client: cache, APIReader: apiBuilder.Build(), Scheme: scheme,
			JobRequeueInterval: time.Second,
		}

		type pass struct {
			RequeueAfter string `json:"requeueAfter"`
			Error        string `json:"error,omitempty"`
			GroupPhase   string `json:"groupPhase"`
			JobRef       string `json:"jobRef"`
			Dependencies int    `json:"trackedDependencies"`
			ClaimExists  bool   `json:"claimTemplateExists"`
			// The persisted InProgress condition. This is where a group that
			// was left running shows up: the tail write only happens when the
			// loop reports work still in flight.
			InProgress string `json:"inProgressCondition"`
		}
		passes := []pass{}
		for range input.Passes {
			result, err := r.updateStatusFromJobs(ctx, workflow)
			current := &nvcrev1alpha1.Workflow{}
			if getErr := cache.Get(ctx,
				types.NamespacedName{Name: testWorkflowName, Namespace: testNS}, current); getErr != nil {
				return getErr
			}
			probe := &unstructured.Unstructured{}
			probe.SetAPIVersion(resourcev1.SchemeGroupVersion.String())
			probe.SetKind("ResourceClaimTemplate")
			claimErr := cache.Get(ctx,
				types.NamespacedName{Name: testClaimTemplateName, Namespace: testNS}, probe)
			if claimErr != nil && !apierrors.IsNotFound(claimErr) {
				return claimErr
			}
			g := current.Status.Orchestration.Groups[0]
			p := pass{
				RequeueAfter: result.RequeueAfter.String(),
				GroupPhase:   string(g.Phase),
				JobRef:       testNilRef,
				Dependencies: len(current.Status.DependencyRefs),
				ClaimExists:  claimErr == nil,
			}
			if err != nil {
				p.Error = err.Error()
			}
			if g.JobRef != nil {
				p.JobRef = g.JobRef.Name
			}
			if c := meta.FindStatusCondition(current.Status.Conditions,
				nvcrev1alpha1.WorkflowInProgress); c != nil {
				p.InProgress = string(c.Status) + "/" + c.Reason + ": " + c.Message
			}
			passes = append(passes, p)
		}

		output := struct {
			Passes   []pass `json:"passes"`
			APIReads int    `json:"apiReads"`
		}{Passes: passes, APIReads: apiReads}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestWorkflowCreateOrAdoptJobLookup covers the other cached read on this path.
// When the Workflow's own view is stale it re-enters createOrAdoptJob, Create
// comes back AlreadyExists, and the Job it just created may still be missing
// from the cache. Reading that holder live is what turns a spurious "failed to
// get existing Job" into an adoption (issue #385).
func TestWorkflowCreateOrAdoptJobLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workflow-create-adopt-job",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Holder decides who owns the Job already carrying the name:
			// "own", "own-terminating", "foreign", or "foreign-terminating".
			Holder string `yaml:"holder"`
			// InCache also places that holder in the cache.
			InCache bool `yaml:"inCache"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupScheme(t)
		workflow := &nvcrev1alpha1.Workflow{
			Name: testWorkflowName, Namespace: testNS, UID: "workflow-uid",
		}

		holder := &nvcrev1alpha1.Job{Name: testGroupJobName, Namespace: testNS, UID: "job-uid"}
		ownerUID := workflow.UID
		if strings.HasPrefix(input.Holder, "foreign") {
			ownerUID = "a-different-workflow-uid"
		}
		holder.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: nvcrev1alpha1.GroupVersion.String(),
			Kind:       "Workflow",
			Name:       testWorkflowName,
			UID:        ownerUID,
			Controller: new(true),
		}}
		if strings.HasSuffix(input.Holder, "-terminating") {
			now := metav1.Now()
			holder.DeletionTimestamp = &now
			holder.Finalizers = []string{jobFinalizer}
		}

		cacheObjects := []client.Object{workflow}
		if input.InCache {
			cacheObjects = append(cacheObjects, holder.DeepCopy())
		}
		cache := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cacheObjects...).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
					opts ...client.CreateOption,
				) error {
					// The API server holds the name whether or not the cache
					// knows about it, so the Create always collides.
					if _, ok := obj.(*nvcrev1alpha1.Job); ok {
						return apierrors.NewAlreadyExists(
							nvcrev1alpha1.GroupVersion.WithResource(testJobsResource).GroupResource(),
							obj.GetName())
					}
					return c.Create(ctx, obj, opts...)
				},
			}).
			Build()
		apiServer := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(holder.DeepCopy()).Build()

		r := &WorkflowReconciler{Client: cache, APIReader: apiServer, Scheme: scheme}
		job := &nvcrev1alpha1.Job{Name: testGroupJobName, Namespace: testNS}
		err := r.createOrAdoptJob(ctx, workflow, job)

		output := struct {
			Error     string `json:"error"`
			Collision bool   `json:"nameCollision"`
			JobUID    string `json:"jobUIDAfterCall"`
		}{Error: testNilRef, JobUID: string(job.UID)}
		if err != nil {
			output.Error = err.Error()
			_, output.Collision = errors.AsType[*nameCollisionError](err)
		}
		data, marshalErr := json.MarshalIndent(output, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestWorkflowDeletionJobLookup pins the teardown half of the same defect.
// handleDeletion lists the Workflow's Jobs and reads an empty list as proof
// they have all drained: Phase 1 deletes nothing, the drain wait is skipped,
// and Phase 2 revokes the job-scoped DRA dependencies backing the workload's
// pods. Deleting a Workflow shortly after a group's Job is created reaches
// that through cache lag alone, and the result is the CUDA 719 failure the
// pod-drain barrier from #121 exists to prevent (issue #385).
//
// The reconciler's Client plays the informer cache and APIReader plays the API
// server, so each case sets exactly what each side can see.
func TestWorkflowDeletionJobLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workflow-deletion-job-lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// InCache and InAPI place the group's Job in the cache and on the
			// API server respectively.
			InCache bool `yaml:"inCache"`
			InAPI   bool `yaml:"inAPI"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupScheme(t)

		now := metav1.Now()
		workflow := &nvcrev1alpha1.Workflow{
			Name: testWorkflowName, Namespace: testNS, UID: "workflow-uid",
			Finalizers:        []string{workflowFinalizer},
			DeletionTimestamp: &now,
			Status: nvcrev1alpha1.WorkflowStatus{
				DependencyRefs: []nvcrev1alpha1.DependencyResourceRef{{
					APIVersion: resourcev1.SchemeGroupVersion.String(),
					Kind:       "ResourceClaimTemplate",
					Name:       testClaimTemplateName,
					Namespace:  testNS,
					Scope:      "job",
					GroupName:  testGroupName,
				}},
			},
		}

		// The group's job-scoped DRA dependency. Phase 2 deletes it, so whether
		// it survives the call is the whole point of the fixture.
		claim := &unstructured.Unstructured{}
		claim.SetAPIVersion(resourcev1.SchemeGroupVersion.String())
		claim.SetKind("ResourceClaimTemplate")
		claim.SetName(testClaimTemplateName)
		claim.SetNamespace(testNS)
		claim.SetAnnotations(map[string]string{annotationWorkflowUID: string(workflow.UID)})

		newJob := func() *nvcrev1alpha1.Job {
			return &nvcrev1alpha1.Job{
				Name: testGroupJobName, Namespace: testNS, UID: "job-uid",
				Labels:     map[string]string{labelWorkflowTracking: testWorkflowName},
				Finalizers: []string{jobFinalizer},
			}
		}

		cacheObjects := []client.Object{workflow, claim}
		if input.InCache {
			cacheObjects = append(cacheObjects, newJob())
		}
		cache := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cacheObjects...).Build()

		apiBuilder := fake.NewClientBuilder().WithScheme(scheme)
		if input.InAPI {
			apiBuilder = apiBuilder.WithObjects(newJob())
		}

		r := &WorkflowReconciler{Client: cache, APIReader: apiBuilder.Build(), Scheme: scheme}
		result, reconcileErr := r.handleDeletion(ctx, workflow)

		// Read the dependency back through the cache, which is where Phase 2
		// would have deleted it.
		probe := &unstructured.Unstructured{}
		probe.SetAPIVersion(resourcev1.SchemeGroupVersion.String())
		probe.SetKind("ResourceClaimTemplate")
		claimErr := cache.Get(ctx,
			client.ObjectKey{Namespace: testNS, Name: testClaimTemplateName}, probe)

		output := struct {
			RequeueAfter  string `json:"requeueAfter"`
			Error         string `json:"error,omitempty"`
			ClaimSurvives bool   `json:"jobScopedDependencySurvives"`
			FinalizerHeld bool   `json:"finalizerStillHeld"`
		}{
			RequeueAfter:  result.RequeueAfter.String(),
			ClaimSurvives: claimErr == nil,
			FinalizerHeld: controllerutil.ContainsFinalizer(workflow, workflowFinalizer),
		}
		if reconcileErr != nil {
			output.Error = reconcileErr.Error()
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
