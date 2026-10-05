// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// runWorkloadRunRender validates --platform before doing anything else, so an
// invalid name must fail with the full list of valid names, and every name
// platform detection can return must be accepted. Issue #184: nscale is
// detected by the controller and referenced by catalog overrides, but the
// hardcoded validator list rejected it. Cases with gpuArch cover --gpu-arch
// supplying the architecture a nodeSelector without gpu.product lacks.
func TestRenderPlatformFlag(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-platform-flag",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `yaml:"platform"`
			GPUArch  string `yaml:"gpuArch"`
			DryRun   bool   `yaml:"dryRun"`
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

		emitted, renderErr := captureStdout(t, func() error {
			if cfg.DryRun {
				return runWorkloadRunRenderDryRun(runPath, "yaml", cfg.Platform, cfg.GPUArch, kubeconfig.NewConfigFlags(true))
			}
			return runWorkloadRunRender(runPath, "yaml", cfg.Platform, cfg.GPUArch)
		})

		type result struct {
			Error                   string `json:"error"`
			DetectedGPUArchitecture string `json:"detectedGPUArchitecture,omitempty"`
		}
		var r result
		if renderErr != nil {
			r.Error = renderErr.Error()
		} else {
			var workflow nvcrev1alpha1.Workflow
			if err := yaml.Unmarshal([]byte(emitted), &workflow); err != nil {
				return err
			}
			r.DetectedGPUArchitecture = workflow.Annotations["nvcrectl.nvidia.com/detected-gpu-architecture"]
		}

		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
