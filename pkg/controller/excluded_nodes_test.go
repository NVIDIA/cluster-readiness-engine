// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nodeWithProduct(name, product string) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if product != "" {
		n.Labels = map[string]string{"nvidia.com/gpu.product": product}
	}
	return n
}

func TestExcludedNodeNames(t *testing.T) {
	all := []corev1.Node{
		nodeWithProduct("node003", "NVIDIA-H100-80GB-HBM3"),
		nodeWithProduct("node002", "NVIDIA-A100-PCIE-40GB"),
		nodeWithProduct("node001", "NVIDIA-A100-PCIE-40GB"),
	}

	t.Run("names the dropped nodes, sorted", func(t *testing.T) {
		kept := []corev1.Node{all[1], all[2]}
		require.Equal(t, []string{"node003"}, excludedNodeNames(all, kept))
	})

	t.Run("nothing dropped gives nothing", func(t *testing.T) {
		require.Empty(t, excludedNodeNames(all, all))
	})

	t.Run("everything dropped names them all, sorted", func(t *testing.T) {
		require.Equal(t, []string{"node001", "node002", "node003"},
			excludedNodeNames(all, nil))
	})
}

// The filter itself keeps nodes[0]'s architecture, and the node list is not
// sorted before this runs, so which architecture wins depends on List order.
// This pins the current behaviour so a change to it is deliberate.
func TestDetectGPUArchConsistentKeepsFirstNodesArch(t *testing.T) {
	tests := []struct {
		name         string
		nodes        []corev1.Node
		wantArch     string
		wantKept     int
		wantExcluded []string
	}{
		{
			name: "uniform set is untouched",
			nodes: []corev1.Node{
				nodeWithProduct("a", "NVIDIA-A100-PCIE-40GB"),
				nodeWithProduct("b", "NVIDIA-A100-PCIE-40GB"),
			},
			wantArch: "a100", wantKept: 2, wantExcluded: nil,
		},
		{
			name: "mixed set keeps the first node's arch, even in the minority",
			nodes: []corev1.Node{
				nodeWithProduct("first", "NVIDIA-H100-80GB-HBM3"),
				nodeWithProduct("second", "NVIDIA-A100-PCIE-40GB"),
				nodeWithProduct("third", "NVIDIA-A100-PCIE-40GB"),
			},
			wantArch: "h100", wantKept: 1,
			wantExcluded: []string{"second", "third"},
		},
		{
			name: "reversing the order changes which nodes survive",
			nodes: []corev1.Node{
				nodeWithProduct("second", "NVIDIA-A100-PCIE-40GB"),
				nodeWithProduct("third", "NVIDIA-A100-PCIE-40GB"),
				nodeWithProduct("first", "NVIDIA-H100-80GB-HBM3"),
			},
			wantArch: "a100", wantKept: 2,
			wantExcluded: []string{"first"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arch, kept := detectGPUArchConsistent(tt.nodes)
			require.Equal(t, tt.wantArch, arch)
			require.Len(t, kept, tt.wantKept)
			require.Equal(t, tt.wantExcluded, excludedNodeNames(tt.nodes, kept))
		})
	}
}
