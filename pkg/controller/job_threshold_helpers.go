// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// missingJobThresholdKeys returns threshold keys that have no corresponding measured value.
func missingJobThresholdKeys(thresholds map[string]string, measured map[string]float64) []string {
	var missing []string
	for key := range thresholds {
		if _, ok := measured[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

// findJobGoodputMeasurement returns the first GoodputMeasurement in the namespace
// that references this Job, or nil if none exists.
func findJobGoodputMeasurement(ctx context.Context, c client.Reader, job *crev1alpha1.Job) *crev1alpha1.GoodputMeasurement {
	var measurements crev1alpha1.GoodputMeasurementList
	if err := c.List(ctx, &measurements, matchingJobRef(job.Namespace, job.Name)...); err != nil {
		return nil
	}
	if len(measurements.Items) == 0 {
		return nil
	}
	return &measurements.Items[0]
}

// findJobBandwidthMeasurement returns the first BandwidthMeasurement in the namespace
// that references this Job, or nil if none exists.
func findJobBandwidthMeasurement(ctx context.Context, c client.Reader, job *crev1alpha1.Job) *crev1alpha1.BandwidthMeasurement {
	var measurements crev1alpha1.BandwidthMeasurementList
	if err := c.List(ctx, &measurements, matchingJobRef(job.Namespace, job.Name)...); err != nil {
		return nil
	}
	if len(measurements.Items) == 0 {
		return nil
	}
	return &measurements.Items[0]
}

// collectJobMeasuredValues gathers metric values from BandwidthMeasurement and
// GoodputMeasurement status fields. Keys match the threshold registry.
func collectJobMeasuredValues(ctx context.Context, c client.Reader, job *crev1alpha1.Job) map[string]float64 {
	values := make(map[string]float64)

	if bm := findJobBandwidthMeasurement(ctx, c, job); bm != nil && len(bm.Status.Results) > 0 {
		values["busBandwidthGBps"] = maxBusBandwidth(bm.Status.Results)
		values["algBandwidthGBps"] = maxAlgBandwidth(bm.Status.Results)
	}

	// Goodput-derived values are provisional until the measurement's Complete
	// condition is True: the GoodputMeasurement controller freezes the status
	// in a single terminal write anchored to the Job's terminal transition
	// (ADR-072). Evaluating earlier would make pass/fail depend on when this
	// controller happened to read. The missing-key requeue and the
	// measurementTimeout machinery in checkPerformanceThresholds handle the
	// wait for Complete.
	if gm := findJobGoodputMeasurement(ctx, c, job); gm != nil &&
		meta.IsStatusConditionTrue(gm.Status.Conditions, crev1alpha1.GoodputMeasurementComplete) {
		if v := parseStallFloat(gm.Status.Result); v > 0 {
			values["goodputRatio"] = v
		}
		if v := parseStallFloat(gm.Status.AvgTFLOPSPerGPU); v > 0 {
			values["avgTFLOPsPerGPU"] = v
		}
		if v := parseStallFloat(gm.Status.AvgStepTimeSec); v > 0 {
			values["avgStepTimeSec"] = v
		}
	}
	return values
}

// isJobAwaitingThresholdEvaluation returns true when a succeeded Job has performance
// thresholds configured but the Job controller has not yet recorded the outcome.
// The Job controller sets ValidationFailed=True on violation or ValidationFailed=False
// when all thresholds pass. Workflow should keep groups running until one is set.
func isJobAwaitingThresholdEvaluation(job *crev1alpha1.Job) bool {
	if len(job.Spec.Thresholds) == 0 {
		return false
	}
	succeededCond := meta.FindStatusCondition(job.Status.Conditions, crev1alpha1.JobSucceeded)
	if succeededCond == nil || succeededCond.Status != metav1.ConditionTrue {
		return false
	}
	return meta.FindStatusCondition(job.Status.Conditions, crev1alpha1.JobValidationFailed) == nil
}
