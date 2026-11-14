// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

func cleanupMeasurementGoodputMetrics(namespace, measurement string) {
	labels := prometheus.Labels{labelNamespace: namespace, labelMeasurement: measurement}
	for _, g := range []*prometheus.GaugeVec{
		goodputRatioGauge, goodputAvgTFLOPSGauge, goodputAvgStepTimeGauge,
		goodputRescheduleTimeGauge, goodputResumeTimeGauge, goodputCheckpointSaveTimeGauge,
		goodputLostWorkTimeGauge, goodputTrainingTimeGauge, goodputWarmupTimeGauge,
		goodputNonWarmupTimeGauge,
	} {
		g.DeletePartialMatch(labels)
	}
}

// syncFinalBandwidthMetrics publishes only the frozen final read, never a
// provisional fallback retained in status after a failed or missing Job.
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

// syncFinalGoodputMetrics is called after the terminal write succeeds, including
// conflict refresh, and on already-complete reconciles. It does not recompute
// cumulative values or mutate the frozen status.
func (r *GoodputMeasurementReconciler) syncFinalGoodputMetrics(ctx context.Context, m *nvcrev1alpha1.GoodputMeasurement) {
	cleanupMeasurementGoodputMetrics(m.Namespace, m.Name)
	complete := meta.FindStatusCondition(m.Status.Conditions, nvcrev1alpha1.GoodputMeasurementComplete)
	if complete == nil || complete.Status != metav1.ConditionTrue ||
		(complete.Reason != "JobSucceeded" && complete.Reason != "JobFailed") {
		return
	}
	workflow := m.Labels[labelWorkflowTracking]
	if workflow == "" {
		if owner := metav1.GetControllerOf(m); owner != nil && owner.Kind == "Workflow" && owner.APIVersion == nvcrev1alpha1.GroupVersion.String() {
			workflow = owner.Name
		}
	}
	if workflow == "" {
		// Legacy standalone measurements may not carry a workflow label or owner.
		// Missing Jobs do not prevent restoration of the persisted result itself.
		job := &nvcrev1alpha1.Job{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.JobRef.Name}, job); err == nil {
			workflow = job.Labels[labelWorkflowTracking]
		}
	}
	s := m.Status
	recordGoodputMetrics(m.Namespace, m.Name, m.Spec.JobRef.Name, workflow, goodputMetricValues{
		GoodputRatio:       parseFloat(s.Result),
		AvgTFLOPS:          parseFloat(s.AvgTFLOPSPerGPU),
		AvgStepTime:        parseFloat(s.AvgStepTimeSec),
		RescheduleTime:     parseFloat(s.RescheduleTimeSec),
		ResumeTime:         parseFloat(s.ResumeTimeSec),
		CheckpointSaveTime: parseFloat(s.CheckpointSaveTimeSec),
		LostWorkTime:       parseFloat(s.LostWorkTimeSec),
		TrainingTime:       parseFloat(s.TrainingTimeSec),
		WarmupTime:         parseFloat(s.WarmupTimeSec),
		NonWarmupTime:      parseFloat(s.NonWarmupTimeSec),
	})
}
