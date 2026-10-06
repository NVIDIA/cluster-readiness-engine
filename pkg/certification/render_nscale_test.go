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
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// nscaleClaimTemplate records the one DeviceRequest a nscale-gpu-rdma.yaml
// ResourceClaimTemplate carries: its deviceClassName and requested count, so
// a golden shows the gpu.nvidia.com/rdma.nscale.com claims directly rather
// than by name alone.
type nscaleClaimTemplate struct {
	Name            string `json:"name"`
	DeviceClassName string `json:"deviceClassName"`
	Count           int64  `json:"count"`
}

type nscaleContainer struct {
	Name string `json:"name"`
	// Resources renders limits and requests as sorted "limits/<name>=<qty>"
	// and "requests/<name>=<qty>" strings. nvidia.com/gpu must be absent on
	// nscale: the override nulls out the base entry's request. On the
	// training entries cpu/memory must survive that null.
	Resources []string `json:"resources"`
	// Claims is resources.claims in declaration order, where the gpu/rdma
	// DRA claims and (on GB300) the ComputeDomain channel claim coexist.
	Claims []string `json:"claims"`
	// Env is the runtime container env ("NAME=value") in declaration order.
	// The training entries carry the portable IB env here (merged by name
	// into the base env) because torchrun inherits the container env.
	Env []string `json:"env"`
}

// nscaleOrchestration pins the topology key an Nscale GB300 override must
// replace: Nscale runs no GFD, so nvidia.com/gpu.clique never exists there
// and the NVLink domain is topology.nks.nscale.com/accelerator-domain. The
// diagnose override replaces the whole diagnose object, so it must repeat
// the base minGroupSize.
type nscaleOrchestration struct {
	TopologyKey          string `json:"topologyKey"`
	StrictDomain         bool   `json:"strictDomain"`
	DiagnoseTopologyKey  string `json:"diagnoseTopologyKey"`
	DiagnoseMinGroupSize int    `json:"diagnoseMinGroupSize"`
}

type nscaleReplicatedJob struct {
	Dependency string `json:"dependency"`
	// ResourceClaims is pod-spec-level "name=resourceClaimTemplateName", in
	// declaration order.
	ResourceClaims []string          `json:"resourceClaims"`
	Tolerations    []string          `json:"tolerations"`
	Containers     []nscaleContainer `json:"containers"`
}

