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

type sourceRepoInput struct {
	Category    string                   `json:"category"`
	Subcategory string                   `json:"subcategory"`
	Target      nvcrev1alpha1.TargetSpec `json:"target"`
	NodesPerJob int32                    `json:"nodesPerJob"`
	GpusPerNode int32                    `json:"gpusPerNode"`
	SourceRepo  string                   `json:"sourceRepo"`
}

// TestSourceRepo verifies the clone URL that entries with a source checkout
// render into their clone init container: the entry's own canonical upstream
// when CategoryOptions.sourceRepo is unset, and the user's mirror URL when it
// is set (issue #320). Both nemotron5 entries default to Megatron-LM.
func TestSourceRepo(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "source-repo",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input sourceRepoInput
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
			SourceRepo:      input.SourceRepo,
		})
		if buildErr != nil {
			return buildErr
		}
		args, err := sourceCloneArgs(spec)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(args, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// sourceCloneArgs extracts the "megatron-clone" init container's args from
// the TrainingRuntime dependency of a built WorkflowSpec. The container name
// is entry-owned; both nemotron5 entries name their source-clone step
// "megatron-clone" because Megatron-LM is their entry-defined source.
func sourceCloneArgs(spec nvcrev1alpha1.WorkflowSpec) ([]string, error) {
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
											InitContainers []struct {
												Name string   `json:"name"`
												Args []string `json:"args"`
											} `json:"initContainers"`
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
			for _, c := range rj.Template.Spec.Template.Spec.InitContainers {
				if c.Name == "megatron-clone" {
					return c.Args, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no TrainingRuntime dependency with a %q init container found", "megatron-clone")
}
