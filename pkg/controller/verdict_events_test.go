// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// testReasonThresholdViolated is the violation reason the threshold evaluator
// produces (pkg/threshold), surfaced verbatim on the Job's ValidationFailed
// condition. It is a literal there rather than a controller constant.
const testReasonThresholdViolated = "ThresholdViolated"

const (
	testStatusHardError           = "hard-error"
	testStatusConflictsExhausted  = "exhausted-conflicts"
	testStatusConflictThenSuccess = "conflict-then-success"
)

func TestJobVerdictEventsRequireSuccessfulStatusWrite(t *testing.T) {
	for _, verdict := range []string{"hardware", "validation-failed", "validation-passed"} {
		for _, mode := range []string{testStatusHardError, testStatusConflictsExhausted, testStatusConflictThenSuccess} {
			t.Run(verdict+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				r, job, recorder := newJobVerdictFixture(t)
				c, ok := r.Client.(client.WithWatch)
				require.True(t, ok, "fixture must use a watch-capable fake client")
				attempts := 0
				r.Client = interceptor.NewClient(c, interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, _ string,
						obj client.Object, opts ...client.SubResourceUpdateOption) error {
						attempts++
						if mode == testStatusHardError {
							return errSimulatedStatus
						}
						if mode == testStatusConflictsExhausted || attempts == 1 {
							return apierrors.NewConflict(schema.GroupResource{Resource: testJobsResource}, obj.GetName(), errSimulatedStatus)
						}
						return c.Status().Update(ctx, obj, opts...)
					},
				})
				var err error
				switch verdict {
				case "hardware":
					err = r.setJobHardwareFailed(ctx, job, ReasonHardwareFailureDetected, "hardware failed",
						[]nvcrev1alpha1.FailedNode{{Name: testNodeA}})
				case "validation-failed":
					err = r.setJobValidationStatus(ctx, job, metav1.ConditionTrue, testReasonThresholdViolated, "failed")
				case "validation-passed":
					err = r.setJobValidationStatus(ctx, job, metav1.ConditionFalse, reasonThresholdsMet, "passed")
				}
				persisted := &nvcrev1alpha1.Job{}
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(job), persisted))
				if mode == testStatusConflictThenSuccess {
					require.NoError(t, err)
					require.Equal(t, 2, attempts)
					require.Len(t, recorder.Events, 1)
					require.Len(t, persisted.Status.Conditions, 1)
				} else {
					require.Error(t, err)
					require.Empty(t, recorder.Events)
					require.Empty(t, persisted.Status.Conditions)
					require.Empty(t, persisted.Status.FailedNodes)
					if mode == testStatusConflictsExhausted {
						require.Greater(t, attempts, 1)
					}
				}
			})
		}
	}
}

// newJobVerdictFixture builds a Job whose group spans one node, wired to a fake
// client and a FakeRecorder. The verdict setters are additive writers outside
// the exclusive set, so they are driven directly rather than through Reconcile.
func newJobVerdictFixture(t *testing.T) (*JobReconciler, *nvcrev1alpha1.Job, *events.FakeRecorder) {
	t.Helper()
	job := &nvcrev1alpha1.Job{
		Name:      "verdict-job",
		Namespace: testNS,
		Annotations: map[string]string{
			"nvcre.nvidia.com/group-nodes": testNodeA,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newWorkflowScheme(t)).
		WithObjects(job).
		WithStatusSubresource(&nvcrev1alpha1.Job{}).
		Build()
	current := &nvcrev1alpha1.Job{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(job), current))
	recorder := events.NewFakeRecorder(10)
	return &JobReconciler{Client: c, Recorder: recorder}, current, recorder
}

