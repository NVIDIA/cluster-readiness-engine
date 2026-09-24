// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
)

// Tests for checkSchedulingBlocked (ADR-083): the detector must fire
// WorkloadSchedulingBlocked when a running-path workload's pods carry
// PodScheduled=False/Unschedulable past the grace window — including after
// WorkloadStartTime is set (clock-pause amendment) — must relay the
// scheduler's FailedScheduling message, and must clear the persisted
// blocked-since marker when any pod schedules.

const schedulingTestNS = "sched-test"

const testUnschedulableMsg = "unschedulable"

func schedulingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newSchedulingFakeClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&nvcrev1alpha1.Job{}).
		WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return nil
			}
			if jn, found := pod.Labels[nodemonitor.NVCREJobLabel]; found {
				return []string{jn}
			}
			return nil
		}).
		WithIndex(&corev1.Event{}, eventInvolvedNameField, func(obj client.Object) []string {
			ev, ok := obj.(*corev1.Event)
			if !ok || ev.InvolvedObject.Name == "" {
				return nil
			}
			return []string{ev.InvolvedObject.Name}
		}).
		Build()
}

func unschedulablePod(name, jobName string) *corev1.Pod {
	meta := metav1.ObjectMeta{
		Name: name, Namespace: schedulingTestNS,
		Labels: map[string]string{nodemonitor.NVCREJobLabel: jobName},
	}
	return &corev1.Pod{
		ObjectMeta: meta,
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodScheduled,
					Status: corev1.ConditionFalse,
					Reason: "Unschedulable",
				},
			},
		},
	}
}

func failedSchedulingEvent(name string, message string, at time.Time) *corev1.Event {
	meta := metav1.ObjectMeta{
		Name: name + "-evt-" + messageNameSuffix(message), Namespace: schedulingTestNS,
	}
	return &corev1.Event{
		ObjectMeta:     meta,
		InvolvedObject: corev1.ObjectReference{Name: name, Namespace: schedulingTestNS},
		Reason:         "FailedScheduling",
		Message:        message,
		LastTimestamp:  metav1.Time{Time: messageTime(at)},
	}
}

// messageNameSuffix derives a stable unique suffix from the message text so
// multiple events for the same pod never collide on the fake client's name
// uniqueness check.
func messageNameSuffix(message string) string {
	return strings.ReplaceAll(strings.ToLower(message[:min(len(message), 12)]), " ", "-")
}

// messageTime falls back to a minute ago when no explicit timestamp is given,
// so "newest wins" is observable in tests that register two events.
func messageTime(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().Add(-time.Minute)
	}
	return at
}

func runningPod(name, jobName string) *corev1.Pod {
	p := unschedulablePod(name, jobName)
	p.Spec.NodeName = "some-node"
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
	}
	return p
}

func schedulingJob(name string, blockedSince *metav1.Time, grace *int32) *nvcrev1alpha1.Job {
	meta := metav1.ObjectMeta{Name: name, Namespace: schedulingTestNS}
	return &nvcrev1alpha1.Job{
		ObjectMeta: meta,
		Spec: nvcrev1alpha1.JobSpec{
			SchedulingStallGraceSeconds: grace,
		},
		Status: nvcrev1alpha1.JobStatus{
			SchedulingBlockedSince: blockedSince,
		},
	}
}

