// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Render through the CLI's resolution path, so provider detection, platform
// matching, MPI exports, and the TrainingRuntime GPU requests are checked together.
func TestAkamaiTCPRendering(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "akamai-tcp", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Platform      string `json:"platform"`
			Domain        string `json:"domain"`
			Variant       string `json:"variant"`
			GpusPerNode   int32  `json:"gpusPerNode"`
			NodesPerJob   int32  `json:"nodesPerJob"`
			MaxBytes      string `json:"maxBytes"`
			NumIterations *int32 `json:"numIterations"`
			NumCycles     *int32 `json:"numCycles"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		cert := &nvcrev1alpha1.Certification{Name: "tcp", Spec: nvcrev1alpha1.CertificationSpec{
			Target: nvcrev1alpha1.TargetSpec{NodeSelector: map[string]string{
				gpu.ProductLabel: "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition",
			}},
			Categories: []nvcrev1alpha1.CertificateCategory{{Domain: input.Domain, Variant: input.Variant}},
			CategoryOptions: nvcrev1alpha1.CategoryOptions{
				GpusPerNode: &input.GpusPerNode, NodesPerJob: &input.NodesPerJob,
				MaxBytes: input.MaxBytes, NumIterations: input.NumIterations, NumCycles: input.NumCycles,
			},
		}}
		workflows, err := renderCertification(cert, input.Platform, "rtxpro6000", nil, "", catalog.DefaultOCISecondaryNetwork)
		if err != nil {
			return err
		}
		if err := resolveWorkflowsOffline(cert, workflows, input.Platform, "rtxpro6000"); err != nil {
			return err
		}
		if len(workflows) != 1 {
			return fmt.Errorf("rendered %d workflows, want one", len(workflows))
		}
		var result struct {
			NCCLEnv           map[string]string `json:"ncclEnv"`
			GPURequests       string            `json:"gpuRequests"`
			GPULimits         string            `json:"gpuLimits"`
			ExtendedResources map[string]string `json:"extendedResources"`
			Executable        string            `json:"executable,omitempty"`
			PerfArgs          []string          `json:"perfArgs,omitempty"`
		}
		result.NCCLEnv = map[string]string{}
		result.ExtendedResources = map[string]string{}
		capture := func(name, value string) {
			recordAkamaiTransportEnv(result.NCCLEnv, name, value)
		}
		wf := &workflows[0]
		for _, dep := range wf.Spec.Dependencies {
			var meta struct{ Kind string }
			if err := json.Unmarshal(dep.Raw, &meta); err != nil {
				return err
			}
			if meta.Kind != trainerv1alpha1.TrainingRuntimeKind {
				continue
			}
			var rt trainerv1alpha1.TrainingRuntime
			if err := json.Unmarshal(dep.Raw, &rt); err != nil {
				return err
			}
			for _, job := range rt.Spec.Template.Spec.ReplicatedJobs {
				if job.Name != "node" {
					continue
				}
				for _, container := range job.Template.Spec.Template.Spec.Containers {
					for _, env := range container.Env {
						capture(env.Name, env.Value)
					}
					if q, ok := container.Resources.Requests["nvidia.com/gpu"]; ok {
						result.GPURequests = q.String()
					}
					if q, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {
						result.GPULimits = q.String()
					}
					for name, quantity := range container.Resources.Requests {
						if strings.Contains(string(name), "/") && name != "nvidia.com/gpu" {
							result.ExtendedResources[string(name)] = quantity.String()
						}
					}
				}
			}
		}
		if result.GPURequests == "" || result.GPULimits == "" {
			return fmt.Errorf("rendered runtime has no worker GPU request or limit")
		}
		job := wf.Spec.JobTemplate.Spec.Workload.TrainJob
		if job == nil || job.Trainer == nil {
			return fmt.Errorf("rendered workflow has no trainer")
		}
		for _, env := range job.Trainer.Env {
			capture(env.Name, env.Value)
		}
		for i, arg := range job.Trainer.Args {
			if arg == "-x" && i+1 < len(job.Trainer.Args) {
				if name, value, ok := strings.Cut(job.Trainer.Args[i+1], "="); ok {
					capture(name, value)
				}
			}
			if strings.HasSuffix(arg, "_perf_mpi") {
				result.Executable = arg
				if input.MaxBytes != "" {
					result.PerfArgs = job.Trainer.Args[i+1:]
				}
			}
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// recordAkamaiTransportEnv keeps the projection focused on transport selection.
func recordAkamaiTransportEnv(values map[string]string, name, value string) {
	switch name {
	case "NCCL_DEBUG", "NCCL_IB_DISABLE", "NCCL_NET_PLUGIN", "NCCL_SOCKET_IFNAME", "NCCL_P2P_LEVEL", "NCCL_P2P_DISABLE":
		values[name] = value
	}
}
