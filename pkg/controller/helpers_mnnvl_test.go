// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestDefaultEnableMNNVL pins which architectures default to multi-node
// NVLink. HGX B200/B300 have NVSwitch inside the chassis only, so they stay
// false even though B300 shares a name prefix with GB300 (ADR-092).
func TestDefaultEnableMNNVL(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "default-enable-mnnvl",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			GPUArch string `yaml:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		b, err := json.MarshalIndent(struct {
			EnableMNNVL bool `json:"enableMNNVL"`
		}{DefaultEnableMNNVL(input.GPUArch)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
