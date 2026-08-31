// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A node can be left uncertified for more than one reason at once, and the
// report derives its INCOMPLETE verdict from this one list. So the summary has
// to carry every cause: a fleet that is part cordoned, part mixed-architecture
// and part under GPU capacity must report all of them, not whichever was
// recorded last.
//
// The empty case matters as much as the rest. A run that certified everything
// it targeted must produce no list at all, because any list at all turns a
// PASSED into an INCOMPLETE.
func TestExclusionSummary(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "exclusion-summary",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Cordoned         []string `yaml:"cordoned"`
			ArchExcluded     []string `yaml:"archExcluded"`
			GPUArch          string   `yaml:"gpuArch"`
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

		nodes, reason := exclusionSummary(input.Cordoned, input.ArchExcluded, input.GPUArch, capExcluded, input.GpusPerNode)

		out := struct {
			ExcludedNodes []string `json:"excludedNodes"`
			Reason        string   `json:"exclusionReason"`
			// Verdict mirrors what pkg/report does with the list, which is the
			// reason any of this is recorded. Keeping it in the golden means a
			// change that empties the list shows up as a verdict change.
			Verdict string `json:"verdictAfterPassingRun"`
		}{ExcludedNodes: nodes, Reason: reason, Verdict: "PASSED"}
		if len(nodes) > 0 {
			out.Verdict = "INCOMPLETE"
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
