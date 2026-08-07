// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

func gpuNode(name, product string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.present": "true",
				"nvidia.com/gpu.product": product,
			},
		},
	}
}

// unorderedReader hands back nodes in a fixed, deliberately wrong order.
// The controller-runtime fake client sorts by name, which would hide the very
// thing this test is for, so the ordering has to be controlled directly.
type unorderedReader struct{ nodes []corev1.Node }

func (u unorderedReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	nl, ok := list.(*corev1.NodeList)
	if !ok {
		return nil
	}
	nl.Items = append([]corev1.Node(nil), u.nodes...)
	return nil
}

func (u unorderedReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return nil
}

// Callers read nodes[0] to choose the platform and the GPU architecture for a
// whole run, so discovery has to return a stable order. client.List promises
// none: over two H100 nodes and one A100, two envtest runs certified h100 with
// 2 nodes and a100 with 1 node from identical input.
func TestDiscoverTargetNodesSortsByName(t *testing.T) {
	target := &burninv1alpha1.TargetSpec{
		NodeSelector: map[string]string{"nvidia.com/gpu.present": "true"},
	}

	tests := []struct {
		name  string
		given []corev1.Node
	}{
		{
			name: "reverse order",
			given: []corev1.Node{
				gpuNode("node-c", "NVIDIA-A100-PCIE-40GB"),
				gpuNode("node-b", "NVIDIA-H100-80GB-HBM3"),
				gpuNode("node-a", "NVIDIA-H100-80GB-HBM3"),
			},
		},
		{
			name: "odd node first, which is what made the run non-deterministic",
			given: []corev1.Node{
				gpuNode("node-c", "NVIDIA-A100-PCIE-40GB"),
				gpuNode("node-a", "NVIDIA-H100-80GB-HBM3"),
				gpuNode("node-b", "NVIDIA-H100-80GB-HBM3"),
			},
		},
		{
			name: "already sorted stays sorted",
			given: []corev1.Node{
				gpuNode("node-a", "NVIDIA-H100-80GB-HBM3"),
				gpuNode("node-b", "NVIDIA-H100-80GB-HBM3"),
				gpuNode("node-c", "NVIDIA-A100-PCIE-40GB"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes, err := discoverTargetNodes(context.Background(),
				unorderedReader{nodes: tt.given}, target)
			require.NoError(t, err)

			names := make([]string, len(nodes))
			for i := range nodes {
				names[i] = nodes[i].Name
			}
			require.Equal(t, []string{"node-a", "node-b", "node-c"}, names,
				"discovery must return nodes in name order whatever List gave back")

			// The point of the ordering: the architecture chosen for the run is
			// the same every time, so the same cluster certifies the same subset.
			arch, kept := detectGPUArchConsistent(nodes)
			require.Equal(t, "h100", arch)
			require.Equal(t, []string{"node-c"}, excludedNodeNames(nodes, kept))
		})
	}
}
