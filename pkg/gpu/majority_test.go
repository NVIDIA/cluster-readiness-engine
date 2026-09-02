// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// MajorityArchitecture is the one vote every detection path shares, so these
// cases pin the semantics all of them inherit: majority wins, ties keep the
// earliest node's architecture, unlabeled nodes never outvote labeled ones,
// and "" comes back only when no node is labeled at all.
func TestMajorityArchitecture(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "majority-architecture",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name    string `yaml:"name"`
				Product string `yaml:"product"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{Name: n.Name}
			if n.Product != "" {
				node.Labels = map[string]string{"nvidia.com/gpu.product": n.Product}
			}
			nodes = append(nodes, node)
		}

		got := MajorityArchitecture(nodes)

		b, err := json.MarshalIndent(struct {
			Architecture string `json:"architecture"`
		}{got}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
