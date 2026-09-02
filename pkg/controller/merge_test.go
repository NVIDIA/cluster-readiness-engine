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

func TestMergeFailedNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "merge-failed-nodes",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Existing []string `yaml:"existing"`
			New      []string `yaml:"new"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		// The CEL hardware-failure path attributes both existing and newly
		// detected nodes to the same reason, so use a single reason here to
		// exercise (name, reason) dedup the way it happens in production.
		existing := noderesults.NodesWithFailureDetails(input.Existing, ReasonHardwareFailureDetected, "test message")
		merged, hasNew := mergeFailedNodes(existing, input.New, ReasonHardwareFailureDetected, "test message")
		output := struct {
			Result []string `json:"result"`
			HasNew bool     `json:"hasNew"`
		}{Result: noderesults.FailedNodeNames(merged), HasNew: hasNew}
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
