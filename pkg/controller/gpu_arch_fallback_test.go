// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestGPUArchFallbackMessage pins the GPUArchitectureDefaults event text
// (ADR-092): an architecture missing from gpu-defaults.yaml names the
// assumed counts and the caller's fields; a listed architecture, an empty
// one, and the "unknown" sentinel for unlabeled nodes all stay silent.
func TestGPUArchFallbackMessage(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "gpu-arch-fallback-message",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			GPUArch     string `yaml:"gpuArch"`
			GpusPerNode int32  `yaml:"gpusPerNode"`
			MlnxPerNode int32  `yaml:"mlnxPerNode"`
			FieldHint   string `yaml:"fieldHint"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		hint := map[string]string{
			"Certification": gpuArchFieldHintCertification,
			"WorkloadRun":   gpuArchFieldHintWorkloadRun,
		}[input.FieldHint]
		if hint == "" {
			return fmt.Errorf("unknown fieldHint %q", input.FieldHint)
		}
		nd := catalog.NodeDefaults{GpusPerNode: input.GpusPerNode, MlnxPerNode: input.MlnxPerNode}
		out := struct {
			Message string `json:"message"`
		}{Message: gpuArchFallbackMessage(input.GPUArch, nd, hint)}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
