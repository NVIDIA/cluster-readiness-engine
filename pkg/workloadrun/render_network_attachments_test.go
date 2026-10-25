// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// wrRenderNetAttachJob is what networkAttachments controls on one
// replicatedJob of the rendered TrainingRuntime: the Multus networks
// annotation and each container's sorted "limits/<name>=<qty>" and
// "requests/<name>=<qty>" entries.
type wrRenderNetAttachJob struct {
	Dependency    string              `json:"dependency"`
	ReplicatedJob string              `json:"replicatedJob"`
	Networks      string              `json:"networks"`
	Resources     map[string][]string `json:"resources"`
}

// TestWorkloadRunRenderNetworkAttachments runs "nvcrectl workloadrun render
// --platform <csp> --gpu-arch <arch>" end to end, platform overrides
// included. An entry on a resource the platform override also requests adds
// to it: Azure or TogetherAI H100 plus one nvidia.com/mlnxnics entry requests
// 9, the same as certification render's cases of the same name.
func TestWorkloadRunRenderNetworkAttachments(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-network-attachments",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
			GPUArch  string `json:"gpuArch"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}
		runPath := filepath.Join(tc.T.TempDir(), "workloadrun.yaml")
		if err := os.WriteFile(runPath, []byte(tc.Inputs["input_workloadrun.yaml"]), 0o600); err != nil {
			return err
		}
		emitted, err := captureStdout(t, func() error {
			return runWorkloadRunRender(runPath, "yaml", cfg.Platform, cfg.GPUArch)
		})
		if err != nil {
			return err
		}
		var workflow nvcrev1alpha1.Workflow
		if err := yaml.Unmarshal([]byte(emitted), &workflow); err != nil {
			return err
		}
		out, err := projectRenderNetworkAttachments(workflow.Spec.Dependencies)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func projectRenderNetworkAttachments(deps []nvcrev1alpha1.DependencySpec) ([]wrRenderNetAttachJob, error) {
	out := []wrRenderNetAttachJob{}
	for _, dep := range deps {
		var obj struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						ReplicatedJobs []struct {
							Name     string `json:"name"`
							Template struct {
								Spec struct {
									Template corev1.PodTemplateSpec `json:"template"`
								} `json:"spec"`
							} `json:"template"`
						} `json:"replicatedJobs"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
			return nil, err
		}
		if obj.Kind != "TrainingRuntime" {
			continue
		}
		for _, rj := range obj.Spec.Template.Spec.ReplicatedJobs {
			pod := rj.Template.Spec.Template
			row := wrRenderNetAttachJob{
				Dependency:    obj.Metadata.Name,
				ReplicatedJob: rj.Name,
				Networks:      pod.Annotations[platform.MultusNetworksAnnotation],
				Resources:     map[string][]string{},
			}
			for _, ctr := range pod.Spec.Containers {
				entries := []string{}
				for name, q := range ctr.Resources.Limits {
					entries = append(entries, fmt.Sprintf("limits/%s=%s", name, q.String()))
				}
				for name, q := range ctr.Resources.Requests {
					entries = append(entries, fmt.Sprintf("requests/%s=%s", name, q.String()))
				}
				sort.Strings(entries)
				row.Resources[ctr.Name] = entries
			}
			out = append(out, row)
		}
	}
	return out, nil
}
