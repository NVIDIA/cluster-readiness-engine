// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"fmt"
	"testing"
)

const benchTopologyKey = "network.nvidia.com/rack"

// benchNodes synthesizes n nodes spread round-robin across domains of
// perDomain nodes each, labeled with benchTopologyKey. Names are zero-padded
// so the sort inside PartitionNodes sees realistic ordered input.
func benchNodes(n, perDomain int) []NodeInfo {
	nodes := make([]NodeInfo, n)
	for i := range nodes {
		nodes[i] = NodeInfo{
			Name: fmt.Sprintf("node-%04d", i),
			Labels: map[string]string{
				benchTopologyKey: fmt.Sprintf("rack-%03d", i/perDomain),
			},
		}
	}
	return nodes
}

func benchmarkPartition(b *testing.B, input PartitionInput) {
	b.ReportAllocs()
	for b.Loop() {
		groups, err := PartitionNodes(input)
		if err != nil {
			b.Fatal(err)
		}
		if len(groups) == 0 {
			b.Fatal("no groups produced")
		}
	}
}

func BenchmarkPartitionSimple64(b *testing.B) {
	benchmarkPartition(b, PartitionInput{
		Nodes:       benchNodes(64, 8),
		NodesPerJob: 8,
	})
}

func BenchmarkPartitionSimple512(b *testing.B) {
	benchmarkPartition(b, PartitionInput{
		Nodes:       benchNodes(512, 16),
		NodesPerJob: 16,
	})
}

func BenchmarkPartitionTopology64(b *testing.B) {
	benchmarkPartition(b, PartitionInput{
		Nodes:       benchNodes(64, 8),
		NodesPerJob: 8,
		TopologyKey: benchTopologyKey,
	})
}

func BenchmarkPartitionTopology512(b *testing.B) {
	benchmarkPartition(b, PartitionInput{
		Nodes:       benchNodes(512, 16),
		NodesPerJob: 16,
		TopologyKey: benchTopologyKey,
	})
}
