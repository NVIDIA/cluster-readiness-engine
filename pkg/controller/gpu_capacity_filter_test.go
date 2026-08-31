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

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A node with fewer allocatable GPUs than the workload's per-node request can
// never schedule the workload's pods. Before this filter existed such a node
// was partitioned into a group anyway, its pods sat Pending forever, and the
// run hung at InProgress with nothing naming the cause (issue #82).
//
// The missing-allocatable and zero-allocatable cases pin the deliberately
// conservative choices: the resource is advertised asynchronously by the
// device plugin, so a node that does not report it is kept, not dropped — and
// a node reporting zero is kept too, because the kubelet zeroes the count
// (rather than removing it) when the plugin endpoint goes away. Excluding on
// either would shrink or terminally fail the run over a plugin restart.
func TestFilterNodesByGPUCapacity(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "gpu-capacity-filter",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name string `yaml:"name"`
				// AllocatableGPUs is the node's reported allocatable
				// nvidia.com/gpu. Nil models a node that does not report the
				// resource at all (device plugin not up yet).
				AllocatableGPUs *int64 `yaml:"allocatableGPUs"`
			} `yaml:"nodes"`
			GpusPerNode int32 `yaml:"gpusPerNode"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		given := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}}
			if n.AllocatableGPUs != nil {
				node.Status.Allocatable = corev1.ResourceList{
					"nvidia.com/gpu": *resource.NewQuantity(*n.AllocatableGPUs, resource.DecimalSI),
				}
			}
			given = append(given, node)
		}

		kept, excluded := filterNodesByGPUCapacity(given, input.GpusPerNode)

		keptNames := make([]string, 0, len(kept))
		for i := range kept {
			keptNames = append(keptNames, kept[i].Name)
		}
		excludedOut := make([]map[string]any, 0, len(excluded))
		for _, e := range excluded {
			excludedOut = append(excludedOut, map[string]any{
				"node": e.Node,
				"has":  e.AllocatableGPUs,
			})
		}

		out := struct {
			Kept     []string         `json:"kept"`
			Excluded []map[string]any `json:"excluded"`
			// BestAvailable is what an all-nodes-too-small failure names next
			// to the requirement, so the golden pins it alongside the split.
			BestAvailable int64 `json:"bestAvailable"`
		}{Kept: keptNames, Excluded: excludedOut, BestAvailable: maxAllocatableGPUs(excluded)}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
