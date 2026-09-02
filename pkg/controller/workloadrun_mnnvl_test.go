// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The MPI launcher forwards NCCL_MNNVL_ENABLE to the workers with -x, while
// the worker runtime container gets the same variable through
// BaseNCCLEnvVars. Both must come from a single resolution: the architecture
// default (GB200/GB300 -> 1, everything else -> 0) overridden by
// spec.enableMNNVL when set. The launcher used to read spec.enableMNNVL
// alone, so an unset field on GB300 forwarded 0 to the workers while the
// runtime env said 1 (issue #199). These cases pin the launcher args and the
// runtime env against each other so the two can never disagree again.
func TestWorkloadRunMPILauncherMNNVL(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-mpi-launcher-mnnvl",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			GPUProduct  string `yaml:"gpuProduct"`
			EnableMNNVL *bool  `yaml:"enableMNNVL"`
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

		node := &corev1.Node{
			Name: "node-0",
			Labels: map[string]string{
				GPUNodeLabel:        present,
				testGPUProductLabel: in.GPUProduct,
			},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()

		run := &nvcrev1alpha1.WorkloadRun{
			Name: "mnnvl-run", Namespace: testNS,
			Spec: nvcrev1alpha1.WorkloadRunSpec{
				Image:       "nvcr.io/nvidia/pytorch:24.01-py3",
				NumNodes:    2,
				EnableMNNVL: in.EnableMNNVL,
				Framework: nvcrev1alpha1.FrameworkSpec{
					MPI: &nvcrev1alpha1.MPIFramework{
						Binary:     "/usr/local/bin/all_reduce_perf_mpi",
						Args:       []string{"-b", "8", "-e", "32G"},
						MpirunPath: "/usr/local/mpi/bin/mpirun",
					},
				},
			},
		}

		r := &WorkloadRunReconciler{Client: c, Scheme: scheme}
		ws := r.buildWorkflowSpec(context.Background(), run)

		trainer := ws.JobTemplate.Spec.Workload.TrainJob.Trainer

		// Everywhere else in the WorkflowSpec (platform overrides, runtime
		// dependencies) NCCL_MNNVL_ENABLE is emitted from the same resolved
		// value the launcher must forward. Walk the whole marshaled spec for
		// every {"name": "NCCL_MNNVL_ENABLE", "value": ...} env entry so the
		// golden file proves the launcher and the env agree.
		raw, err := json.Marshal(ws)
		if err != nil {
			return err
		}
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return err
		}
		envVals := collectEnvValues(tree, "NCCL_MNNVL_ENABLE")

		out := struct {
			LauncherCommand []string `json:"launcherCommand"`
			LauncherArgs    []string `json:"launcherArgs"`
			EnvMNNVL        []string `json:"envMNNVL"`
		}{trainer.Command, trainer.Args, envVals}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// collectEnvValues walks an unmarshalled JSON tree and returns the sorted,
// de-duplicated values of every {"name": name, "value": ...} env entry.
func collectEnvValues(v any, name string) []string {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if got, ok := t["name"].(string); ok && got == name {
				if val, ok := t["value"].(string); ok {
					seen[val] = true
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	vals := make([]string, 0, len(seen))
	for val := range seen {
		vals = append(vals, val)
	}
	sort.Strings(vals)
	return vals
}
