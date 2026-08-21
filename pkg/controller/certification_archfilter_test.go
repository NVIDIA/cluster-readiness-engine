// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// TestResolveNodesPerJobAfterArchFilter covers sizing a job on a heterogeneous
// target. The Workflow filters to one GPU architecture before it partitions, so
// nodesPerJob has to be resolved against the filtered set. Resolving it against
// the whole target asks for nodes that will not be there and fails partitioning
// with a shortfall the operator did not cause.
//
// The constraint cases matter most: entry constraints (minGPUs, TP×PP
// divisibility) must still be honoured after filtering. A clamp that simply
// lowered nodesPerJob to the surviving count would break them.
func TestResolveNodesPerJobAfterArchFilter(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-nodes-per-job-after-arch-filter",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name    string `yaml:"name"`
				Product string `yaml:"product"`
			} `yaml:"nodes"`
			NodesPerJob *int32 `yaml:"nodesPerJob"`
			// Constraint emulates an entry's MaxValidNodes:
			//   "none" leaves it nil (any count valid)
			//   "even" accepts only even counts, as TP×PP divisibility does
			//   "min4" accepts only counts >= 4, as minGPUs does
			Constraint string `yaml:"constraint"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			nodes = append(nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:   n.Name,
				Labels: map[string]string{"nvidia.com/gpu.product": n.Product},
			}})
		}

		var entry *catalog.Entry
		switch input.Constraint {
		case "even":
			entry = &catalog.Entry{MaxValidNodes: func(available, _ int32, _ string) int32 {
				return available - available%2
			}}
		case "min4":
			entry = &catalog.Entry{MaxValidNodes: func(available, _ int32, _ string) int32 {
				if available < 4 {
					return 0
				}
				return available
			}}
		}

		opts := crev1alpha1.CategoryOptions{NodesPerJob: input.NodesPerJob}
		cat := crev1alpha1.CertificateCategory{Domain: "communication", Variant: "nccl-all-reduce"}

		// The two steps the Certification controller performs, in order.
		gpuArch, archNodes := detectGPUArchConsistent(nodes)
		out := struct {
			GPUArch    string `json:"gpuArch"`
			TotalNodes int    `json:"totalNodes"`
			ArchNodes  int    `json:"archNodes"`
			// NodesPerJob resolved against the filtered set. Sizing against
			// TotalNodes instead is what produced "need 2, found 1".
			NodesPerJob int32  `json:"nodesPerJob"`
			Error       string `json:"error,omitempty"`
		}{GPUArch: gpuArch, TotalNodes: len(nodes), ArchNodes: len(archNodes)}

		npj, err := resolveNodesPerJob(archNodes, cat, opts, entry, 8, gpuArch)
		if err != nil {
			out.Error = err.Error()
		}
		out.NodesPerJob = npj

		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
