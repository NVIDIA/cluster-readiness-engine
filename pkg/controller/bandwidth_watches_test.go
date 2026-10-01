// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func TestJobPhaseChangePredicate(t *testing.T) {
	job := func(workload string, trueConds ...string) *nvcrev1alpha1.Job {
		j := &nvcrev1alpha1.Job{Name: "j", Namespace: "ns"}
		for _, c := range trueConds {
			j.Status.Conditions = append(j.Status.Conditions, metav1.Condition{Type: c, Status: metav1.ConditionTrue})
		}
		if workload != "" {
			j.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{Kind: "TrainJob", Name: workload}
		}
		return j
	}

	tests := []struct {
		name     string
		old, new *nvcrev1alpha1.Job
		want     bool
	}{
		{"unchanged phase and workload", job("w", nvcrev1alpha1.JobInProgress), job("w", nvcrev1alpha1.JobInProgress), false},
		{"running to succeeded", job("w", nvcrev1alpha1.JobInProgress), job("w", nvcrev1alpha1.JobSucceeded), true},
		{"running to failed", job("w", nvcrev1alpha1.JobInProgress), job("w", nvcrev1alpha1.JobFailed), true},
		{"pending to running", job("w"), job("w", nvcrev1alpha1.JobInProgress), true},
		{"workload assigned", job("", nvcrev1alpha1.JobInProgress), job("w", nvcrev1alpha1.JobInProgress), true},
		{"workload replaced", job("w1", nvcrev1alpha1.JobInProgress), job("w2", nvcrev1alpha1.JobInProgress), true},
	}
	p := jobPhaseChangePredicate()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
	require.True(t, p.Delete(event.DeleteEvent{Object: job("w")}), "a deleted Job completes its measurement")
	require.False(t, p.Create(event.CreateEvent{Object: job("w")}), "a new Job has no measurement to wake")
}

func TestBandwidthCompletedPredicate(t *testing.T) {
	bm := func(complete bool, results int) *nvcrev1alpha1.BandwidthMeasurement {
		m := &nvcrev1alpha1.BandwidthMeasurement{Name: "m", Namespace: "ns"}
		if complete {
			m.Status.Conditions = []metav1.Condition{{
				Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue,
			}}
		}
		for range results {
			m.Status.Results = append(m.Status.Results, nvcrev1alpha1.BandwidthResult{})
		}
		return m
	}

	tests := []struct {
		name     string
		old, new *nvcrev1alpha1.BandwidthMeasurement
		want     bool
	}{
		{"a live sample", bm(false, 1), bm(false, 2), false},
		{"completion", bm(false, 2), bm(true, 3), true},
		{"already complete", bm(true, 3), bm(true, 3), false},
	}
	p := bandwidthCompletedPredicate()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
}
