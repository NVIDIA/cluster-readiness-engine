// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// testScale: intra-node used to do nothing. The catalog template had a branch
// for it that emitted an empty execution block, while numNodes kept the full
// node count, so the run was partitioned exactly as full-scale. Confirmed on
// hardware: a Certification asking for intra-node still produced
// nodesPerJob: 2, totalGroups: 1 over two nodes.
//
// Partitioning reads the workload's numNodes, so that is what has to become 1.
func TestIntraNodeGivesOneNodePerJob(t *testing.T) {
	target := burninv1alpha1.TargetSpec{
		NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
	}

	tests := []struct {
		name         string
		testScale    string
		wantNumNodes int32
	}{
		{name: "intra-node tests each node on its own", testScale: "intra-node", wantNumNodes: 1},
		{name: "full-scale keeps the requested count", testScale: "full-scale", wantNumNodes: 8},
		{name: "intra-rack keeps the requested count", testScale: "intra-rack", wantNumNodes: 8},
		{name: "unset defaults to full-scale", testScale: "", wantNumNodes: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := Lookup("communication", "nccl-all-reduce")
			require.NotNil(t, entry)

			spec, err := entry.Build(target, BuildConfig{
				NodesPerJob:     8,
				GpusPerNode:     8,
				GPUArchitecture: "h100",
				TestScale:       tt.testScale,
			})
			require.NoError(t, err)

			trainer := spec.JobTemplate.Spec.Workload.TrainJob.Trainer
			require.NotNil(t, trainer.NumNodes)
			require.Equal(t, tt.wantNumNodes, *trainer.NumNodes)
		})
	}
}

// The removed intra-node branch emitted `execution: {}`, which collided with
// the maxConcurrent block below it and could produce two execution keys in one
// mapping.
func TestIntraNodeWithMaxConcurrentHasOneExecutionKey(t *testing.T) {
	entry := Lookup("communication", "nccl-all-reduce")
	require.NotNil(t, entry)

	spec, err := entry.Build(burninv1alpha1.TargetSpec{
		NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
	}, BuildConfig{
		NodesPerJob:     8,
		GpusPerNode:     8,
		GPUArchitecture: "h100",
		TestScale:       "intra-node",
		MaxConcurrent:   2,
	})
	require.NoError(t, err)

	out, err := yaml.Marshal(spec.Orchestration)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(out), "execution:"),
		"exactly one execution key, got:\n%s", string(out))
	require.Contains(t, string(out), "maxConcurrent: 2")
}
