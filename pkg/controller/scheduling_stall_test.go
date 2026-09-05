// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
)

// Tests for checkSchedulingBlocked (ADR-075): the detector must fire
// WorkloadSchedulingBlocked only when a running-path workload's pods carry
// PodScheduled=False/Unschedulable past the grace window, must relay the
// scheduler's FailedScheduling message, must never fire once
// WorkloadStartTime is set (the workload already ran), and must clear the
// persisted blocked-since marker on recovery.

const schedulingTestNS = "sched-test"

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
		WithIndex(&corev1.Event{}, eventInvolvedNameIndexField, func(obj client.Object) []string {
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
			wantMsgPart: "unschedulable",
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
			name: "WorkloadStartTime set — never blocked even with unschedulable pods",
			job: func() *nvcrev1alpha1.Job {
				j := schedulingJob("job-f", &oldBlockedSince, &grace)
				now := metav1.Now()
				j.Status.WorkloadStartTime = &now
				return j
			}(),
			pods:        []*corev1.Pod{unschedulablePod("p1", "job-f")},
			wantBlocked: false,
		},
		{
			name: "mixed: one running pod, one unschedulable — blocked",
			job:  schedulingJob("job-g", &oldBlockedSince, &grace),
			pods: []*corev1.Pod{
				runningPod("p1", "job-g"),
				unschedulablePod("p2", "job-g"),
			},
			wantBlocked: true,
			wantMsgPart: "unschedulable",
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
