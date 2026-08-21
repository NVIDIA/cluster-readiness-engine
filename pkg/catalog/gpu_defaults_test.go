// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

type gpuDefaultsInput struct {
	GpuArch  string `yaml:"gpuArch"`
	Platform string `yaml:"platform"`
}

func TestGPUDefaults(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "gpu-defaults",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input gpuDefaultsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		nd := GPUDefaults(input.GpuArch, input.Platform)
		b, err := json.MarshalIndent(nd, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
