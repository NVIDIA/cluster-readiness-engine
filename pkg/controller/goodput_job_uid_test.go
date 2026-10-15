// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// A retried group recreates its Job under the same name with a new UID, and
// the failed attempt's Workflow-owned GoodputMeasurement outlives it (issue
// #389 follow-up, mirroring #418 for BandwidthMeasurement). These cases pin
// that the earlier attempt's measurement is neither judged nor used for stall
// detection, and that ensureGoodputMeasurement replaces it.

const (
	goodputUIDTestNS  = "ns"
	goodputUIDTestJob = "wf-job"
	earlierJobUID     = "uid-earlier"

	goodputUIDThreshold   = "value >= 0.9"
	goodputUIDWorkloadOK  = "WorkloadSucceeded"
	goodputUIDUnannotated = "unannotated measurement is taken at its name"
)

func goodputUIDTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(schedulingTestScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&nvcrev1alpha1.Job{}).
		WithIndex(&nvcrev1alpha1.GoodputMeasurement{}, measurementJobRefIndexField, func(obj client.Object) []string {
			return []string{obj.(*nvcrev1alpha1.GoodputMeasurement).Spec.JobRef.Name}
		}).Build()
}

// goodputOf returns a GoodputMeasurement for goodputUIDTestJob created for
// the Job instance with uid; an empty uid leaves it unannotated, as one
// created before the annotation existed.
func goodputOf(uid string, mutate func(*nvcrev1alpha1.GoodputMeasurement)) *nvcrev1alpha1.GoodputMeasurement {
	gm := &nvcrev1alpha1.GoodputMeasurement{
		Name: goodputUIDTestJob + "-goodput", Namespace: goodputUIDTestNS, UID: "gm-" + types.UID(uid),
		Spec: nvcrev1alpha1.GoodputMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{Kind: kindJob, Name: goodputUIDTestJob},
		},
	}
	if uid != "" {
		gm.Annotations = map[string]string{annotationJobUID: uid}
	}
	if mutate != nil {
		mutate(gm)
	}
	return gm
}

func goodputUIDJob() *nvcrev1alpha1.Job {
	return &nvcrev1alpha1.Job{
		Name: goodputUIDTestJob, Namespace: goodputUIDTestNS, UID: currentJobUID, Generation: 1,
		Spec: nvcrev1alpha1.JobSpec{
			GoodputMeasurement: &nvcrev1alpha1.GoodputMeasurementConfig{},
		},
	}
}

// TestGoodputThresholdIgnoresEarlierJob is the repro from #389: attempt 1's
// Complete measurement with goodput 0.42 must not fail attempt 2 against
// `value >= 0.9`. Attempt 2 waits for its own measurement instead.
func TestGoodputThresholdIgnoresEarlierJob(t *testing.T) {
	complete := func(result string) func(*nvcrev1alpha1.GoodputMeasurement) {
		return func(gm *nvcrev1alpha1.GoodputMeasurement) {
			gm.Status.Result = result
			gm.Status.Conditions = []metav1.Condition{{
				Type: nvcrev1alpha1.GoodputMeasurementComplete, Status: metav1.ConditionTrue,
				Reason: "JobSucceeded", LastTransitionTime: metav1.Now(),
			}}
		}
	}
	tests := []struct {
		name string
		gm   *nvcrev1alpha1.GoodputMeasurement
		// wantRequeue: the threshold is still waiting for a measurement.
		wantRequeue bool
		// wantReason is the ValidationFailed reason; "" means not yet set.
		wantReason string
	}{
		{name: "earlier attempt's measurement is not judged", gm: goodputOf(earlierJobUID, complete("0.42")), wantRequeue: true},
		{name: "this attempt's low goodput fails", gm: goodputOf(currentJobUID, complete("0.42")), wantReason: testReasonThresholdViolated},
		{name: "this attempt's goodput passes", gm: goodputOf(currentJobUID, complete("0.95")), wantReason: reasonThresholdsMet},
		{name: goodputUIDUnannotated, gm: goodputOf("", complete("0.42")), wantReason: testReasonThresholdViolated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			job := goodputUIDJob()
			job.Spec.Thresholds = map[string]string{testMetricGoodputRatio: goodputUIDThreshold}
			now := metav1.Now()
			job.Status.Conditions = []metav1.Condition{{
				Type: nvcrev1alpha1.JobSucceeded, Status: metav1.ConditionTrue,
				Reason: goodputUIDWorkloadOK, LastTransitionTime: now, ObservedGeneration: 1,
			}}
			c := goodputUIDTestClient(t, job, tt.gm)
			r := &JobReconciler{Client: c, Scheme: c.Scheme(), Recorder: events.NewFakeRecorder(10)}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), job))

			require.Equal(t, tt.wantRequeue, r.checkPerformanceThresholds(ctx, job))

			got := &nvcrev1alpha1.Job{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), got))
			cond := meta.FindStatusCondition(got.Status.Conditions, nvcrev1alpha1.JobValidationFailed)
			if tt.wantReason == "" {
				require.Nil(t, cond)
				return
			}
			require.NotNil(t, cond)
			require.Equal(t, tt.wantReason, cond.Reason)
		})
	}
}

