// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

// entry is the per-node triple recorded in the golden output.
type entry struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func TestMergeFailedNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "merge-failed-nodes",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Existing []string `yaml:"existing"`
			New      []string `yaml:"new"`
			// Messages maps a node name to the message the detector supplied for
			// it, mirroring checkNodeHealth's per-node map. Names absent from it
			// fall back to the shared constant, which is what a detector that
			// supplies no message produces.
			Messages map[string]string `yaml:"messages"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		msgFor := func(name string) string {
			if msg := input.Messages[name]; msg != "" {
				return msg
			}
			return defaultNodeHealthFailureMessage
		}
		// The CEL hardware-failure path attributes both existing and newly
		// detected nodes to the same reason, so use a single reason here to
		// exercise (name, reason) dedup the way it happens in production.
		existing := noderesults.NodesWithPerNodeDetails(input.Existing, ReasonHardwareFailureDetected, msgFor)
		incoming := noderesults.NodesWithPerNodeDetails(input.New, ReasonHardwareFailureDetected, msgFor)
		merged, hasNew := mergeFailedNodes(existing, incoming)
		// Entries carry the merged messages, not just names: a regression that
		// re-flattened per-node attribution to one constant would keep the names
		// identical and only show up here.
		output := struct {
			Result  []string `json:"result"`
			Entries []entry  `json:"entries"`
			HasNew  bool     `json:"hasNew"`
		}{Result: noderesults.FailedNodeNames(merged), Entries: []entry{}, HasNew: hasNew}
		for _, fn := range merged {
			output.Entries = append(output.Entries, entry{
				Name: fn.Name, Reason: string(fn.Reason), Message: fn.Message,
			})
		}
		// Handle nil result for empty output
		if output.Result == nil {
			output.Result = []string{}
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
