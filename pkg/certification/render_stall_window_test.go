// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestRenderStartupStallWindow pins the catalog default and option precedence
// without changing the catalog's adaptive stall multiplier.
func TestRenderStartupStallWindow(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-startup-stall-window",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}
		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, "aws", "", nil, "")
		if err != nil {
			return err
		}
		if len(workflows) != 1 {
			return fmt.Errorf("rendered %d workflows, want 1", len(workflows))
		}
		spec := workflows[0].Spec.JobTemplate.Spec
		result := struct {
			StartupStallTimeoutSeconds *int32 `json:"startupStallTimeoutSeconds"`
			StallMultiplier            *int32 `json:"stallMultiplier"`
		}{
			StartupStallTimeoutSeconds: spec.StartupStallTimeoutSeconds,
			StallMultiplier:            spec.StallMultiplier,
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
