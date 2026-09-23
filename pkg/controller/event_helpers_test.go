// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// The tests below pin that each reconciler's warnf tolerates an unset
// Recorder, mirroring TestJobWarnfNilRecorder (job_event_test.go). Every warnf
// runs inside a reconcile path that unit tests and the integration harness
// exercise with a bare reconciler struct, so a nil dereference here would
// panic the reconcile loop rather than drop an event.

// TestCertificationWarnfNilRecorder pins that warnf tolerates an unset
// Recorder. It is called from createWorkflowForCategory.
func TestCertificationWarnfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &CertificationReconciler{} // Recorder deliberately unset
	cert := &nvcrev1alpha1.Certification{
		Name: "cert", Namespace: testNS,
	}

	r.warnf(cert, ReasonWorkflowCreationError,
		"Failed to create Workflow %s: %v", "cert-training-nemotron5-8b", errNilRecorderProbe)
}

// TestGoodputMeasurementWarnfNilRecorder pins that warnf tolerates an unset
// Recorder. It is called from noteLogProfileUnresolved.
func TestGoodputMeasurementWarnfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &GoodputMeasurementReconciler{} // Recorder deliberately unset
	gm := &nvcrev1alpha1.GoodputMeasurement{
		Name: "gm", Namespace: testNS,
	}

	r.warnf(gm, reasonGoodputLogProfileMissing,
		"LogProfile %q could not be resolved: %v", "missing", errNilRecorderProbe)
}

// TestBandwidthMeasurementWarnfNilRecorder pins that warnf tolerates an unset
// Recorder. It is called from noteLogProfileUnresolved.
func TestBandwidthMeasurementWarnfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &BandwidthMeasurementReconciler{} // Recorder deliberately unset
	bm := &nvcrev1alpha1.BandwidthMeasurement{
		Name: "bm", Namespace: testNS,
	}

	r.warnf(bm, reasonBandwidthLogProfileMissing,
		"LogProfile %q could not be resolved: %v", "missing", errNilRecorderProbe)
}

// TestWorkloadRunWarnfNilRecorder pins that warnf tolerates an unset Recorder.
// It is called from Reconcile (build guard and Workflow creation).
func TestWorkloadRunWarnfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &WorkloadRunReconciler{} // Recorder deliberately unset
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "run", Namespace: "default",
	}

	r.warnf(run, ReasonWorkflowCreationError,
		"Failed to create Workflow %s: %v", "run", errNilRecorderProbe)
}

// The eventf helpers below are the generalized form warnf and normalf now
// delegate to (ADR-080). The transition hooks call eventf directly with a
// computed type, so it needs its own nil-Recorder pin at each tier rather than
// relying on the warnf pins above.

// TestCertificationEventfNilRecorder pins that eventf tolerates an unset
// Recorder. It is called from setExclusiveCondition's transition hook.
func TestCertificationEventfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &CertificationReconciler{} // Recorder deliberately unset
	cert := &nvcrev1alpha1.Certification{Name: "cert", Namespace: testNS}

	r.eventf(cert, corev1.EventTypeNormal, ReasonWorkflowSucceeded,
		"All certification categories completed successfully")
	r.eventf(cert, corev1.EventTypeWarning, ReasonWorkflowFailed,
		"One or more certification categories failed")
}

// TestJobEventfNilRecorder pins that the Job tier's eventf tolerates an unset
// Recorder. Both the phase hook and the verdict hooks reach it.
func TestJobEventfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &JobReconciler{} // Recorder deliberately unset
	job := &nvcrev1alpha1.Job{Name: testJobName, Namespace: testNS}

	r.eventf(job, corev1.EventTypeNormal, reasonThresholdsMet,
		"All performance thresholds satisfied")
	r.eventf(job, corev1.EventTypeWarning, ReasonHardwareFailureDetected,
		"Hardware failure detected on node(s): [%s]", testNodeA)
}

// TestWorkloadRunEventfNilRecorder pins that eventf tolerates an unset
// Recorder. setWorkloadRunConditionAndUpdate calls it after a successful write.
func TestWorkloadRunEventfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &WorkloadRunReconciler{} // Recorder deliberately unset
	run := &nvcrev1alpha1.WorkloadRun{Name: testRunName, Namespace: testNS}

	r.eventf(run, corev1.EventTypeNormal, ReasonWorkflowCreated, "Workflow %s created", testRunName)
	r.eventf(run, corev1.EventTypeWarning, ReasonBuildFailedStatusUpdateFailed,
		"WorkloadRun build failed: %v", errNilRecorderProbe)
}

// TestWorkflowEventfNilRecorder pins the Workflow tier's eventf, which now also
// carries the two status-write-failure fallbacks.
func TestWorkflowEventfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &WorkflowReconciler{} // Recorder deliberately unset
	workflow := &nvcrev1alpha1.Workflow{Name: "workflow", Namespace: testNS}

	r.eventf(workflow, corev1.EventTypeWarning, ReasonOverrideErrorStatusUpdateFailed,
		"Override failed: %v; recording the Workflow Failed condition also failed: %v",
		errNilRecorderProbe, errNilRecorderProbe)
	r.eventf(workflow, corev1.EventTypeWarning, ReasonHeterogeneousPlatformStatusUpdateFailed,
		"Platform detection failed: %v; recording the Workflow Failed condition also failed: %v",
		errNilRecorderProbe, errNilRecorderProbe)
}
