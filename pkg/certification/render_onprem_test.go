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

// onpremContainer records the fields the ADR-075 on-prem override owns on one
// runtime container: the resource requests (where the optional NIC resource
// lands) and the env list (where the portable IB vars land for env-based
// entries).
type onpremContainer struct {
	Name string `json:"name"`
	// Resources renders limits and requests as sorted "limits/<name>=<qty>"
	// and "requests/<name>=<qty>" strings, so a golden shows the NIC resource
	// next to the GPU request it merged with.
	Resources []string `json:"resources"`
	// Env is the container env in declaration order as "NAME=value".
	Env []string `json:"env"`
}

// onpremReplicatedJob records what the override did to one replicatedJob of
// one TrainingRuntime dependency.
type onpremReplicatedJob struct {
	Dependency    string `json:"dependency"`
	ReplicatedJob string `json:"replicatedJob"`
	// MLPolicy lists the policy keys set on the runtime (torch, mpi, ...),
	// sorted. The TrainingRuntime CRD allows exactly one, so a fragment that
	// merges a second policy into a per-node torch runtime shows up here as
	// two keys (ADR-094).
	MLPolicy []string `json:"mlPolicy"`
	// Tolerations renders each toleration in declaration order as
	// "key=value:effect", or "operator=Exists" for a keyless one; the NVL72
	// on-prem override contributes the arm64 and GPU taints, the HGX override
	// only the GPU taint, and their absence on a control case is as
	// load-bearing as their presence on an on-prem one.
	Tolerations []string          `json:"tolerations"`
	Containers  []onpremContainer `json:"containers"`
}

// onpremWorkflow is the per-Workflow projection written to the golden file.
type onpremWorkflow struct {
	Workflow string `json:"workflow"`
	// TargetNodeSelector is the emitted orchestration target selector. Offline
	// render must leave it equal to the certification's own selector: neither
	// --gpu-arch nor the synthetic render node may leak into it.
	TargetNodeSelector map[string]string `json:"targetNodeSelector"`
	DependencyKinds    []string          `json:"dependencyKinds"`
	// TrainerArgs is the resolved jobTemplate trainer args. For the MPI
	// collectives this is where the on-prem override's env rides as -x pairs,
	// and where NCCL_IB_HCA/UCX_NET_DEVICES must NOT appear.
	TrainerArgs []string `json:"trainerArgs"`
	// TrainerEnv is the resolved jobTemplate trainer env ("NAME=value"),
	// where the loopback variants carry the replaced env list.
	TrainerEnv     []string              `json:"trainerEnv"`
	ReplicatedJobs []onpremReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderOnPrem covers the ADR-075 on-prem GB200/GB300
// override and the ADR-094 on-prem x86 HGX B200/B300 override end to end
// through the same path "nvcrectl certification render --platform onprem"
// uses: renderCertification resolves catalog templates with the
// certification's options (including nicResourceName), and
// resolveWorkflowsOffline matches the onprem override blocks against a
// synthetic no-providerID node. The goldens pin the markers the overrides own:
// the tolerations (arm64 and GPU on NVL72, GPU only on HGX, tolerate-everything
// kept on the per-node loopback entries), the optional NIC resource (present
// only when nicResourceName is set and mlnxPerNode is positive), the portable
// IB env on NVL72 and its absence on HGX, the runtime's single mlPolicy, and
// the absence of pinned HCA names. The h100 control case pins that none of it
// leaks outside those four architectures, and the gpu-arch-flag cases pin
// that --gpu-arch, bare or as a product name, stands in for a missing
// nvidia.com/gpu.product label.
func TestCertificationRenderOnPrem(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-onprem",
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

		result := make([]onpremWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectOnPremOverride(&workflows[i])
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

// projectOnPremOverride walks a resolved Workflow in declaration order:
// dependencies as the catalog lists them, then replicatedJobs as the runtime
// lists them. Only the resource strings are sorted (map iteration order),
// so the output is stable without hiding a reordering.
func projectOnPremOverride(wf *nvcrev1alpha1.Workflow) (onpremWorkflow, error) {
	out := onpremWorkflow{
		Workflow:        wf.Name,
		DependencyKinds: []string{},
		TrainerArgs:     []string{},
		TrainerEnv:      []string{},
		ReplicatedJobs:  []onpremReplicatedJob{},
	}
	if target := wf.Spec.Orchestration.Target; target != nil {
		out.TargetNodeSelector = target.NodeSelector
	}

	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
		out.TrainerArgs = append(out.TrainerArgs, tj.Trainer.Args...)
		for _, e := range tj.Trainer.Env {
			out.TrainerEnv = append(out.TrainerEnv, e.Name+"="+e.Value)
		}
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
			podSpec := rj.Template.Spec.Template.Spec
			projected := onpremReplicatedJob{
				Dependency:    rt.Name,
				ReplicatedJob: rj.Name,
				MLPolicy:      mlPolicyKeys(rt.Spec.MLPolicy),
				Tolerations:   []string{},
				Containers:    []onpremContainer{},
			}
			for _, tol := range podSpec.Tolerations {
				if tol.Key == "" {
					// A keyless toleration matches every taint; the operator is
					// the whole story (the per-node entries' tolerate-everything
					// toleration that tolerate-all-runtime-patch.yaml restores).
					projected.Tolerations = append(projected.Tolerations,
						fmt.Sprintf("operator=%s", tol.Operator))
					continue
				}
				projected.Tolerations = append(projected.Tolerations,
					fmt.Sprintf("%s=%s:%s", tol.Key, tol.Value, tol.Effect))
			}
			for _, c := range podSpec.Containers {
				pc := onpremContainer{Name: c.Name, Resources: []string{}, Env: []string{}}
				for name, qty := range c.Resources.Limits {
					pc.Resources = append(pc.Resources, fmt.Sprintf("limits/%s=%s", name, qty.String()))
				}
				for name, qty := range c.Resources.Requests {
					pc.Resources = append(pc.Resources, fmt.Sprintf("requests/%s=%s", name, qty.String()))
				}
				sort.Strings(pc.Resources)
				for _, e := range c.Env {
					pc.Env = append(pc.Env, e.Name+"="+e.Value)
				}
				projected.Containers = append(projected.Containers, pc)
			}
			out.ReplicatedJobs = append(out.ReplicatedJobs, projected)
		}
	}
	return out, nil
}

// mlPolicyKeys returns the sorted names of the policies set on an MLPolicy.
func mlPolicyKeys(p *trainerv1alpha1.MLPolicy) []string {
	keys := []string{}
	if p == nil {
		return keys
	}
	if p.Torch != nil {
		keys = append(keys, "torch")
	}
	if p.MPI != nil {
		keys = append(keys, "mpi")
	}
	sort.Strings(keys)
	return keys
}