// TestJobHardwareVerdictEmitsOnceOnFlip pins ADR-080's rule that the hardware
// verdict announces the flip to True, not every pass that grows failedNodes.
// The second detection adds a node while the condition is already True and must
// stay silent; status carries the list.
func TestJobHardwareVerdictEmitsOnceOnFlip(t *testing.T) {
	ctx := context.Background()
	r, job, recorder := newJobVerdictFixture(t)

	require.NoError(t, r.setJobHardwareFailed(ctx, job,
		ReasonHardwareFailureDetected, "Hardware failure detected on node(s): [node-a]",
		[]nvcrev1alpha1.FailedNode{{
			Name:    testNodeA,
			Reason:  ReasonHardwareFailureDetected,
			Message: testNodeFailureDetail,
		}}))
	require.Len(t, recorder.Events, 1)
	require.Equal(t,
		"Warning HardwareFailureDetected Hardware failure detected on node(s): [node-a]",
		<-recorder.Events)

	// A later pass adds a second failed node. The condition is already True, so
	// its status does not flip and no second Warning is emitted.
	require.NoError(t, r.setJobHardwareFailed(ctx, job,
		ReasonHardwareFailureDetected, "Hardware failure detected on node(s): [node-a node-b]",
		[]nvcrev1alpha1.FailedNode{
			{Name: testNodeA, Reason: ReasonHardwareFailureDetected, Message: testNodeFailureDetail},
			{Name: "node-b", Reason: ReasonHardwareFailureDetected, Message: testNodeFailureDetail},
		}))
	require.Empty(t, recorder.Events)

	require.True(t, condIsTrue(job.Status.Conditions, nvcrev1alpha1.JobHardwareFailed))
	require.Len(t, job.Status.FailedNodes, 2)
}

// TestJobValidationVerdictEvents covers both directions of the threshold
// verdict. The pass is the only event in this design keyed on a condition being
// written False, so a Job that ran and passed is distinguishable from one whose
// validation has not run yet.
func TestJobValidationVerdictEvents(t *testing.T) {
	t.Run("pass emits normal thresholds met once across repeated writes", func(t *testing.T) {
		ctx := context.Background()
		r, job, recorder := newJobVerdictFixture(t)

		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionFalse,
			reasonThresholdsMet, "All performance thresholds satisfied"))
		require.Len(t, recorder.Events, 1)
		require.Equal(t,
			"Normal ThresholdsMet All performance thresholds satisfied",
			<-recorder.Events)
		require.Empty(t, job.Status.FailedNodes,
			"a passing verdict must not seed failedNodes")
		for range 4 {
			require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionFalse,
				reasonThresholdsMet, "All performance thresholds satisfied"))
		}
		require.Empty(t, recorder.Events, "repeated passing verdicts must not emit again")
		require.Empty(t, job.Status.FailedNodes)
	})

	t.Run("violation emits warning", func(t *testing.T) {
		ctx := context.Background()
		r, job, recorder := newJobVerdictFixture(t)

		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionTrue,
			testReasonThresholdViolated, "Threshold \"busBandwidthGBps\" violated"))
		require.Len(t, recorder.Events, 1)
		require.Equal(t,
			"Warning ThresholdViolated Threshold \"busBandwidthGBps\" violated",
			<-recorder.Events)
	})

	t.Run("pass then violation emits both", func(t *testing.T) {
		ctx := context.Background()
		r, job, recorder := newJobVerdictFixture(t)

		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionFalse,
			reasonThresholdsMet, "All performance thresholds satisfied"))
		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionTrue,
			testReasonThresholdViolated, "Threshold violated on a later measurement"))

		require.Len(t, recorder.Events, 2)
		require.Equal(t,
			"Normal ThresholdsMet All performance thresholds satisfied",
			<-recorder.Events)
		require.Equal(t,
			"Warning ThresholdViolated Threshold violated on a later measurement",
			<-recorder.Events)
	})

	t.Run("repeat with a changed message is silent", func(t *testing.T) {
		ctx := context.Background()
		r, job, recorder := newJobVerdictFixture(t)

		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionTrue,
			testReasonThresholdViolated, "Threshold violated: measured 370.14"))
		require.Len(t, recorder.Events, 1)
		<-recorder.Events

		// Same status, new reason and message. Not a flip, so it emits nothing
		// even though the write itself changes the object.
		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionTrue,
			reasonUnknownThresholdKey, "Threshold violated: measured 368.02"))
		require.Empty(t, recorder.Events)
	})

}

