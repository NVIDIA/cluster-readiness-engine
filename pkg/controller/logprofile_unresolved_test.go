// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func measurementScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, burninv1alpha1.AddToScheme(scheme))
	return scheme
}

func jobWithCondition(condType string) *burninv1alpha1.Job {
	return &burninv1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
		Status: burninv1alpha1.JobStatus{
			Conditions: []metav1.Condition{{
				Type:               condType,
				Status:             metav1.ConditionTrue,
				Reason:             "Test",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// A spec.logProfileRef that does not resolve must be reported on the
// BandwidthMeasurement rather than swallowed. Before this was fixed the
// measurement carried no conditions while the Job ran, then ended as
// Complete=True/JobSucceeded with zero results, which reads as a successful
// measurement.
func TestBandwidthUnresolvedLogProfileIsReported(t *testing.T) {
	t.Run("while the Job runs, the cause is on the status", func(t *testing.T) {
		ctx := context.Background()
		scheme := measurementScheme(t)

		m := &burninv1alpha1.BandwidthMeasurement{
			ObjectMeta: metav1.ObjectMeta{
				Name: "m", Namespace: "ns",
				Finalizers: []string{bandwidthMeasurementFinalizer},
			},
			Spec: burninv1alpha1.BandwidthMeasurementSpec{
				JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
				LogProfileRef: "typo-does-not-exist",
			},
		}
		job := jobWithCondition(burninv1alpha1.JobInProgress)

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(job, m).WithStatusSubresource(job, m).Build()
		r := &BandwidthMeasurementReconciler{Client: c, Scheme: scheme, RequeueInterval: time.Second}

		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "m", Namespace: "ns"}})
		require.NoError(t, err)

		got := &burninv1alpha1.BandwidthMeasurement{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "m", Namespace: "ns"}, got))

		cond := meta.FindStatusCondition(got.Status.Conditions, burninv1alpha1.BandwidthMeasurementMeasuring)
		require.NotNil(t, cond, "an unresolved LogProfile must be visible in status")
		require.Equal(t, metav1.ConditionFalse, cond.Status)
		require.Equal(t, reasonBandwidthLogProfileMissing, cond.Reason)
		require.Contains(t, cond.Message, "typo-does-not-exist")

		// Not terminal: the LogProfile may still be created.
		require.Nil(t, meta.FindStatusCondition(got.Status.Conditions, burninv1alpha1.BandwidthMeasurementComplete))
	})

	t.Run("a Job that succeeds with no data is not reported as JobSucceeded", func(t *testing.T) {
		ctx := context.Background()
		scheme := measurementScheme(t)

		m := &burninv1alpha1.BandwidthMeasurement{
			ObjectMeta: metav1.ObjectMeta{
				Name: "m", Namespace: "ns",
				Finalizers: []string{bandwidthMeasurementFinalizer},
			},
			Spec: burninv1alpha1.BandwidthMeasurementSpec{
				JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
				LogProfileRef: "typo-does-not-exist",
			},
		}
		job := jobWithCondition(burninv1alpha1.JobSucceeded)

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(job, m).WithStatusSubresource(job, m).Build()
		r := &BandwidthMeasurementReconciler{Client: c, Scheme: scheme, RequeueInterval: time.Second}

		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "m", Namespace: "ns"}})
		require.NoError(t, err)

		got := &burninv1alpha1.BandwidthMeasurement{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "m", Namespace: "ns"}, got))

		require.Empty(t, got.Status.Results, "guards the premise of this test")
		complete := meta.FindStatusCondition(got.Status.Conditions, burninv1alpha1.BandwidthMeasurementComplete)
		require.NotNil(t, complete)
		require.Equal(t, reasonBandwidthNoData, complete.Reason,
			"a measurement that parsed nothing must not claim JobSucceeded")
	})

	t.Run("a Job that succeeds with data still reports JobSucceeded", func(t *testing.T) {
		ctx := context.Background()
		scheme := measurementScheme(t)

		m := &burninv1alpha1.BandwidthMeasurement{
			ObjectMeta: metav1.ObjectMeta{
				Name: "m", Namespace: "ns",
				Finalizers: []string{bandwidthMeasurementFinalizer},
			},
			Spec: burninv1alpha1.BandwidthMeasurementSpec{
				JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
				LogProfileRef: "nccl",
			},
			Status: burninv1alpha1.BandwidthMeasurementStatus{
				Results: []burninv1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "10", BusBW: "20"}},
			},
		}
		job := jobWithCondition(burninv1alpha1.JobSucceeded)

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(job, m).WithStatusSubresource(job, m).Build()
		r := &BandwidthMeasurementReconciler{Client: c, Scheme: scheme, RequeueInterval: time.Second}

		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "m", Namespace: "ns"}})
		require.NoError(t, err)

		got := &burninv1alpha1.BandwidthMeasurement{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "m", Namespace: "ns"}, got))

		complete := meta.FindStatusCondition(got.Status.Conditions, burninv1alpha1.BandwidthMeasurementComplete)
		require.NotNil(t, complete)
		require.Equal(t, reasonBandwidthJobSucceeded, complete.Reason,
			"a measurement with results is unaffected")
	})
}

// The GoodputMeasurement controller has the same shape.
func TestGoodputUnresolvedLogProfileIsReported(t *testing.T) {
	ctx := context.Background()
	scheme := measurementScheme(t)

	m := &burninv1alpha1.GoodputMeasurement{
		ObjectMeta: metav1.ObjectMeta{
			Name: "m", Namespace: "ns",
			Finalizers: []string{goodputMeasurementFinalizer},
		},
		Spec: burninv1alpha1.GoodputMeasurementSpec{
			JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
			LogProfileRef: "typo-does-not-exist",
		},
	}
	job := jobWithCondition(burninv1alpha1.JobInProgress)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job, m).WithStatusSubresource(job, m).Build()
	r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m", Namespace: "ns"}})
	require.NoError(t, err)

	got := &burninv1alpha1.GoodputMeasurement{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "m", Namespace: "ns"}, got))

	cond := meta.FindStatusCondition(got.Status.Conditions, burninv1alpha1.GoodputMeasurementMeasuring)
	require.NotNil(t, cond, "an unresolved LogProfile must be visible in status")
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, reasonGoodputLogProfileMissing, cond.Reason)
	require.Contains(t, cond.Message, "typo-does-not-exist")
}
