// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// TestBuildJobTemplate_NilExecPanics verifies that buildJobTemplate panics with
// a clear message when the exec framework is selected but spec.framework.exec is
// nil. The exec path is reached when neither Framework.Torch nor Framework.MPI
// is set, so frameworkType falls through to the default (exec) case.
func TestBuildJobTemplate_NilExecPanics(t *testing.T) {
	r := &WorkloadRunReconciler{}
	run := &burninv1alpha1.WorkloadRun{}
	run.Name = "wr-nil-exec"
	run.Spec.Image = "nvcr.io/test:latest"
	run.Spec.NumNodes = 1
	// Framework.Exec is deliberately left nil; Torch and MPI are also nil.

	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("expected panic but got none")
		}
		msg, ok := v.(string)
		if !ok {
			t.Fatalf("panic value is not a string: %v", v)
		}
		want := "spec.framework.exec is nil"
		if !strings.Contains(msg, want) {
			t.Fatalf("panic message %q does not contain %q", msg, want)
		}
	}()

	r.buildJobTemplate(run, FrameworkExec, 8)
}
