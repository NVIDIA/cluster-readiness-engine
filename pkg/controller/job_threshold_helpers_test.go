// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

func TestMissingJobThresholdKeys(t *testing.T) {
	t.Parallel()

	missing := missingJobThresholdKeys(
		map[string]string{"busBandwidthGBps": "value >= 900", "goodputRatio": "value >= 0.9"},
		map[string]float64{"busBandwidthGBps": 1000},
	)
	if len(missing) != 1 || missing[0] != "goodputRatio" {
		t.Fatalf("missing = %v, want [goodputRatio]", missing)
	}
}

func TestIsJobAwaitingThresholdEvaluation(t *testing.T) {
	t.Parallel()

	job := &crev1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default"},
		Spec: crev1alpha1.JobSpec{
			Thresholds: map[string]string{"busBandwidthGBps": "value >= 900"},
		},
		Status: crev1alpha1.JobStatus{
			Conditions: []metav1.Condition{{
				Type:   crev1alpha1.JobSucceeded,
				Status: metav1.ConditionTrue,
			}},
		},
	}

	if !isJobAwaitingThresholdEvaluation(job) {
		t.Fatal("expected awaiting when ValidationFailed condition is unset")
	}

	job.Status.Conditions = append(job.Status.Conditions, metav1.Condition{
		Type:   crev1alpha1.JobValidationFailed,
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