func TestCheckSchedulingBlocked(t *testing.T) {
	ctx := context.Background()

	grace := int32(300)
	oldBlockedSince := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	recentBlockedSince := metav1.NewTime(time.Now().Add(-30 * time.Second))

	tests := []struct {
		name        string
		job         *nvcrev1alpha1.Job
		pods        []*corev1.Pod
		events      []*corev1.Event
		wantBlocked bool
		wantMsgPart string
	}{
		{
			name:        "no pods at all — not blocked",
			job:         schedulingJob("job-a", nil, &grace),
			pods:        nil,
			wantBlocked: false,
		},
		{
			name:        "pod running on a node — not blocked",
			job:         schedulingJob("job-b", nil, &grace),
			pods:        []*corev1.Pod{runningPod("p1", "job-b")},
			wantBlocked: false,
		},
		{
			name:        "unschedulable pod within grace window — not blocked yet",
			job:         schedulingJob("job-c", &recentBlockedSince, &grace),
			pods:        []*corev1.Pod{unschedulablePod("p1", "job-c")},
			wantBlocked: false,
		},
		{
			name:        "unschedulable pod past grace — blocked with generic message",
			job:         schedulingJob("job-d", &oldBlockedSince, &grace),
			pods:        []*corev1.Pod{unschedulablePod("p1", "job-d")},
			wantBlocked: true,
			wantMsgPart: testUnschedulableMsg,
		},
		{
			name: "unschedulable pod with FailedScheduling event relays scheduler message",
			job:  schedulingJob("job-e", &oldBlockedSince, &grace),
			pods: []*corev1.Pod{unschedulablePod("p1", "job-e")},
			events: []*corev1.Event{
				failedSchedulingEvent("p1", "0/3 nodes are available: 1 Insufficient nvidia.com/gpu.", time.Time{}),
			},
			wantBlocked: true,
			wantMsgPart: "0/3 nodes are available: 1 Insufficient nvidia.com/gpu.",
		},
		{
			name: "WorkloadStartTime set — blocked still fires (clock-pause amendment)",
			job: func() *nvcrev1alpha1.Job {
				j := schedulingJob("job-f", &oldBlockedSince, &grace)
				now := metav1.Now()
				j.Status.WorkloadStartTime = &now
				return j
			}(),
			pods:        []*corev1.Pod{unschedulablePod("p1", "job-f")},
			wantBlocked: true,
			wantMsgPart: testUnschedulableMsg,
		},
		{
			name: "mixed: one running pod, one unschedulable — blocked",
			job:  schedulingJob("job-g", &oldBlockedSince, &grace),
			pods: []*corev1.Pod{
				runningPod("p1", "job-g"),
				unschedulablePod("p2", "job-g"),
			},
			wantBlocked: true,
			wantMsgPart: testUnschedulableMsg,
		},
		{
			name: "SchedulingGated pod past grace — not blocked (intentional hold, not a rejection)",
			job:  schedulingJob("job-i", &oldBlockedSince, &grace),
			pods: func() []*corev1.Pod {
				p := unschedulablePod("p1", "job-i")
				p.Status.Conditions[0].Reason = corev1.PodReasonSchedulingGated
				return []*corev1.Pod{p}
			}(),
			wantBlocked: false,
		},
		{
			name: "unscheduled but PodScheduled=True — not blocked",
			job:  schedulingJob("job-h", &oldBlockedSince, &grace),
			pods: func() []*corev1.Pod {
				p := unschedulablePod("p1", "job-h")
				p.Status.Conditions = []corev1.PodCondition{
					{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				}
				return []*corev1.Pod{p}
			}(),
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := schedulingTestScheme(t)
			objs := []client.Object{tc.job.DeepCopy()}
			for _, p := range tc.pods {
				objs = append(objs, p.DeepCopy())
			}
			for _, e := range tc.events {
				objs = append(objs, e.DeepCopy())
			}
			c := newSchedulingFakeClient(t, scheme, objs...)
			r := &JobReconciler{Client: c, Scheme: scheme}

			gotBlocked, msg := r.checkSchedulingBlocked(ctx, tc.job)
			if gotBlocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v (msg=%q)", gotBlocked, tc.wantBlocked, msg)
			}
			if tc.wantBlocked && tc.wantMsgPart != "" && !contains(msg, tc.wantMsgPart) {
				t.Fatalf("message %q does not contain %q", msg, tc.wantMsgPart)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestNewestFailedSchedulingMessage(t *testing.T) {
	ctx := context.Background()

	scheme := schedulingTestScheme(t)
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-5 * time.Minute)

	pod := unschedulablePod("p1", "job-x")
	job := schedulingJob("job-x", nil, nil)

	c := newSchedulingFakeClient(t, scheme,
		pod.DeepCopy(), job.DeepCopy(),
		failedSchedulingEvent("p1", "older message", old),
		failedSchedulingEvent("p1", "newer message wins", recent),
	)

	got := newestFailedSchedulingMessage(ctx, c, pod)
	if got != "newer message wins" {
		t.Fatalf("newestFailedSchedulingMessage = %q, want %q", got, "newer message wins")
	}
}

func TestNewestFailedSchedulingMessageNoEvents(t *testing.T) {
	ctx := context.Background()

	scheme := schedulingTestScheme(t)
	pod := unschedulablePod("p1", "job-y")
	job := schedulingJob("job-y", nil, nil)

	c := newSchedulingFakeClient(t, scheme, pod.DeepCopy(), job.DeepCopy())

	got := newestFailedSchedulingMessage(ctx, c, pod)
	if got != "" {
		t.Fatalf("expected empty message with no events, got %q", got)
	}
}

// A workload that ran 49 minutes, then sat unschedulable for 10, resumes with
// exactly 49 minutes consumed: WorkloadStartTime moves forward by the paused
// interval instead of being reset to the recovery instant.
func TestResumeFromSchedulingBlockPreservesConsumedRuntime(t *testing.T) {
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	start := metav1.NewTime(now.Add(-59 * time.Minute))
	blockedSince := metav1.NewTime(now.Add(-10 * time.Minute))

	j := schedulingJob("job-r", &blockedSince, nil)
	j.Status.WorkloadStartTime = &start
	resumeFromSchedulingBlock(j, now)

	require.Nil(t, j.Status.SchedulingBlockedSince)
	require.NotNil(t, j.Status.SchedulingResumedTime)
	require.True(t, j.Status.SchedulingResumedTime.Equal(&now))
	require.Equal(t, 49*time.Minute, now.Sub(j.Status.WorkloadStartTime.Time))
}

// A workload blocked before its clock ever started keeps the clock unset, so
// the first-observe logic stamps it when the workload actually runs.
func TestResumeFromSchedulingBlockLeavesUnstartedClockUnset(t *testing.T) {
	now := metav1.Now()
	blockedSince := metav1.NewTime(now.Add(-10 * time.Minute))

	j := schedulingJob("job-s", &blockedSince, nil)
	resumeFromSchedulingBlock(j, now)

	require.Nil(t, j.Status.WorkloadStartTime)
	require.Nil(t, j.Status.SchedulingBlockedSince)
}

// End to end through the detector: once no pod is blocked, the marker is
// cleared and the shifted start and resume time are persisted.
func TestCheckSchedulingBlockedRecoveryShiftsClock(t *testing.T) {
	ctx := context.Background()
	scheme := schedulingTestScheme(t)

	start := metav1.NewTime(time.Now().Add(-30 * time.Minute).Truncate(time.Second))
	blockedSince := metav1.NewTime(time.Now().Add(-20 * time.Minute).Truncate(time.Second))
	job := schedulingJob("job-t", &blockedSince, nil)
	job.Status.WorkloadStartTime = &start

	c := newSchedulingFakeClient(t, scheme, job.DeepCopy(), runningPod("p1", "job-t"))
	r := &JobReconciler{Client: c, Scheme: scheme}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), job))

	blocked, _ := r.checkSchedulingBlocked(ctx, job)
	require.False(t, blocked)

	persisted := &nvcrev1alpha1.Job{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), persisted))
	require.Nil(t, persisted.Status.SchedulingBlockedSince)
	require.NotNil(t, persisted.Status.SchedulingResumedTime)
	// About 10 minutes consumed before the block; the ~20 blocked minutes
	// are not charged.
	consumed := time.Since(persisted.Status.WorkloadStartTime.Time)
	require.InDelta(t, (10 * time.Minute).Seconds(), consumed.Seconds(), 5)
}

