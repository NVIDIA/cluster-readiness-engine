// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// workloadRunWatchLine formats the "[watch]" heartbeat that "nvcrectl
// workloadrun run --wait" prints while polling. These cases pin the line
// format: which phase is picked from the execution conditions, when the
// condition message is appended, and the pending line before the controller
// has set any condition. Elapsed time is a fixed input, never wall clock.
func TestWorkloadRunWatchLine(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "watch-line",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// json tags, not yaml: sigs.k8s.io/yaml converts YAML to JSON and
		// encoding/json ignores yaml tags, silently zeroing mistyped keys.
		var input struct {
			Name    string                    `json:"name"`
			Elapsed string                    `json:"elapsed"`
			Run     nvcrev1alpha1.WorkloadRun `json:"run"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		elapsed, err := time.ParseDuration(input.Elapsed)
		if err != nil {
			return err
		}

		tc.Actual = workloadRunWatchLine(&input.Run, input.Name, elapsed) + "\n"
		return nil
	})
}
