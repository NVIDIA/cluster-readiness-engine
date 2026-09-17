// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// Observe calls before API aggregation. Forward every call unchanged, including
// timing-dependent action errors. Store only scalars, never controller objects.
type phaseCountingRecorder struct {
	events.EventRecorder
	mu         sync.Mutex
	inProgress map[types.UID]int
}

func (r *phaseCountingRecorder) Eventf(
	regarding, related runtime.Object, eventType, reason, action, note string, args ...any,
) {
	var condition *metav1.Condition
	var uid types.UID
	switch obj := regarding.(type) {
	case *nvcrev1alpha1.Job:
		condition = meta.FindStatusCondition(obj.Status.Conditions, nvcrev1alpha1.JobInProgress)
		uid = obj.UID
	case *nvcrev1alpha1.Certification:
		condition = meta.FindStatusCondition(obj.Status.Conditions, nvcrev1alpha1.CertificationInProgress)
		uid = obj.UID
	}
	if eventType == corev1.EventTypeNormal {
		if condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == reason &&
			condition.Message == fmt.Sprintf(note, args...) {
			r.mu.Lock()
			r.inProgress[uid]++
			r.mu.Unlock()
		}
	}
	r.EventRecorder.Eventf(regarding, related, eventType, reason, action, note, args...)
}

// Observe actual persisted countdown changes, not elapsed sleeps or synthetic
// status writes. The caller releases the matching Node only after this returns.
func waitForNodePollMessages(t *testing.T, c client.Client, cfg waitConfig, deadline time.Time) types.UID {
	t.Helper()
	require.Equal(t, "Certification", cfg.WaitFor.Kind)
	ctx, cancel := contextForDeadline(deadline)
	defer cancel()
	key := client.ObjectKey{Name: cfg.WaitFor.Name, Namespace: cfg.WaitFor.Namespace}
	var uid types.UID
	messages := make(map[string]bool)
	require.Eventually(t, func() bool {
		cert := &nvcrev1alpha1.Certification{}
		if c.Get(ctx, key, cert) != nil {
			return false
		}
		if uid == "" {
			uid = cert.UID
		}
		require.Equal(t, uid, cert.UID)
		condition := meta.FindStatusCondition(cert.Status.Conditions, nvcrev1alpha1.CertificationInProgress)
		if condition != nil && condition.Status == metav1.ConditionTrue && condition.Reason == "WaitingForNodes" {
			messages[condition.Message] = true
		}
		return len(messages) >= 3
	}, boundedWaitTimeout(t, 15*time.Second, deadline), 50*time.Millisecond,
		"expected initial WaitingForNodes message and two distinct countdown updates")
	return uid
}

func (r *phaseCountingRecorder) count(uid types.UID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inProgress[uid]
}

func TestPhaseCountingRecorderForwardsAndCountsByUID(t *testing.T) {
	forward := events.NewFakeRecorder(10)
	r := &phaseCountingRecorder{EventRecorder: forward, inProgress: make(map[types.UID]int)}
	job := &nvcrev1alpha1.Job{UID: "original-job", Status: nvcrev1alpha1.JobStatus{
		Conditions: []metav1.Condition{{Type: nvcrev1alpha1.JobInProgress, Status: metav1.ConditionTrue,
			Reason: "WorkloadRunning", Message: "running"}},
	}}
	for range 2 {
		r.Eventf(job, nil, corev1.EventTypeNormal, "WorkloadRunning", "WorkloadRunning", "%s", "running")
	}
	require.Equal(t, 2, r.count(job.UID), "duplicate calls must remain visible before API aggregation")
	r.Eventf(job, nil, corev1.EventTypeWarning, "WorkloadCreationError", "WorkloadCreationError", "already exists")
	r.Eventf(job, nil, corev1.EventTypeNormal, "ThresholdsMet", "ThresholdsMet", "thresholds passed")
	require.Equal(t, 2, r.count(job.UID), "other diagnostics must not count as InProgress transitions")
	job.UID = "different-job"
	r.Eventf(job, nil, corev1.EventTypeNormal, "WorkloadRunning", "WorkloadRunning", "running")
	require.Equal(t, 2, r.count("original-job"))
	require.Equal(t, 1, r.count(job.UID))
	require.Len(t, forward.Events, 5, "all calls must reach the real recorder unchanged")
	require.Equal(t, "Normal WorkloadRunning running", <-forward.Events)
	require.Equal(t, "Normal WorkloadRunning running", <-forward.Events)
	require.Equal(t, "Warning WorkloadCreationError already exists", <-forward.Events)
	require.Equal(t, "Normal ThresholdsMet thresholds passed", <-forward.Events)
	require.Equal(t, "Normal WorkloadRunning running", <-forward.Events)
	cert := &nvcrev1alpha1.Certification{}
	cert.UID = "polling-cert"
	cert.Status.Conditions = []metav1.Condition{{Type: nvcrev1alpha1.CertificationInProgress,
		Status: metav1.ConditionTrue, Reason: "WaitingForNodes", Message: "retrying"}}
	for range 2 {
		r.Eventf(cert, nil, corev1.EventTypeNormal, "WaitingForNodes", "WaitingForNodes", "%s", "retrying")
	}
	require.Equal(t, 2, r.count(cert.UID), "Certification duplicates must also remain visible")
	require.Equal(t, "Normal WaitingForNodes retrying", <-forward.Events)
	require.Equal(t, "Normal WaitingForNodes retrying", <-forward.Events)
}

