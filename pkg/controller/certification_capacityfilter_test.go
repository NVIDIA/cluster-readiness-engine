// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestResolveNodesPerJobAfterCapacityFilter covers sizing a job on a fleet
// with mixed GPU counts — the same class of problem as the architecture
// filter, on a different axis (issue #82). Nodes reporting fewer allocatable
// GPUs than gpusPerNode are dropped before resolveNodesPerJob, so the job is
// sized against nodes that can actually run it; sizing against the whole set
// partitioned unschedulable nodes into groups whose pods sat Pending forever.
//
// The all-nodes-too-small case pins the fail-fast: it must error with the
// requirement and the best available count, never quietly size a job no node
// can host.
func TestResolveNodesPerJobAfterCapacityFilter(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-nodes-per-job-after-capacity-filter",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name            string `yaml:"name"`
				AllocatableGPUs *int64 `yaml:"allocatableGPUs"`
			} `yaml:"nodes"`
			NodesPerJob *int32 `yaml:"nodesPerJob"`
			GpusPerNode int32  `yaml:"gpusPerNode"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name:   n.Name,
				Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
			}}
			if n.AllocatableGPUs != nil {
				node.Status.Allocatable = corev1.ResourceList{
					"nvidia.com/gpu": *resource.NewQuantity(*n.AllocatableGPUs, resource.DecimalSI),
				}
			}
			nodes = append(nodes, node)
		}

		opts := nvcrev1alpha1.CategoryOptions{NodesPerJob: input.NodesPerJob}
		cat := nvcrev1alpha1.CertificateCategory{Domain: "communication", Variant: "nccl-all-reduce"}

		// The steps the Certification controller performs, in order: arch
		// detection first (gpusPerNode derives from it), then the capacity
		// filter with the resolved gpusPerNode, then sizing against survivors.
		gpuArch, archNodes := detectGPUArchConsistent(nodes)

		out := struct {
			TotalNodes   int      `json:"totalNodes"`
			CapableNodes []string `json:"capableNodes"`
			// NodesPerJob resolved against the capacity-filter survivors.
			NodesPerJob int32  `json:"nodesPerJob"`
			Error       string `json:"error,omitempty"`
		}{TotalNodes: len(nodes)}

		capableNodes, err := dropUnderCapacityNodes(archNodes, cat, input.GpusPerNode)
		if err != nil {
			out.Error = err.Error()
		}
		for i := range capableNodes {
			out.CapableNodes = append(out.CapableNodes, capableNodes[i].Name)
		}

		if err == nil {
			npj, err := resolveNodesPerJob(capableNodes, cat, opts, nil, input.GpusPerNode, gpuArch)
			if err != nil {
				out.Error = err.Error()
			}
			out.NodesPerJob = npj
		}

		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