// TestCheckStallTimeoutIgnoresEarlierJob: attempt 1's measurement carries a
// last step from hours ago. Read as attempt 2's, that is a training stall the
// moment attempt 2 starts; it must be ignored instead.
func TestCheckStallTimeoutIgnoresEarlierJob(t *testing.T) {
	lastStep := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	stale := func(gm *nvcrev1alpha1.GoodputMeasurement) {
		gm.Status.LastStepTimestamp = &lastStep
		gm.Status.AvgStepTimeSec = "10"
		gm.Status.LogInterval = 1
	}
	tests := []struct {
		name        string
		uid         string
		wantStalled bool
	}{
		{name: "earlier attempt's steps are not this attempt's stall", uid: earlierJobUID, wantStalled: false},
		{name: "this attempt's old step is a stall", uid: currentJobUID, wantStalled: true},
		{name: goodputUIDUnannotated, uid: "", wantStalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c := goodputUIDTestClient(t, goodputOf(tt.uid, stale))
			r := &JobReconciler{Client: c, Scheme: c.Scheme()}
			job := goodputUIDJob()
			multiplier := int32(3)
			job.Spec.StallMultiplier = &multiplier
			start := metav1.NewTime(time.Now().Add(-3 * time.Hour))
			job.Status.WorkloadStartTime = &start

			stalled, _ := r.checkStallTimeout(ctx, job, &start)
			require.Equal(t, tt.wantStalled, stalled)
		})
	}
}

// TestEnsureGoodputMeasurementJobUID pins creation and replacement: a new
// measurement records the Job UID, one of this Job or an unannotated one is
// kept, and one of an earlier Job is deleted, then recreated on the next pass.
func TestEnsureGoodputMeasurementJobUID(t *testing.T) {
	tests := []struct {
		name     string
		existing *nvcrev1alpha1.GoodputMeasurement
		// wantGoneAfterFirst: the first call deleted the existing measurement.
		wantGoneAfterFirst bool
		// wantTerminating: the first call left the existing measurement
		// terminating behind its finalizer.
		wantTerminating bool
		// wantUID is the annotation after the second call.
		wantUID string
	}{
		{name: "created with job uid", wantUID: currentJobUID},
		{name: "this job's measurement kept", existing: goodputOf(currentJobUID, nil), wantUID: currentJobUID},
		{name: "unannotated measurement kept", existing: goodputOf("", nil), wantUID: ""},
		{name: "earlier job's measurement replaced", existing: goodputOf(earlierJobUID, nil),
			wantGoneAfterFirst: true, wantUID: currentJobUID},
		// In production the GoodputMeasurement controller has added its
		// finalizer, so the delete only sets a DeletionTimestamp. No
		// replacement is created until the finalizer clears.
		{name: "earlier job's measurement held by finalizer is not replaced yet",
			existing: goodputOf(earlierJobUID, func(gm *nvcrev1alpha1.GoodputMeasurement) {
				gm.Finalizers = []string{goodputMeasurementFinalizer}
			}),
			wantTerminating: true, wantUID: earlierJobUID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			objs := []client.Object{}
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			c := goodputUIDTestClient(t, objs...)
			r := &JobReconciler{Client: c, Scheme: c.Scheme()}
			job := goodputUIDJob()
			key := client.ObjectKey{Namespace: goodputUIDTestNS, Name: goodputUIDTestJob + "-goodput"}

			require.NoError(t, r.ensureGoodputMeasurement(ctx, job))
			if tt.wantGoneAfterFirst {
				err := c.Get(ctx, key, &nvcrev1alpha1.GoodputMeasurement{})
				require.True(t, apierrors.IsNotFound(err), "stale measurement should be deleted, got %v", err)
			}

			if tt.wantTerminating {
				held := &nvcrev1alpha1.GoodputMeasurement{}
				require.NoError(t, c.Get(ctx, key, held))
				require.False(t, held.DeletionTimestamp.IsZero(), "stale measurement should be terminating")
			}

			require.NoError(t, r.ensureGoodputMeasurement(ctx, job))
			got := &nvcrev1alpha1.GoodputMeasurement{}
			require.NoError(t, c.Get(ctx, key, got))
			uid, ok := got.Annotations[annotationJobUID]
			require.Equal(t, tt.wantUID != "", ok)
			require.Equal(t, tt.wantUID, uid)
		})
	}
}

