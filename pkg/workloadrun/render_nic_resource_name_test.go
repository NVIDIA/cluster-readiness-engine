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
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestWorkloadRunRenderNICResourceName runs "nvcrectl workloadrun render
// --platform <csp> --gpu-arch <arch>" end to end and pins which NIC resource
// the platform override requests. On Azure and TogetherAI, nicResourceName
// replaces the nvidia.com/mlnxnics default (issue #583), the same answer
// certification render gives through the catalog fragments; unset, the
// default stays, and mlnxPerNode: 0 drops the request whichever name is set.
func TestWorkloadRunRenderNICResourceName(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-nic-resource-name",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
			GPUArch  string `json:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}
		runPath := filepath.Join(tc.T.TempDir(), "workloadrun.yaml")
		if err := os.WriteFile(runPath, []byte(tc.Inputs["input_workloadrun.yaml"]), 0o600); err != nil {
			return err
		}
		emitted, err := captureStdout(t, func() error {
			return runWorkloadRunRender(runPath, "yaml", cfg.Platform, cfg.GPUArch, "")
		})
		if err != nil {
			return err
		}
		var workflow nvcrev1alpha1.Workflow
		if err := yaml.Unmarshal([]byte(emitted), &workflow); err != nil {
			return err
		}
		out, err := projectRenderNetworkAttachments(workflow.Spec.Dependencies)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