// TestJobVerdictEventsNilRecorder pins that both verdict setters tolerate an
// unset Recorder, matching TestJobWarnfNilRecorder. A JobReconciler constructed
// directly in a unit test has no recorder.
func TestJobVerdictEventsNilRecorder(t *testing.T) {
	ctx := context.Background()
	r, job, _ := newJobVerdictFixture(t)
	r.Recorder = nil

	require.NotPanics(t, func() {
		require.NoError(t, r.setJobValidationStatus(ctx, job, metav1.ConditionFalse,
			reasonThresholdsMet, "All performance thresholds satisfied"))
		require.NoError(t, r.setJobHardwareFailed(ctx, job,
			ReasonHardwareFailureDetected, "Hardware failure detected",
			[]nvcrev1alpha1.FailedNode{{
				Name:   testNodeA,
				Reason: ReasonHardwareFailureDetected,
			}}))
	})
}

// TestJobRestartDoesNotReAnnounceInProgress covers the checkpoint-restart rule
// from ADR-080's testing plan at the recorder level. A restart rewrites
// InProgress with a new reason (WorkloadRestarting, then WorkloadRunning); the
// phase never flips, so only the first write may emit.
//
// This complements the running-manager recorder assertion in integration.
// Neither test relies on a complete Event golden: checkpoint restart can also
// emit timing-dependent WorkloadCreationError diagnostics during cache catch-up.
func TestJobRestartDoesNotReAnnounceInProgress(t *testing.T) {
	ctx := context.Background()
	r, job, recorder := newJobVerdictFixture(t)

	require.NoError(t, r.setExclusiveCondition(ctx, job,
		nvcrev1alpha1.JobInProgress, "WorkloadCreated", "Workload TrainJob/job-workload created"))
	require.Len(t, recorder.Events, 1)
	require.Equal(t,
		"Normal WorkloadCreated Workload TrainJob/job-workload created",
		<-recorder.Events)

	// Drive the actual restart method, including workload deletion and the
	// persisted restart count, rather than synthesizing its condition write.
	require.NoError(t, trainerv1alpha1.AddToScheme(r.Client.Scheme()))
	trainJob := &trainerv1alpha1.TrainJob{Name: "restart-workload", Namespace: testNS}
	require.NoError(t, r.Create(ctx, trainJob))
	maxRestarts := int32(3)
	job.Spec.Checkpoint = &nvcrev1alpha1.CheckpointConfig{MaxRestarts: &maxRestarts}
	job.Spec.Workload.TrainJob = &trainerv1alpha1.TrainJobSpec{}
	ref := &nvcrev1alpha1.WorkloadReference{Name: trainJob.Name, Namespace: testNS}
	job.Status.WorkloadRef = ref
	adapter, err := workload.ForSpec(&job.Spec.Workload)
	require.NoError(t, err)
	_, err = r.restartFromCheckpoint(ctx, job, ref, adapter)
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Status.RestartCount)
	require.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKeyFromObject(trainJob), &trainerv1alpha1.TrainJob{})))
	require.NoError(t, r.setExclusiveCondition(ctx, job,
		nvcrev1alpha1.JobInProgress, "WorkloadRunning", "Workload is running"))
	require.Empty(t, recorder.Events)

	// Leaving the phase emits exactly once more.
	require.NoError(t, r.setExclusiveCondition(ctx, job,
		nvcrev1alpha1.JobSucceeded, "WorkloadCompleted", "All workers completed"))
	require.Len(t, recorder.Events, 1)
	require.Equal(t,
		"Normal WorkloadCompleted All workers completed",
		<-recorder.Events)
}
