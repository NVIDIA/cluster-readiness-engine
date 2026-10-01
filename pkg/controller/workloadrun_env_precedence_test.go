// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

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
			Image:    "nvcr.io/nvidia/pytorch:24.01-py3",
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