// The reviewer's reproduction: started 2m ago, blocked since 1m ago, default
// 5m grace, 90s timeoutPerJob. The clock is paused at the first blocked
// observation, so the Workflow must not time the Job out during grace.
func TestIsJobTimedOutPausedWhileSchedulingBlocked(t *testing.T) {
	r := &WorkflowReconciler{}
	wf := &nvcrev1alpha1.Workflow{}
	wf.Spec.Orchestration.Execution.TimeoutPerJob = &metav1.Duration{Duration: 90 * time.Second}

	start := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	blockedSince := metav1.NewTime(time.Now().Add(-time.Minute))
	job := &nvcrev1alpha1.Job{Status: nvcrev1alpha1.JobStatus{
		WorkloadStartTime:      &start,
		SchedulingBlockedSince: &blockedSince,
	}}
	require.False(t, r.isJobTimedOut(wf, &nvcrev1alpha1.GroupStatus{}, job),
		"only 60s were consumed before the block")

	// Consumed runtime before the block still counts.
	early := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	job.Status.WorkloadStartTime = &early
	require.True(t, r.isJobTimedOut(wf, &nvcrev1alpha1.GroupStatus{}, job),
		"4m consumed before the block exceeds the 90s budget")

	// Without a block the same start time times out.
	job.Status.WorkloadStartTime = &start
	job.Status.SchedulingBlockedSince = nil
	require.True(t, r.isJobTimedOut(wf, &nvcrev1alpha1.GroupStatus{}, job))
}

