// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// testScale: intra-node used to do nothing. The template had a branch for it
// that emitted an empty execution block while numNodes kept the full count, so
// the run was partitioned exactly as full-scale. Confirmed on hardware: a
// Certification asking for intra-node over two nodes still produced
// nodesPerJob 2 and totalGroups 1.
//
// Partitioning reads the workload's numNodes, so that is what has to become 1.
func TestTestScaleNodeCount(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "test-scale-node-count",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			NodesPerJob   int32  `yaml:"nodesPerJob"`
			TestScale     string `yaml:"testScale"`
			MaxConcurrent int32  `yaml:"maxConcurrent"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		entry := Lookup("communication", "nccl-all-reduce")
		spec, err := entry.Build(burninv1alpha1.TargetSpec{
			NodeSelector: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
			},
		}, BuildConfig{
			NodesPerJob:     in.NodesPerJob,
			GpusPerNode:     8,
			GPUArchitecture: "h100",
			TestScale:       in.TestScale,
			MaxConcurrent:   in.MaxConcurrent,
		})
		if err != nil {
			return err
		}

		orch, err := yaml.Marshal(spec.Orchestration)
		if err != nil {
			return err
		}

		out := struct {
			NumNodes            *int32 `json:"numNodes"`
			ExecutionKeyCount   int    `json:"executionKeyCount"`
			HasTopologyBlock    bool   `json:"hasTopologyBlock"`
			MaxConcurrentInSpec int    `json:"maxConcurrentInSpec"`
		}{
			NumNodes:            spec.JobTemplate.Spec.Workload.TrainJob.Trainer.NumNodes,
			ExecutionKeyCount:   strings.Count(string(orch), "execution:"),
			HasTopologyBlock:    spec.Orchestration.Topology != nil,
			MaxConcurrentInSpec: spec.Orchestration.Execution.MaxConcurrent,
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
