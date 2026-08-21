// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// A certification reports PASSED after excluding nodes whose GPU architecture
// differs from the primary one. Each case records which nodes are certified and
// which are left untested, so the exclusion is visible rather than silent.
func TestGPUArchExclusion(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "gpu-arch-exclusion",
		ExpectedSuffix: ".json",
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
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}}
			if n.Product != "" {
				node.Labels = map[string]string{"nvidia.com/gpu.product": n.Product}
			}
			nodes = append(nodes, node)
		}

		arch, kept := detectGPUArchConsistent(nodes)
		keptNames := make([]string, 0, len(kept))
		for i := range kept {
			keptNames = append(keptNames, kept[i].Name)
		}

		out := struct {
			Architecture string   `json:"architecture"`
			Certified    []string `json:"certified"`
			Excluded     []string `json:"excluded"`
		}{arch, keptNames, excludedNodeNames(nodes, kept)}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
