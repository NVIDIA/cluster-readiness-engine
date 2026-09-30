// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// The diagnose gate evaluates a group's bandwidth only from a final
// measurement of that very Job: anything else waits or fails closed.
func TestIsBelowBandwidthThreshold(t *testing.T) {
	bm := func(jobUID, reason, busBW string) *nvcrev1alpha1.BandwidthMeasurement {
		m := &nvcrev1alpha1.BandwidthMeasurement{
			Name: "j-bandwidth", Namespace: "ns",
			Spec: nvcrev1alpha1.BandwidthMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: "j"}},
		}
		if jobUID != "" {
			m.Annotations = map[string]string{annotationJobUID: jobUID}
		}
		if reason != "" {
			m.Status.Conditions = []metav1.Condition{{
				Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue,
				Reason: reason, LastTransitionTime: metav1.Now(),
			}}
		}
		if busBW != "" {
			m.Status.Results = []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1 << 30, AlgBW: "1.00", BusBW: busBW, Samples: 1}}
		}
		return m
	}

	tests := []struct {
		name  string
		noJob bool
		// jobOnlyLive leaves the Job out of the cache but on the API server.
		jobOnlyLive bool
		// liveErr misses the cache and fails the API server read.
		liveErr     bool
		measurement *nvcrev1alpha1.BandwidthMeasurement
		wantBelow   bool
		wantPending bool
		wantErr     bool
	}{
		{name: "no measurement yet", wantPending: true},
		{name: "provisional results", measurement: bm(currentJobUID, "", "900.00"), wantPending: true},
		{name: "measurement of an earlier Job with the same name", measurement: bm("uid-earlier", reasonBandwidthJobSucceeded, "900.00"), wantPending: true},
		{name: "Job confirmed deleted", noJob: true, measurement: bm(currentJobUID, reasonBandwidthJobSucceeded, "900.00"), wantErr: true},
		{name: "Job not yet in cache", jobOnlyLive: true, measurement: bm(currentJobUID, reasonBandwidthJobSucceeded, "900.00")},
		{name: "Job unreadable on the API server", liveErr: true, measurement: bm(currentJobUID, reasonBandwidthJobSucceeded, "100.00"), wantPending: true},
		{name: "completed without final results", measurement: bm(currentJobUID, reasonBandwidthLogsUnavailable, "900.00"), wantErr: true},
		{name: "completed with no data", measurement: bm(currentJobUID, reasonBandwidthNoData, ""), wantErr: true},
		{name: "final and above threshold", measurement: bm(currentJobUID, reasonBandwidthJobSucceeded, "900.00")},
		{name: "final and below threshold", measurement: bm(currentJobUID, reasonBandwidthJobSucceeded, "100.00"), wantBelow: true},
		{name: "legacy measurement without annotation", measurement: bm("", reasonBandwidthJobSucceeded, "100.00"), wantBelow: true},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &nvcrev1alpha1.Job{Name: "j", Namespace: "ns", UID: currentJobUID}
			var objs []client.Object
			if !tt.noJob && !tt.jobOnlyLive && !tt.liveErr {
				objs = append(objs, job)
			}
			if tt.measurement != nil {
				objs = append(objs, tt.measurement)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&nvcrev1alpha1.BandwidthMeasurement{}, measurementJobRefIndexField, func(obj client.Object) []string {
					return []string{obj.(*nvcrev1alpha1.BandwidthMeasurement).Spec.JobRef.Name}
				}).
				WithObjects(objs...).Build()
			var live client.Reader = c
			if tt.jobOnlyLive {
				live = fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
			}
			if tt.liveErr {
				live = failingReader{}
			}

			r := &WorkflowReconciler{Client: c, APIReader: live, Scheme: scheme}
			below, pending, err := r.isBelowBandwidthThreshold(context.Background(), "j", "ns", "value >= 400")
			require.Equal(t, tt.wantErr, err != nil, "err: %v", err)
			require.Equal(t, tt.wantPending, pending, "pending")
			require.Equal(t, tt.wantBelow, below, "below")
		})
	}
}

// failingReader fails every read, as an unreachable API server does.
type failingReader struct{ client.Reader }

func (failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("connection refused")
}