type checkpointObservation struct {
	jobKey, workloadKey         client.ObjectKey
	jobUID, originalWorkloadUID types.UID
	recorder                    *phaseCountingRecorder
}

func newCheckpointObservation(t *testing.T, c client.Client, cfg waitConfig) *checkpointObservation {
	t.Helper()
	if !cfg.VerifyCheckpointEvents {
		return nil
	}
	require.Equal(t, kindJob, cfg.WaitFor.Kind)
	jobKey := client.ObjectKey{Name: cfg.WaitFor.Name, Namespace: cfg.WaitFor.Namespace}
	job := &nvcrev1alpha1.Job{}
	require.NoError(t, c.Get(context.Background(), jobKey, job))
	require.NotNil(t, job.Status.WorkloadRef)
	workloadKey := client.ObjectKey{Name: job.Status.WorkloadRef.Name, Namespace: job.Namespace}
	original := &trainerv1alpha1.TrainJob{}
	require.NoError(t, c.Get(context.Background(), workloadKey, original))
	require.NotEmpty(t, job.UID)
	require.NotEmpty(t, original.UID)
	return &checkpointObservation{
		jobKey: jobKey, workloadKey: workloadKey, jobUID: job.UID, originalWorkloadUID: original.UID,
		recorder: &phaseCountingRecorder{inProgress: make(map[types.UID]int)},
	}
}

func (o *checkpointObservation) waitForInitialEvent(t *testing.T, deadline time.Time) {
	t.Helper()
	require.Eventually(t, func() bool { return o.recorder.count(o.jobUID) > 0 },
		boundedWaitTimeout(t, 10*time.Second, deadline), 10*time.Millisecond)
	require.Equal(t, 1, o.recorder.count(o.jobUID))
}

func (o *checkpointObservation) verifyReplacementReconcile(
	t *testing.T, direct, cached client.Client, cfg waitConfig, deadline time.Time,
) {
	t.Helper()
	ctx, cancel := contextForDeadline(deadline)
	defer cancel()
	var replacementUID types.UID
	require.Eventually(t, func() bool {
		replacement, observed := &trainerv1alpha1.TrainJob{}, &trainerv1alpha1.TrainJob{}
		if direct.Get(ctx, o.workloadKey, replacement) != nil || cached.Get(ctx, o.workloadKey, observed) != nil {
			return false
		}
		if replacement.UID == o.originalWorkloadUID || replacement.UID != observed.UID {
			return false
		}
		replacementUID = replacement.UID
		return true
	}, boundedWaitTimeout(t, 30*time.Second, deadline), 50*time.Millisecond,
		"replacement TrainJob UID was not observed by the manager cache")

	// A reason-only probe gives an observable acknowledgement of a subsequent
	// reconcile. Waiting only for the replacement to exist could pass before its
	// post-write Eventf call. Keep the phase True and never synthesize an Event.
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		job := &nvcrev1alpha1.Job{}
		if err := direct.Get(ctx, o.jobKey, job); err != nil {
			return err
		}
		require.Equal(t, o.jobUID, job.UID)
		require.EqualValues(t, 1, job.Status.RestartCount)
		condition := meta.FindStatusCondition(job.Status.Conditions, nvcrev1alpha1.JobInProgress)
		require.NotNil(t, condition)
		require.Equal(t, metav1.ConditionTrue, condition.Status)
		condition.Reason = "CheckpointReconcileProbe"
		return direct.Status().Update(ctx, job)
	}))
	// Use direct reads so a pre-probe cached status cannot satisfy this wait.
	waitForCondition(t, direct, cfg, deadline)
	settled := &nvcrev1alpha1.Job{}
	require.NoError(t, direct.Get(ctx, o.jobKey, settled))
	require.Eventually(t, func() bool {
		observed := &nvcrev1alpha1.Job{}
		return cached.Get(ctx, o.jobKey, observed) == nil && observed.ResourceVersion == settled.ResourceVersion
	}, boundedWaitTimeout(t, 10*time.Second, deadline), 10*time.Millisecond,
		"cache did not observe the post-probe Job status before golden collection")
	current := &trainerv1alpha1.TrainJob{}
	require.NoError(t, direct.Get(ctx, o.workloadKey, current))
	require.Equal(t, replacementUID, current.UID, "unexpected additional workload replacement")
}
