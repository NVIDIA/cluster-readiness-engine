// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gpuNode builds a ready node. A negative count leaves nvidia.com/gpu unset,
// which is what a node shows before the device plugin advertises the resource.
func gpuNode(name string, count int64) corev1.Node {
	n := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if count >= 0 {
		n.Status.Allocatable[resourceNvidiaGPU] = *resource.NewQuantity(count, resource.DecimalSI)
	}
	return n
}

func TestNodeGPUCount(t *testing.T) {
	assert.Equal(t, int32(8), nodeGPUCount(gpuNode("a", 8)))
	assert.Equal(t, int32(1), nodeGPUCount(gpuNode("b", 1)))
	assert.Equal(t, int32(0), nodeGPUCount(gpuNode("c", 0)))
	// Resource absent: the device plugin has not advertised it.
	assert.Equal(t, int32(0), nodeGPUCount(gpuNode("d", -1)))
	// Too large for int32. Without the guard this wraps to a negative count,
	// which then becomes the reported minimum across every node.
	assert.Equal(t, int32(0), nodeGPUCount(gpuNode("e", math.MaxInt32+1)))
}

func TestBuildClusterInfoCountsRealGPUs(t *testing.T) {
	// The catalog default for a100 is 8. These nodes have 1 each.
	const catalogDefault = int32(8)

	tests := []struct {
		name            string
		nodes           []corev1.Node
		wantGpusPerNode int32
		wantTotal       int
		wantPerNode     []int32
	}{
		{
			name:            "one GPU per node does not report the catalog default",
			nodes:           []corev1.Node{gpuNode("node002", 1), gpuNode("node003", 1)},
			wantGpusPerNode: 1,
			wantTotal:       2,
			wantPerNode:     []int32{1, 1},
		},
		{
			name:            "eight GPUs per node matches the catalog default",
			nodes:           []corev1.Node{gpuNode("n0", 8), gpuNode("n1", 8)},
			wantGpusPerNode: 8,
			wantTotal:       16,
			wantPerNode:     []int32{8, 8},
		},
		{
			name:            "mixed counts report the smallest",
			nodes:           []corev1.Node{gpuNode("n0", 8), gpuNode("n1", 2)},
			wantGpusPerNode: 2,
			wantTotal:       10,
			wantPerNode:     []int32{8, 2},
		},
		{
			// A broken device plugin leaves a node at 0. The smallest must
			// stay 0 even when it comes first, so 0 is not mistaken for
			// "not seen yet".
			name:            "a zero node first keeps the smallest at zero",
			nodes:           []corev1.Node{gpuNode("n0", 0), gpuNode("n1", 5)},
			wantGpusPerNode: 0,
			wantTotal:       5,
			wantPerNode:     []int32{0, 5},
		},
		{
			name:            "a zero node last keeps the smallest at zero",
			nodes:           []corev1.Node{gpuNode("n0", 5), gpuNode("n1", 0)},
			wantGpusPerNode: 0,
			wantTotal:       5,
			wantPerNode:     []int32{5, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := buildClusterInfo(tt.nodes, "onprem", "a100", "NVIDIA-A100-PCIE-40GB",
				catalogDefault, "")

			assert.Equal(t, tt.wantGpusPerNode, info.GpusPerNode)
			assert.Equal(t, tt.wantTotal, info.TotalGPUs)
			assert.Equal(t, catalogDefault, info.CatalogGpusPerNode,
				"the catalog default must still be reported")
			assert.Equal(t, len(tt.nodes), info.TotalNodes)

			got := make([]int32, 0, len(info.Nodes))
			for _, n := range info.Nodes {
				got = append(got, n.GPUs)
			}
			assert.Equal(t, tt.wantPerNode, got)
		})
	}
}

// No nodes must not panic or report a stale count.
func TestBuildClusterInfoNoNodes(t *testing.T) {
	info := buildClusterInfo(nil, "onprem", "a100", "", 8, "")
	assert.Equal(t, int32(0), info.GpusPerNode)
	assert.Equal(t, 0, info.TotalGPUs)
	assert.Equal(t, 0, info.TotalNodes)
	assert.Empty(t, info.Nodes)
}
