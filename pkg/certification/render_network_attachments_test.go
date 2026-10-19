// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
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

// netAttachReplicatedJob records what networkAttachments controls on one
// replicatedJob: the Multus networks annotation, verbatim because its order
// is the rail order, and each container's limits and requests as sorted
// "limits/<name>=<qty>" entries.
type netAttachReplicatedJob struct {
	Dependency    string              `json:"dependency"`
	ReplicatedJob string              `json:"replicatedJob"`
	Networks      string              `json:"networks"`
	Resources     map[string][]string `json:"resources"`
}

type netAttachWorkflow struct {
	Workflow       string                   `json:"workflow"`
	ReplicatedJobs []netAttachReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderNetworkAttachments covers issue #400 through the path
// "nvcrectl certification render --platform <csp>" uses: the annotation in list
// order, per-rail limits and requests on the worker container only (never the
// MPI launcher), the OCI sriov-net default replaced rather than appended to, a
// per-category list replacing a spec-level nicResourceName, and two categories
// differing only in their attachments rendering under distinct Workflow names.
func TestCertificationRenderNetworkAttachments(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-network-attachments",
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

		result := make([]netAttachWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectNetworkAttachments(&workflows[i])
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

func projectNetworkAttachments(wf *nvcrev1alpha1.Workflow) (netAttachWorkflow, error) {
	out := netAttachWorkflow{Workflow: wf.Name, ReplicatedJobs: []netAttachReplicatedJob{}}
	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			continue
		}
		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		if typeMeta.Kind != trainerv1alpha1.TrainingRuntimeKind {
			continue
		}
		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			pod := rj.Template.Spec.Template
			projected := netAttachReplicatedJob{
				Dependency:    rt.Name,
				ReplicatedJob: rj.Name,
				Networks:      pod.Annotations["k8s.v1.cni.cncf.io/networks"],
				Resources:     map[string][]string{},
			}
			for _, c := range pod.Spec.Containers {
				res := []string{}
				for name, qty := range c.Resources.Limits {
					res = append(res, fmt.Sprintf("limits/%s=%s", name, qty.String()))
				}
				for name, qty := range c.Resources.Requests {
					res = append(res, fmt.Sprintf("requests/%s=%s", name, qty.String()))
				}
				sort.Strings(res)
				projected.Resources[c.Name] = res
			}
			out.ReplicatedJobs = append(out.ReplicatedJobs, projected)
		}
	}
	return out, nil
}
