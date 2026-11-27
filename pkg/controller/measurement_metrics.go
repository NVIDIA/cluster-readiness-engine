// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// Match the measurement identity, not mutable labels or the latest result sizes.
// This also removes provisional-only series and works after the Job is deleted.
func cleanupMeasurementBandwidthMetrics(namespace, measurement string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelMeasurement: measurement}
	for _, g := range ncclBandwidthGaugeSets {
		g.algBW.DeletePartialMatch(labels)
		g.busBW.DeletePartialMatch(labels)
	}
}

// syncFinalBandwidthMetrics publishes final results only after a successful
// terminal read. Failed or unavailable reads can leave provisional status
// behind, but those samples must not be retained as final measurements.
func syncFinalBandwidthMetrics(m *nvcrev1alpha1.BandwidthMeasurement) {
	cleanupMeasurementBandwidthMetrics(m.Namespace, m.Name)
	complete := meta.FindStatusCondition(m.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete)
	if complete == nil || complete.Status != metav1.ConditionTrue || complete.Reason != reasonBandwidthJobSucceeded {
		return
	}
	for _, result := range m.Status.Results {
		recordNCCLBandwidthMetrics(m.Namespace, m.Name, m.Spec.JobRef.Name,
			m.Labels[labelWorkflowTracking], m.Spec.TestType, strconv.FormatInt(result.SizeBytes, 10),
			parseFloat(result.AlgBW), parseFloat(result.BusBW))
	}
}
