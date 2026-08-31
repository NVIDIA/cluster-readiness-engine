// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The startup-stall budget must start from actual running, never from time
// spent suspended in an admission queue (issue #213): the GM anchor is
// clamped forward to the Job's workloadStartTime, and workloadStartTime alone
// is deliberately not an anchor (scheduling and image pulls are bounded by
// timeoutPerJob, not the stall detector). Fixed absolute times are fine here
// because startupStallAnchor never reads the wall clock.
func TestStartupStallAnchor(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "startup-stall-anchor",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			GMStatus  nvcrev1alpha1.GoodputMeasurementStatus `yaml:"gmStatus"`
			JobStatus nvcrev1alpha1.JobStatus                `yaml:"jobStatus"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		gm := &nvcrev1alpha1.GoodputMeasurement{Status: input.GMStatus}

		// The controller passes either the persisted status.workloadStartTime
		// or, on the reconcile that first observes the workload running, the
		// candidate "now" — the clamp treats both identically.
		anchor, ok := startupStallAnchor(gm, input.JobStatus.WorkloadStartTime)
		out := map[string]any{"ok": ok}
		if ok {
			out["anchor"] = anchor.UTC().Format("2006-01-02T15:04:05Z")
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
