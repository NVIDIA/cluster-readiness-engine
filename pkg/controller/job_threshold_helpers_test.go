// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// testMetricBusBandwidth is the bus bandwidth metric/threshold key used
// throughout this file's fixtures.
const testMetricBusBandwidth = "busBandwidthGBps"

func TestMissingJobThresholdKeys(t *testing.T) {
	t.Parallel()

	missing := missingJobThresholdKeys(
		map[string]string{testMetricBusBandwidth: "value >= 900", testMetricGoodputRatio: "value >= 0.9"},
		map[string]float64{testMetricBusBandwidth: 1000},
	)
	if len(missing) != 1 || missing[0] != testMetricGoodputRatio {
		t.Fatalf("missing = %v, want [goodputRatio]", missing)
	}
}

func TestIsJobAwaitingThresholdEvaluation(t *testing.T) {
	t.Parallel()

	job := &nvcrev1alpha1.Job{
		Name: "job", Namespace: testNS,
		Spec: nvcrev1alpha1.JobSpec{
			Thresholds: map[string]string{testMetricBusBandwidth: "value >= 900"},
		},
		Status: nvcrev1alpha1.JobStatus{
			Conditions: []metav1.Condition{{
				Type:   nvcrev1alpha1.JobSucceeded,
				Status: metav1.ConditionTrue,
			}},
		},
	}

	if !isJobAwaitingThresholdEvaluation(job) {
		t.Fatal("expected awaiting when ValidationFailed condition is unset")
	}

	job.Status.Conditions = append(job.Status.Conditions, metav1.Condition{
		Type:   nvcrev1alpha1.JobValidationFailed,
		Status: metav1.ConditionFalse,
	})
	if isJobAwaitingThresholdEvaluation(job) {
		t.Fatal("expected not awaiting when ValidationFailed=False (pass recorded)")
	}

	job.Status.Conditions[len(job.Status.Conditions)-1].Status = metav1.ConditionTrue
	if isJobAwaitingThresholdEvaluation(job) {
		t.Fatal("expected not awaiting when ValidationFailed=True")
	}
}
