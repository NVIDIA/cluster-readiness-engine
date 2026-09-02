// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// readWorkloadRun is the single choke point through which render, dry-run,
// and run load the user's YAML, so validation applied here covers every CLI
// entry point. The unknown-threshold-key case is issue #52: avgTFLOPSPerGPU
// (capital S) is the GoodputMeasurement status field, one character away from
// the threshold key avgTFLOPsPerGPU, and it used to slip through and surface
// only after the run as a misleading MeasurementTimeout.
func TestReadWorkloadRun(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "read-workloadrun",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		file := filepath.Join(tc.T.TempDir(), "workloadrun.yaml")
		if err := os.WriteFile(file, []byte(tc.Inputs["input.yaml"]), 0o600); err != nil {
			return err
		}

		run, err := readWorkloadRun(file)
		if err != nil {
			return err
		}

		// Record identity and the thresholds map — the fields this validation
		// is about — rather than the whole spec.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Name       string            `json:"name"`
			Thresholds map[string]string `json:"thresholds"`
		}{run.Name, run.Spec.Thresholds}); err != nil {
			return err
		}
		tc.Actual = buf.String()
		return nil
	})
}
