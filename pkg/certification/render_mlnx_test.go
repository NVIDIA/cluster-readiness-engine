// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// mlnxContainer records the resource requests on one runtime container. The NIC
// request and the GPU request are projected together on purpose: the mlnxnics
// guard removes the whole resources key on the fragments where mlnxnics is its
// only child, and the override merge deletes nil-valued keys, so a guard that
// left an empty key behind would take the GPU request with it.
type mlnxContainer struct {
	Name string `json:"name"`
	// Resources renders limits and requests as sorted "limits/<name>=<qty>"
	// and "requests/<name>=<qty>" strings.
	Resources []string `json:"resources"`
}

// mlnxReplicatedJob records what the platform override did to one
// replicatedJob of one TrainingRuntime dependency.
type mlnxReplicatedJob struct {
	Dependency    string `json:"dependency"`
	ReplicatedJob string `json:"replicatedJob"`
	// PodAnnotations is the worker pod template's annotations, where
	// k8s.v1.cni.cncf.io/networks carries the SR-IOV attachments. An empty map
	// is as load-bearing as a populated one: attaching sriov-net without
	// requesting nvidia.com/mlnxnics is not a valid configuration, so the
	// annotation and the resource must appear and disappear together.
	PodAnnotations map[string]string `json:"podAnnotations"`
	Containers     []mlnxContainer   `json:"containers"`
}

// mlnxWorkflow is the per-Workflow projection written to the golden file.
type mlnxWorkflow struct {
	Workflow        string              `json:"workflow"`
	DependencyKinds []string            `json:"dependencyKinds"`
	ReplicatedJobs  []mlnxReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderMlnxNIC covers the nvidia.com/mlnxnics request end to
// end through the same path "nvcrectl certification render --platform <csp>"
// uses. The OCI goldens pin both halves of issue #350: the NIC request and the
// k8s.v1.cni.cncf.io/networks annotation that names the sriov-net
// NetworkAttachmentDefinition.
//
// OCI GB200 resolves mlnxPerNode to 0 from the platform override and must emit
// neither, on the NCCL collectives and on training alike. OCI GB300 keeps its
// count of 4, pinning that the guard keys on a zero count rather than on the
// platform, and the explicit-count case pins that a site running the SR-IOV
// device plugin can opt back in.
//
// The Azure pair covers the other guard shape. Azure's fragment carries
// mlnxnics as the only entry under limits and requests, so the guard removes
// the resources key itself; the explicit-zero golden must still show the base
// runtime's nvidia.com/gpu request, because the override merge deletes
// nil-valued keys and would drop the GPU request along with a childless
// resources key. See ADR-087.
func TestCertificationRenderMlnxNIC(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-mlnx",
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

		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		gpuArch, err := catalog.ParseGPUArchFlag(cfg.GPUArch)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform, gpuArch, nil, "")
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform, gpuArch); err != nil {
			return err
		}

		result := make([]mlnxWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectMlnxNIC(&workflows[i])
			if projectErr != nil {
				return projectErr
			}
			result = append(result, projected)
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectMlnxNIC walks a resolved Workflow in declaration order: dependencies as
// the catalog lists them, then replicatedJobs as the runtime lists them. Only
// the resource strings are sorted (map iteration order), so the output is
// stable without hiding a reordering.
func projectMlnxNIC(wf *nvcrev1alpha1.Workflow) (mlnxWorkflow, error) {
	out := mlnxWorkflow{
		Workflow:        wf.Name,
		DependencyKinds: []string{},
		ReplicatedJobs:  []mlnxReplicatedJob{},
	}

	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			out.DependencyKinds = append(out.DependencyKinds, "")
			continue
		}

		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		out.DependencyKinds = append(out.DependencyKinds, typeMeta.Kind)
		if typeMeta.Kind != trainerv1alpha1.TrainingRuntimeKind {
			continue
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			podTemplate := rj.Template.Spec.Template
			projected := mlnxReplicatedJob{
				Dependency:     rt.Name,
				ReplicatedJob:  rj.Name,
				PodAnnotations: map[string]string{},
				Containers:     []mlnxContainer{},
			}
			maps.Copy(projected.PodAnnotations, podTemplate.Annotations)
			for _, c := range podTemplate.Spec.Containers {
				pc := mlnxContainer{Name: c.Name, Resources: []string{}}
				for name, qty := range c.Resources.Limits {
					pc.Resources = append(pc.Resources, fmt.Sprintf("limits/%s=%s", name, qty.String()))
				}
				for name, qty := range c.Resources.Requests {
					pc.Resources = append(pc.Resources, fmt.Sprintf("requests/%s=%s", name, qty.String()))
				}
				sort.Strings(pc.Resources)
				projected.Containers = append(projected.Containers, pc)
			}
			out.ReplicatedJobs = append(out.ReplicatedJobs, projected)
		}
	}
	return out, nil
}