// A training workload whose last step predates a long scheduling block must
// not be declared stalled the moment it recovers: the training-stall budget
// restarts from schedulingResumedTime.
func TestCheckStallTimeoutCreditsSchedulingBlock(t *testing.T) {
	ctx := context.Background()
	scheme := schedulingTestScheme(t)

	multiplier := int32(3)
	lastStep := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	gmMeta := metav1.ObjectMeta{Name: "gm-u", Namespace: schedulingTestNS}
	gm := &nvcrev1alpha1.GoodputMeasurement{
		ObjectMeta: gmMeta,
		Spec: nvcrev1alpha1.GoodputMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{Kind: "Job", Name: "job-u"},
		},
		Status: nvcrev1alpha1.GoodputMeasurementStatus{
			LastStepTimestamp: &lastStep,
			AvgStepTimeSec:    "10",
			LogInterval:       1,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gm).
		WithIndex(&nvcrev1alpha1.GoodputMeasurement{}, measurementJobRefIndexField, func(obj client.Object) []string {
			return []string{obj.(*nvcrev1alpha1.GoodputMeasurement).Spec.JobRef.Name}
		}).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	job := schedulingJob("job-u", nil, nil)
	job.Spec.StallMultiplier = &multiplier
	start := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	job.Status.WorkloadStartTime = &start

	stalled, _ := r.checkStallTimeout(ctx, job, &start)
	require.True(t, stalled, "no block recorded: a 2h-old step is a stall")

	resumed := metav1.NewTime(time.Now().Add(-10 * time.Second))
	job.Status.SchedulingResumedTime = &resumed
	stalled, _ = r.checkStallTimeout(ctx, job, &start)
	require.False(t, stalled, "the budget restarts at recovery")
}

func TestSchedulingBlockedMessage(t *testing.T) {
	meta := metav1.ObjectMeta{Name: "job-v"}
	job := &nvcrev1alpha1.Job{ObjectMeta: meta}
	require.Empty(t, schedulingBlockedMessage(job))

	job.Status.Conditions = []metav1.Condition{{
		Type:    nvcrev1alpha1.JobInProgress,
		Status:  metav1.ConditionTrue,
		Reason:  ReasonWorkloadSchedulingBlocked,
		Message: "Workload pods are unschedulable: 0/3 nodes are available",
	}}
	require.Equal(t,
		"job job-v scheduling blocked: Workload pods are unschedulable: 0/3 nodes are available",
		schedulingBlockedMessage(job))

	job.Status.Conditions[0].Reason = ReasonWorkloadRunning
	require.Empty(t, schedulingBlockedMessage(job))
}
