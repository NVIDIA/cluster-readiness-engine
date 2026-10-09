// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const workloadRunEnvTestImage = "nvcr.io/nvidia/pytorch:24.01-py3"

func TestWorkloadRunControllerCarriesUserEnvIntoMatchingPlatformOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	node := &corev1.Node{
		Name: "gcp-h100-0",
		Labels: map[string]string{
			GPUNodeLabel:        present,
			testGPUProductLabel: "NVIDIA-H100-80GB-HBM3",
		},
		Spec: corev1.NodeSpec{ProviderID: "gce://project/us-central1-a/gcp-h100-0"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "env-precedence", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkloadRunSpec{
			Image:    workloadRunEnvTestImage,
			NumNodes: 1,
			Framework: nvcrev1alpha1.FrameworkSpec{
				Torch: &nvcrev1alpha1.TorchFramework{Script: "/workspace/train.py"},
			},
			Env: []corev1.EnvVar{
				{Name: "NCCL_DEBUG", Value: "TRACE"},
				{Name: "USER_ONLY", Value: "kept-on-runtime"},
				{Name: "PET_NNODES", Value: "999"},
			},
		},
	}

	r := &WorkloadRunReconciler{Client: c, Scheme: scheme}
	ws, err := r.buildWorkflowSpec(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	_, err = applyOverridesWithTracking(ws, OverrideContext{
		Platform:        "gcp",
		GPUArchitecture: "h100",
	})
	if err != nil {
		t.Fatal(err)
	}

	env := ws.JobTemplate.Spec.Workload.TrainJob.Trainer.Env
	assertTrainerEnvValue(t, env, "NCCL_DEBUG", "TRACE")
	assertNoTrainerEnvValue(t, env, "USER_ONLY")
	assertNoTrainerEnvValue(t, env, "PET_NNODES")
}

func TestWorkloadRunControllerCarriesUserEnvIntoGCPGB200RuntimeDependency(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	node := &corev1.Node{
		Name: "gcp-gb200-0",
		Labels: map[string]string{
			GPUNodeLabel:        present,
			testGPUProductLabel: "NVIDIA-GB200-NVL72",
		},
		Spec: corev1.NodeSpec{ProviderID: "gce://project/us-central1-a/gcp-gb200-0"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "env-precedence", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkloadRunSpec{
			Image:    workloadRunEnvTestImage,
			NumNodes: 1,
			Framework: nvcrev1alpha1.FrameworkSpec{
				Torch: &nvcrev1alpha1.TorchFramework{Script: "/workspace/train.py"},
			},
			Env: []corev1.EnvVar{
				{Name: "NCCL_DEBUG", Value: "TRACE"},
				{Name: "USER_ONLY", Value: "kept-on-runtime"},
			},
		},
	}

	r := &WorkloadRunReconciler{Client: c, Scheme: scheme}
	ws, err := r.buildWorkflowSpec(context.Background(), run)
	require.NoError(t, err)
	_, err = applyOverridesWithTracking(ws, OverrideContext{
		Platform:        "gcp",
		GPUArchitecture: "gb200",
	})
	require.NoError(t, err)
	require.NotEmpty(t, ws.Dependencies)

	var root map[string]any
	require.NoError(t, json.Unmarshal(ws.Dependencies[0].Raw, &root))
	runtimeSpec, ok := nestedWorkloadRunMap(root, "spec", "template", "spec")
	require.True(t, ok)
	replicatedJobs, ok := runtimeSpec["replicatedJobs"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, replicatedJobs)
	job, ok := replicatedJobs[0].(map[string]any)
	require.True(t, ok)
	podSpec, ok := nestedWorkloadRunMap(job, "template", "spec", "template", "spec")
	require.True(t, ok)
	containers, ok := podSpec["containers"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, containers)
	container, ok := containers[0].(map[string]any)
	require.True(t, ok)
	rawEnv, ok := container["env"]
	require.True(t, ok)
	envJSON, err := json.Marshal(rawEnv)
	require.NoError(t, err)
	var env []corev1.EnvVar
	require.NoError(t, json.Unmarshal(envJSON, &env))
	assertTrainerEnvValue(t, env, "NCCL_DEBUG", "TRACE")
	assertTrainerEnvValue(t, env, "USER_ONLY", "kept-on-runtime")
}

// MPI ranks start under sshd with a fresh environment, so the launcher's -x
// args alone decide what they see: spec.env must win over both the Nscale IB
// env the platform forwards (NCCL_IB_PCI_RELAXED_ORDERING=1) and the NCCL
// defaults (NCCL_SHM_DISABLE=1), and a name both set is forwarded once.
func TestWorkloadRunControllerForwardsUserEnvToMPIRanks(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	node := &corev1.Node{
		Name: "nscale-b200-0",
		Labels: map[string]string{
			GPUNodeLabel:        present,
			testGPUProductLabel: "NVIDIA-B200",
		},
		Spec: corev1.NodeSpec{ProviderID: "nscale://nscale-b200-0"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "env-mpi-ranks", Namespace: testNS,
		Spec: nvcrev1alpha1.WorkloadRunSpec{
			Image:    workloadRunEnvTestImage,
			NumNodes: 1,
			Framework: nvcrev1alpha1.FrameworkSpec{
				MPI: &nvcrev1alpha1.MPIFramework{
					Binary:     "/usr/local/bin/all_reduce_perf_mpi",
					MpirunPath: "/usr/local/mpi/bin/mpirun",
				},
			},
			Env: []corev1.EnvVar{
				{Name: "NCCL_IB_PCI_RELAXED_ORDERING", Value: "0"},
				{Name: "NCCL_SHM_DISABLE", Value: "0"},
			},
		},
	}

	r := &WorkloadRunReconciler{Client: c, Scheme: scheme}
	ws, err := r.buildWorkflowSpec(context.Background(), run)
	require.NoError(t, err)

	args := ws.JobTemplate.Spec.Workload.TrainJob.Trainer.Args
	require.Equal(t, []string{"0"}, mpiEnvArgValues(args, "NCCL_IB_PCI_RELAXED_ORDERING"))
	require.Equal(t, []string{"0"}, mpiEnvArgValues(args, "NCCL_SHM_DISABLE"))
	require.Equal(t, []string{"INFO"}, mpiEnvArgValues(args, "NCCL_DEBUG"))
	require.Equal(t, []string{"0"}, mpiEnvArgValues(args, "NCCL_MNNVL_ENABLE"))
}

// mpiEnvArgValues returns the value of every -x NAME=value operand for name.
func mpiEnvArgValues(args []string, name string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-x" {
			continue
		}
		if value, ok := strings.CutPrefix(args[i+1], name+"="); ok {
			values = append(values, value)
		}
	}
	return values
}

func nestedWorkloadRunMap(root map[string]any, path ...string) (map[string]any, bool) {
	current := root
	for _, key := range path {
		value, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func assertNoTrainerEnvValue(t *testing.T, env []corev1.EnvVar, name string) {
	t.Helper()
	for _, got := range env {
		if got.Name == name {
			t.Fatalf("did not expect %s in trainer env: %#v", name, env)
		}
	}
}

func assertTrainerEnvValue(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()
	for _, got := range env {
		if got.Name == name {
			require.Equal(t, want, got.Value)
			return
		}
	}
	t.Fatalf("expected %s=%s in trainer env: %#v", name, want, env)
}