// TestReconcileWorkloadTerminalJobReplacesEarlierGoodput drives the terminal
// branch of reconcileWorkload, the only place that creates the measurement
// for a Job that is already terminal on entry. Attempt 1's Complete
// measurement (goodput 0.42) carries the controller's finalizer, so the
// first pass only requests its deletion: no replacement yet, and the stale
// value is not judged. Once the finalizer clears, the next pass creates this
// attempt's measurement.
func TestReconcileWorkloadTerminalJobReplacesEarlierGoodput(t *testing.T) {
	ctx := context.Background()
	stale := goodputOf(earlierJobUID, func(gm *nvcrev1alpha1.GoodputMeasurement) {
		gm.Finalizers = []string{goodputMeasurementFinalizer}
		gm.Status.Result = "0.42"
		gm.Status.Conditions = []metav1.Condition{{
			Type: nvcrev1alpha1.GoodputMeasurementComplete, Status: metav1.ConditionTrue,
			Reason: "JobSucceeded", LastTransitionTime: metav1.Now(),
		}}
	})
	job := goodputUIDJob()
	job.Spec.Thresholds = map[string]string{testMetricGoodputRatio: goodputUIDThreshold}
	job.Status.Conditions = []metav1.Condition{{
		Type: nvcrev1alpha1.JobSucceeded, Status: metav1.ConditionTrue,
		Reason: goodputUIDWorkloadOK, LastTransitionTime: metav1.Now(), ObservedGeneration: 1,
	}}
	c := goodputUIDTestClient(t, job, stale)
	r := &JobReconciler{Client: c, Scheme: c.Scheme(), Recorder: events.NewFakeRecorder(10)}
	key := client.ObjectKeyFromObject(stale)

	pass := func() {
		t.Helper()
		got := &nvcrev1alpha1.Job{}
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), got))
		res, err := r.reconcileWorkload(ctx, got)
		require.NoError(t, err)
		require.NotZero(t, res.RequeueAfter, "thresholds should wait for this attempt's measurement")
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), got))
		require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, nvcrev1alpha1.JobValidationFailed),
			"the earlier attempt's goodput must not be judged")
	}

	// Pass 1: delete requested, measurement held by its finalizer.
	pass()
	held := &nvcrev1alpha1.GoodputMeasurement{}
	require.NoError(t, c.Get(ctx, key, held))
	require.False(t, held.DeletionTimestamp.IsZero(), "stale measurement should be terminating")
	require.Equal(t, earlierJobUID, held.Annotations[annotationJobUID])

	// Pass 2 while still held: still no replacement.
	pass()
	require.NoError(t, c.Get(ctx, key, held))
	require.Equal(t, earlierJobUID, held.Annotations[annotationJobUID])

	// The GoodputMeasurement controller drops its finalizer; the object goes.
	held.Finalizers = nil
	require.NoError(t, c.Update(ctx, held))
	require.True(t, apierrors.IsNotFound(c.Get(ctx, key, &nvcrev1alpha1.GoodputMeasurement{})))

	// Pass 3: this attempt's measurement is created.
	pass()
	got := &nvcrev1alpha1.GoodputMeasurement{}
	require.NoError(t, c.Get(ctx, key, got))
	require.Equal(t, currentJobUID, got.Annotations[annotationJobUID])
}

// TestHasCheckpointIgnoresEarlierJob: attempt 1's measurement, still
// terminating behind its finalizer, recorded checkpoints. They must not let
// attempt 2 restart from a checkpoint it never wrote.
func TestHasCheckpointIgnoresEarlierJob(t *testing.T) {
	tests := []struct {
		name string
		uid  string
		want bool
	}{
		{name: "earlier attempt's checkpoint ignored", uid: earlierJobUID, want: false},
		{name: "this attempt's checkpoint counts", uid: currentJobUID, want: true},
		{name: goodputUIDUnannotated, uid: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := goodputUIDTestClient(t, goodputOf(tt.uid, func(gm *nvcrev1alpha1.GoodputMeasurement) {
				gm.Status.LastCheckpointStep = 100
			}))
			r := &JobReconciler{Client: c, Scheme: c.Scheme()}
			require.Equal(t, tt.want, r.hasCheckpoint(context.Background(), goodputUIDJob()))
		})
	}
}
