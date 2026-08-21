// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"encoding/json"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

func TestScreenGroups(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "screen-groups",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			TopologyKey string `yaml:"topologyKey"`
			Nodes       []struct {
				Name   string            `yaml:"name"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		var nodeInfos []NodeInfo
		for _, n := range input.Nodes {
			nodeInfos = append(nodeInfos, NodeInfo{
				Name:   n.Name,
				Labels: n.Labels,
			})
		}

		groups, err := ScreenGroups(DiagnoseScreenInput{
			Nodes:       nodeInfos,
			TopologyKey: input.TopologyKey,
		})
		if err != nil {
			return err
		}

		data, err := json.MarshalIndent(groups, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestBuildConfirmationGroups(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "confirmation-groups",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			SuspectNodes []string `yaml:"suspectNodes"`
			HealthyNodes []string `yaml:"healthyNodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		groups := BuildConfirmationGroups(input.SuspectNodes, input.HealthyNodes)
		if groups == nil {
			groups = []Group{}
		}

		data, err := json.MarshalIndent(groups, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
