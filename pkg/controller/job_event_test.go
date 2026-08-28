// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// TestJobWarnfNilRecorder pins that warnf tolerates an unset Recorder.
// It is called from reconcileWorkload and createWorkloadFromSpec, both of which
// run in tests and in any embedding that constructs JobReconciler directly, so a
// nil dereference here would panic the reconcile loop rather than drop an event.
func TestJobWarnfNilRecorder(t *testing.T) {
	t.Parallel()

	r := &JobReconciler{} // Recorder deliberately unset
	job := &nvcrev1alpha1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default"},
	}

	r.warnf(job, ReasonMeasurementCreationError,
		"Failed to ensure BandwidthMeasurement: %v", errNilRecorderProbe)
}

// errNilRecorderProbe is a sentinel used only to exercise the formatting path.
var errNilRecorderProbe = &probeError{}

type probeError struct{}

func (e *probeError) Error() string { return "probe" }
