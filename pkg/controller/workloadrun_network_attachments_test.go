// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// wrNetAttachReplicatedJob is what networkAttachments controls on one
// replicatedJob of a WorkloadRun's TrainingRuntime once every override has
// run: the Multus networks annotation and the sorted "limits/<name>=<qty>"
// and "requests/<name>=<qty>" entries of each container.
type wrNetAttachReplicatedJob struct {
	Dependency    string              `json:"dependency"`
	ReplicatedJob string              `json:"replicatedJob"`
	Networks      string              `json:"networks"`
	Resources     map[string][]string `json:"resources"`
}

// TestWorkloadRunControllerNetworkAttachments covers issue #400 on the
// WorkloadRun controller path (buildWorkflowSpecWithWarning), which
// pkg/workloadrun's BuildWorkflowSpec golden does not reach: NIC detection
// skipped when the list is set, a platform NIC request rendered without
// Multus kept beside the list unless mlnxPerNode: 0 drops it, and the user's
// own spec.overrides applied after the list, as the Workflow controller
// applies them, so one that sets the networks annotation wins.
func TestWorkloadRunControllerNetworkAttachments(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-network-attachments",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Platform        string                    `json:"platform"`
			GPUArchitecture string                    `json:"gpuArchitecture"`
			Nodes           []corev1.Node             `json:"nodes"`
			Run             nvcrev1alpha1.WorkloadRun `json:"run"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return err
		}
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		objs := make([]client.Object, 0, len(in.Nodes))
		for i := range in.Nodes {
			objs = append(objs, &in.Nodes[i])
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		r := &WorkloadRunReconciler{Client: c, Scheme: scheme}
		ws, err := r.buildWorkflowSpec(context.Background(), &in.Run)
		if err != nil {
			return err
		}
		if _, err := applyOverridesWithTracking(ws, OverrideContext{
			Platform:        in.Platform,
			GPUArchitecture: in.GPUArchitecture,
		}); err != nil {
			return err
		}
		out, err := projectWRNetworkAttachments(ws.Dependencies)
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

func projectWRNetworkAttachments(deps []nvcrev1alpha1.DependencySpec) ([]wrNetAttachReplicatedJob, error) {
	out := []wrNetAttachReplicatedJob{}
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
			row := wrNetAttachReplicatedJob{
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
