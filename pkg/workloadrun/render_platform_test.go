// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// runWorkloadRunRender validates --platform before doing anything else, so an
// invalid name must fail with the full list of valid names, and every name
// platform detection can return must be accepted. Issue #184: nscale is
// detected by the controller and referenced by catalog overrides, but the
// hardcoded validator list rejected it.
func TestRenderPlatformFlag(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-platform-flag",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `yaml:"platform"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		// Cases with a WorkloadRun file exercise the full render path; cases
		// without one must fail on flag validation before the file is read.
		runPath := filepath.Join(t.TempDir(), "workloadrun.yaml")
		if runData, ok := tc.Inputs["input_workloadrun.yaml"]; ok {
			if err := os.WriteFile(runPath, []byte(runData), 0o644); err != nil {
				return err
			}
		}

		renderErr := runWorkloadRunRender(runPath, "yaml", cfg.Platform)

		type result struct {
			Error string `json:"error"`
		}
		var r result
		if renderErr != nil {
			r.Error = renderErr.Error()
		}

		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
