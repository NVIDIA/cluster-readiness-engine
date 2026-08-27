// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
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
	p := testutil.TestCaseParser{
		Subdir:         "build-cluster-info-counts-real-gpus",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Nodes []struct {
				Name  string `yaml:"name"`
				Count int64  `yaml:"count"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(in.Nodes))
		for _, n := range in.Nodes {
			nodes = append(nodes, gpuNode(n.Name, n.Count))
		}

		// The catalog default for a100 is 8. These nodes have 1 each.
		const catalogDefault = int32(8)

		info := buildClusterInfo(nodes, "onprem", "a100", "NVIDIA-A100-PCIE-40GB",
			catalogDefault, "")

		gotPerNode := make([]int32, 0, len(info.Nodes))
		for _, n := range info.Nodes {
			gotPerNode = append(gotPerNode, n.GPUs)
		}

		b, err := json.MarshalIndent(struct {
			GpusPerNode        int32   `json:"gpusPerNode"`
			TotalGPUs          int     `json:"totalGPUs"`
			CatalogGpusPerNode int32   `json:"catalogGpusPerNode"`
			TotalNodes         int     `json:"totalNodes"`
			PerNode            []int32 `json:"perNode"`
		}{
			GpusPerNode:        info.GpusPerNode,
			TotalGPUs:          info.TotalGPUs,
			CatalogGpusPerNode: info.CatalogGpusPerNode,
			TotalNodes:         info.TotalNodes,
			PerNode:            gotPerNode,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// No nodes must not panic or report a stale count.
func TestBuildClusterInfoNoNodes(t *testing.T) {
	info := buildClusterInfo(nil, "onprem", "a100", "", 8, "")
	assert.Equal(t, int32(0), info.GpusPerNode)
	assert.Equal(t, 0, info.TotalGPUs)
	assert.Equal(t, 0, info.TotalNodes)
	assert.Empty(t, info.Nodes)
}
