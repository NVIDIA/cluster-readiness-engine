// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// unorderedReader hands nodes back in the order given. The controller-runtime
// fake client sorts by name, which would hide the very thing these cases are
// for, so the order has to be controlled directly.
type unorderedReader struct{ nodes []corev1.Node }

func (u unorderedReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	if nl, ok := list.(*corev1.NodeList); ok {
		nl.Items = append([]corev1.Node(nil), u.nodes...)
	}
	return nil
}

func (u unorderedReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return nil
}

// Callers read nodes[0] to choose the platform and the GPU architecture for a
// whole run, so discovery has to return a stable order. Before it sorted, two
// runs over two H100 nodes and one A100 certified h100 with 2 nodes and a100
// with 1 node from identical input.
func TestDiscoverNodeOrder(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "discover-node-order",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []string `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		given := make([]corev1.Node, 0, len(input.Nodes))
		for _, name := range input.Nodes {
			given = append(given, corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"nvidia.com/gpu.present": "true",
					"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
				},
			}})
		}

		nodes, _, err := discoverTargetNodes(context.Background(),
			unorderedReader{nodes: given},
			&crev1alpha1.TargetSpec{
				NodeSelector: map[string]string{"nvidia.com/gpu.present": "true"},
			})
		if err != nil {
			return err
		}

		names := make([]string, 0, len(nodes))
		for i := range nodes {
			names = append(names, nodes[i].Name)
		}

		b, err := json.MarshalIndent(struct {
			Discovered []string `json:"discovered"`
		}{names}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
