// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestNotEnoughNodesMessage covers the wording of a node shortfall. The bare
// message talks about schedulability, which is Kubernetes vocabulary for cordons
// and taints, so on a heterogeneous target it sends the operator to look at the
// wrong thing. Cases cover both shapes.
func TestNotEnoughNodesMessage(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "not-enough-nodes-message",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Needed       int      `yaml:"needed"`
			Found        int      `yaml:"found"`
			GPUArch      string   `yaml:"gpuArch"`
			ArchExcluded []string `yaml:"archExcluded"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		got := notEnoughNodesMessage(input.Needed, input.Found, input.GPUArch, input.ArchExcluded)

		data, err := json.MarshalIndent(struct {
			Message string `json:"message"`
		}{Message: got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
