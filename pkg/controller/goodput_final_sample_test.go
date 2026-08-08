// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// handleRunning throttles reads so that status updates do not re-trigger one.
// The final read before a measurement goes terminal happens moments after the
// previous sample, so without clearing the throttle it would be skipped and the
// last window of output lost. This checks the throttle really is bypassed.
func TestFinalSampleBypassesTheThrottle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, burninv1alpha1.AddToScheme(scheme))

	m := &burninv1alpha1.GoodputMeasurement{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: burninv1alpha1.GoodputMeasurementSpec{
			JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
			LogProfileRef: "does-not-resolve",
		},
	}
	job := &burninv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(m, job).WithStatusSubresource(m, job).Build()
	r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}

	key := "ns/m"
	r.mu.Lock()
	r.lastSample = map[string]time.Time{key: time.Now()}
	r.mu.Unlock()

	r.finalSample(context.Background(), m, job)

	r.mu.Lock()
	_, stillThrottled := r.lastSample[key]
	r.mu.Unlock()

	require.False(t, stillThrottled,
		"finalSample must clear the sample time so the last read is not skipped")
}

// A sample taken moments ago would otherwise block the read.
func TestHandleRunningIsThrottledWithoutFinalSample(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, burninv1alpha1.AddToScheme(scheme))

	m := &burninv1alpha1.GoodputMeasurement{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
		Spec: burninv1alpha1.GoodputMeasurementSpec{
			JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
			LogProfileRef: "does-not-resolve",
		},
	}
	job := &burninv1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"}}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(m, job).WithStatusSubresource(m, job).Build()
	r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}

	key := "ns/m"
	r.mu.Lock()
	r.lastSample = map[string]time.Time{key: time.Now()}
	r.mu.Unlock()

	// Straight into handleRunning, no finalSample: the throttle holds, and the
	// requeue it asks for is the remaining interval.
	res, err := r.handleRunning(context.Background(), m, job)
	require.NoError(t, err)
	require.Positive(t, res.RequeueAfter, "the read was throttled, as designed")

	r.mu.Lock()
	_, stillThrottled := r.lastSample[key]
	r.mu.Unlock()
	require.True(t, stillThrottled, "the sample time is untouched when throttled")
}