// nscaleWorkflow is the per-Workflow projection written to the golden file.
type nscaleWorkflow struct {
	Workflow string `json:"workflow"`
	// DependencyKinds is every dependency's kind/name, in declaration order,
	// so the golden shows the gpu.nvidia.com/rdma.nscale.com
	// ResourceClaimTemplates and (on GB300) the ComputeDomain coexisting.
	DependencyKinds []string `json:"dependencyKinds"`
	// ClaimTemplates is every ResourceClaimTemplate dependency's device
	// request, keyed by dependency name.
	ClaimTemplates []nscaleClaimTemplate `json:"claimTemplates"`
	// TrainerArgs is the resolved jobTemplate trainer args, where the
	// nscale-ib-env.yaml vars ride as -x pairs and NCCL_IB_HCA/
	// UCX_NET_DEVICES must NOT appear.
	TrainerArgs []string `json:"trainerArgs"`
	// TrainerEnv is the resolved jobTemplate trainer env ("NAME=value"),
	// where the loopback variants carry the replaced env list.
	TrainerEnv     []string              `json:"trainerEnv"`
	Orchestration  nscaleOrchestration   `json:"orchestration"`
	ReplicatedJobs []nscaleReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderNScale covers the ADR-085 Nscale NKS override end to
// end through the same path "nvcrectl certification render --platform nscale
// --gpu-arch <arch>" uses, for every catalog entry. The goldens pin the
// markers the override owns: the gpu.nvidia.com/rdma.nscale.com
// ResourceClaimTemplates (GPU-only on dcgm-level4), the absence of
// nvidia.com/gpu from the rendered container resources, the portable IB env
// in trainer args (MPI collectives), trainer env (loopbacks) or the runtime
// container env (training), the absence of pinned HCA names, the retained
// tolerate-everything toleration on the per-node entries, the
// accelerator-domain topology key on GB300 (and gpu.clique left alone on
// B200), and — on GB300 — that the Nscale claims and the GB200/GB300
// ComputeDomain block's claim coexist without a name collision.
func TestCertificationRenderNScale(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-nscale",
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
		workflows, err := renderCertification(cert, cfg.Platform, cfg.GPUArch, nil, "")
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform, cfg.GPUArch); err != nil {
			return err
		}

		result := make([]nscaleWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectNScaleOverride(&workflows[i])
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

// projectNScaleOverride walks a resolved Workflow in declaration order:
// dependencies as the catalog lists them, then replicatedJobs as the runtime
// lists them. Only the resource strings are sorted (map iteration order), so
// the output is stable without hiding a reordering.
func projectNScaleOverride(wf *nvcrev1alpha1.Workflow) (nscaleWorkflow, error) {
	out := nscaleWorkflow{
		Workflow:        wf.Name,
		DependencyKinds: []string{},
		ClaimTemplates:  []nscaleClaimTemplate{},
		TrainerArgs:     []string{},
		TrainerEnv:      []string{},
		ReplicatedJobs:  []nscaleReplicatedJob{},
	}

	if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
		out.TrainerArgs = append(out.TrainerArgs, tj.Trainer.Args...)
		for _, e := range tj.Trainer.Env {
			out.TrainerEnv = append(out.TrainerEnv, e.Name+"="+e.Value)
		}
	}
	if topo := wf.Spec.Orchestration.Topology; topo != nil {
		out.Orchestration.TopologyKey = topo.TopologyKey
		out.Orchestration.StrictDomain = topo.StrictDomain
	}
	if diag := wf.Spec.Orchestration.Diagnose; diag != nil {
		out.Orchestration.DiagnoseTopologyKey = diag.TopologyKey
		out.Orchestration.DiagnoseMinGroupSize = diag.MinGroupSize
	}

	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			out.DependencyKinds = append(out.DependencyKinds, "")
			continue
		}

		var typeMeta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		out.DependencyKinds = append(out.DependencyKinds, typeMeta.Kind+"/"+typeMeta.Metadata.Name)

		switch typeMeta.Kind {
		case "ResourceClaimTemplate":
			var rct resourcev1.ResourceClaimTemplate
			if err := json.Unmarshal(raw, &rct); err != nil {
				return out, err
			}
			for _, req := range rct.Spec.Spec.Devices.Requests {
				if req.Exactly == nil {
					continue
				}
				out.ClaimTemplates = append(out.ClaimTemplates, nscaleClaimTemplate{
					Name:            rct.Name,
					DeviceClassName: req.Exactly.DeviceClassName,
					Count:           req.Exactly.Count,
				})
			}
		case trainerv1alpha1.TrainingRuntimeKind:
			var rt trainerv1alpha1.TrainingRuntime
			if err := json.Unmarshal(raw, &rt); err != nil {
				return out, err
			}
			for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
				podSpec := rj.Template.Spec.Template.Spec
				projected := nscaleReplicatedJob{
					Dependency:     rt.Name,
					ResourceClaims: []string{},
					Tolerations:    []string{},
					Containers:     []nscaleContainer{},
				}
				for _, rc := range podSpec.ResourceClaims {
					templateName := ""
					if rc.ResourceClaimTemplateName != nil {
						templateName = *rc.ResourceClaimTemplateName
					}
					projected.ResourceClaims = append(projected.ResourceClaims,
						fmt.Sprintf("%s=%s", rc.Name, templateName))
				}
				for _, tol := range podSpec.Tolerations {
					if tol.Key == "" && tol.Operator == corev1.TolerationOpExists {
						// The per-node entries' tolerate-everything toleration,
						// which the Nscale blocks must leave in place.
						projected.Tolerations = append(projected.Tolerations, "operator=Exists")
						continue
					}
					projected.Tolerations = append(projected.Tolerations,
						fmt.Sprintf("%s=%s:%s", tol.Key, tol.Value, tol.Effect))
				}
				for _, c := range podSpec.Containers {
					pc := nscaleContainer{Name: c.Name, Resources: []string{}, Claims: []string{}, Env: []string{}}
					for _, e := range c.Env {
						pc.Env = append(pc.Env, e.Name+"="+e.Value)
					}
					for name, qty := range c.Resources.Limits {
						pc.Resources = append(pc.Resources, fmt.Sprintf("limits/%s=%s", name, qty.String()))
					}
					for name, qty := range c.Resources.Requests {
						pc.Resources = append(pc.Resources, fmt.Sprintf("requests/%s=%s", name, qty.String()))
					}
					sort.Strings(pc.Resources)
					for _, claim := range c.Resources.Claims {
						pc.Claims = append(pc.Claims, claim.Name)
					}
					projected.Containers = append(projected.Containers, pc)
				}
				out.ReplicatedJobs = append(out.ReplicatedJobs, projected)
			}
		}
	}
	return out, nil
}
