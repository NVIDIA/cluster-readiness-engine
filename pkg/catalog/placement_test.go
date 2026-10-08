// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestPlacementIntoWorkflowSpec pins how BuildConfig.Placement reaches the built
// WorkflowSpec, which is the seam between the Certification tier and the only
// orchestration input the Workflow controller reads.
//
// It is set in Go rather than in each entry's YAML on purpose. The eight entry
// templates have orchestration blocks of different shapes, from a bare
// iterations: 1 to the richer NCCL ones, and a {{- if .Placement }} conditional
// in each is eight chances for a whitespace or nesting mistake to perturb
// rendered output that has nothing to do with this feature.
//
// The unset case is the load-bearing one. The guard is what keeps every existing
// catalog golden byte-identical, and there is deliberately no
// +kubebuilder:default on the field, so if an empty placement ever starts
// writing a value this is where it shows up first.
func TestPlacementIntoWorkflowSpec(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "placement",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Category    string                   `json:"category"`
			Subcategory string                   `json:"subcategory"`
			Target      nvcrev1alpha1.TargetSpec `json:"target"`
			NodesPerJob int32                    `json:"nodesPerJob"`
			GpusPerNode int32                    `json:"gpusPerNode"`
			Placement   string                   `json:"placement"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		entry := Lookup(input.Category, input.Subcategory)
		if entry == nil {
			return fmt.Errorf("category %s/%s not registered", input.Category, input.Subcategory)
		}
		spec, err := entry.Build(input.Target, BuildConfig{
			NodesPerJob:     input.NodesPerJob,
			GpusPerNode:     input.GpusPerNode,
			GPUArchitecture: GPUArchFromNodeSelector(input.Target.NodeSelector),
			Placement:       input.Placement,
		})
		if err != nil {
			return err
		}

		// The trainer's numNodes is reported alongside placement because the two
		// travel together and the Workflow controller reads the size back out of
		// the template, not out of the orchestration block. On GB200/GB300 the
		// ComputeDomain is templated from the same number, so this value is also
		// what sizes the DRA channel allocation.
		//
		// Iterations proves the surrounding orchestration block is intact: the
		// placement is set in Go after the template renders, and a mistake there
		// would be visible as a block that lost its other fields.
		b, err := json.MarshalIndent(struct {
			Placement  string `json:"placement"`
			NumNodes   *int32 `json:"numNodes"`
			Iterations int    `json:"iterations"`
		}{
			Placement:  spec.Orchestration.Placement,
			NumNodes:   spec.JobTemplate.Spec.Workload.TrainJob.Trainer.NumNodes,
			Iterations: spec.Orchestration.Iterations,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
