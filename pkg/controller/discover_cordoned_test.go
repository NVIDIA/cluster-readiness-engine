// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// Discovery drops cordoned nodes because they cannot run workload pods. It used
// to drop them silently, so nothing downstream could tell a fleet that was
// fully certified from one where a leftover cordon meant half of it was never
// tested — both reported PASSED. These cases pin what discovery hands back.
//
// The names come back sorted for the same reason the node list does: they are
// written to status and printed in the report, so an unsorted list would make
// one cluster produce a different report on each reconcile.
func TestDiscoverCordonedNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "discover-cordoned",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name       string `yaml:"name"`
				Cordoned   bool   `yaml:"cordoned"`
				NoGPU      bool   `yaml:"noGPU"`
				Unlabelled bool   `yaml:"unlabelled"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		const present = "true"

		given := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}}
			if !n.Unlabelled {
				node.Labels = map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
				}
				if !n.NoGPU {
					node.Labels[GPUNodeLabel] = present
				}
			}
			node.Spec.Unschedulable = n.Cordoned
			given = append(given, node)
		}

		nodes, cordoned, err := discoverTargetNodes(context.Background(),
			unorderedReader{nodes: given},
			&crev1alpha1.TargetSpec{})
		if err != nil {
			return err
		}

		certified := make([]string, 0, len(nodes))
		for i := range nodes {
			certified = append(certified, nodes[i].Name)
		}

		out := struct {
			Certified []string `json:"certified"`
			Cordoned  []string `json:"cordoned"`
		}{certified, cordoned}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
