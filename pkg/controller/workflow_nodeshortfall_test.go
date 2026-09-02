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
// and taints, so when the real cause is an architecture or GPU capacity filter
// it sends the operator to look at the wrong thing. Cases cover each shape.
func TestNotEnoughNodesMessage(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "not-enough-nodes-message",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Needed           int      `yaml:"needed"`
			Found            int      `yaml:"found"`
			GPUArch          string   `yaml:"gpuArch"`
			ArchExcluded     []string `yaml:"archExcluded"`
			CapacityExcluded []struct {
				Node string `yaml:"node"`
				Has  int64  `yaml:"has"`
			} `yaml:"capacityExcluded"`
			GpusPerNode int32 `yaml:"gpusPerNode"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		capExcluded := make([]gpuCapacityExclusion, 0, len(input.CapacityExcluded))
		for _, e := range input.CapacityExcluded {
			capExcluded = append(capExcluded, gpuCapacityExclusion{Node: e.Node, AllocatableGPUs: e.Has})
		}

		got := notEnoughNodesMessage(input.Needed, input.Found, input.GPUArch, input.ArchExcluded, capExcluded, input.GpusPerNode)

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
