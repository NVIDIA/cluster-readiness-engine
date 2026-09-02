// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

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
