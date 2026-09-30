// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestIsBelowBandwidthThresholdWaitsForComplete(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		reason string
		ready  bool
	}{
		{name: "incomplete", ready: false},
		{name: "complete", ready: true},
		{name: "terminal no data", reason: reasonBandwidthNoData, ready: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conditions := []metav1.Condition(nil)
			if tc.ready || tc.reason != "" {
				conditions = []metav1.Condition{{
					Type:   nvcrev1alpha1.BandwidthMeasurementComplete,
					Status: metav1.ConditionTrue,
					Reason: tc.reason,
				}}
			}
			bm := &nvcrev1alpha1.BandwidthMeasurement{
				Name:      "bm",
				Namespace: "ns",
				Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
					JobRef: corev1.TypedLocalObjectReference{Name: "job"},
				},
				Status: nvcrev1alpha1.BandwidthMeasurementStatus{
					Results: []nvcrev1alpha1.BandwidthResult{{BusBW: "980.1"}},
					Conditions: conditions,
				},
			}
			index := func(obj client.Object) []string {
				return []string{obj.(*nvcrev1alpha1.BandwidthMeasurement).Spec.JobRef.Name}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&nvcrev1alpha1.BandwidthMeasurement{}, measurementJobRefIndexField, index).
				WithObjects(bm).Build()

			r := &WorkflowReconciler{Client: c}
			below, pending, err := r.isBelowBandwidthThreshold(context.Background(), "job", "ns", "value >= 900")
			if err != nil {
				t.Fatal(err)
			}
			if tc.ready {
				if below || pending {
					t.Fatalf("complete measurement returned below=%v pending=%v", below, pending)
				}
			} else if below || !pending {
				t.Fatalf("incomplete measurement returned below=%v pending=%v", below, pending)
			}
		})
	}
}
