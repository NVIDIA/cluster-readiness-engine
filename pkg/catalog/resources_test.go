// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

type resourcesInput struct {
	Category    string                           `json:"category"`
	Subcategory string                           `json:"subcategory"`
	Target      nvcrev1alpha1.TargetSpec         `json:"target"`
	NodesPerJob int32                            `json:"nodesPerJob"`
	GpusPerNode int32                            `json:"gpusPerNode"`
	Resources   *nvcrev1alpha1.CategoryResources `json:"resources"`
}

// TestTrainingResources verifies the CPU/memory resources that training
// entries render into their TrainingRuntime container: the DGX-class catalog
// defaults when CategoryOptions.resources is unset, and the user's values
// when it is (issue #83).
func TestTrainingResources(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "resources",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input resourcesInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		entry := Lookup(input.Category, input.Subcategory)
		if entry == nil {
			return fmt.Errorf("category %s/%s not registered", input.Category, input.Subcategory)
		}
		spec, buildErr := entry.Build(input.Target, BuildConfig{
			NodesPerJob:     input.NodesPerJob,
			GpusPerNode:     input.GpusPerNode,
			GPUArchitecture: GPUArchFromNodeSelector(input.Target.NodeSelector),
			Resources:       input.Resources,
		})
		if buildErr != nil {
			return buildErr
		}
		resources, err := trainingContainerResources(spec)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(resources, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// trainingContainerResources extracts the "node" container's resources block
// from the TrainingRuntime dependency of a built WorkflowSpec.
func trainingContainerResources(spec nvcrev1alpha1.WorkflowSpec) (map[string]any, error) {
	for _, dep := range spec.Dependencies {
		var obj map[string]any
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
			continue
		}
		if obj["kind"] != "TrainingRuntime" {
			continue
		}
		var runtime struct {
			Spec struct {
				Template struct {
					Spec struct {
						ReplicatedJobs []struct {
							Template struct {
								Spec struct {
									Template struct {
										Spec struct {
											Containers []struct {
												Name      string         `json:"name"`
												Resources map[string]any `json:"resources"`
											} `json:"containers"`
										} `json:"spec"`
									} `json:"template"`
								} `json:"spec"`
							} `json:"template"`
						} `json:"replicatedJobs"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(dep.Raw, &runtime); err != nil {
			return nil, err
		}
		for _, rj := range runtime.Spec.Template.Spec.ReplicatedJobs {
			for _, c := range rj.Template.Spec.Template.Spec.Containers {
				if c.Name == "node" {
					return c.Resources, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no TrainingRuntime dependency with a %q container found", "node")
}
