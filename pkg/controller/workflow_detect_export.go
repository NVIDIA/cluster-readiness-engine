// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// DetectPlatform is the exported version of detectPlatform for use by CLI tools.
func DetectPlatform(nodes []corev1.Node) string {
	return detectPlatform(nodes)
}

// DetectGPUArchitecture is the exported version of detectGPUArchitecture for use by CLI tools.
func DetectGPUArchitecture(nodes []corev1.Node) string {
	return detectGPUArchitecture(nodes)
}

// BuildOverrideContext is the exported version of buildOverrideContext for use by CLI tools.
func BuildOverrideContext(spec *crev1alpha1.WorkflowSpec, orch *crev1alpha1.OrchestrationStatus, nodes []corev1.Node) OverrideContext {
	return buildOverrideContext(spec, orch, nodes)
}

// ApplyOverridesWithTracking is the exported version of applyOverridesWithTracking for use by CLI tools.
func ApplyOverridesWithTracking(spec *crev1alpha1.WorkflowSpec, octx OverrideContext) ([]crev1alpha1.AppliedOverride, error) {
	return applyOverridesWithTracking(spec, octx)
}

// ApplyWRPreTemplateOverrides is the exported version of
// applyWRPreTemplateOverrides for use by CLI tools, so the "workloadrun
// render" preview bakes the same platform mpirun args into the spec that the
// controller bakes at reconcile time.
func ApplyWRPreTemplateOverrides(spec *crev1alpha1.WorkloadRunSpec, overrides []platform.WorkloadRunOverride, octx OverrideContext) {
	applyWRPreTemplateOverrides(spec, overrides, octx)
}

// DiscoverTargetNodes is the exported version of discoverTargetNodes for use by CLI tools.
//
// It drops the cordoned-node list that discoverTargetNodes also returns. Only
// the Workflow reconciler records coverage on status; CLI callers want the
// nodes a run would actually use. Widen this if a CLI ever needs to report what
// was skipped.
func DiscoverTargetNodes(ctx context.Context, reader client.Reader, target *crev1alpha1.TargetSpec) ([]corev1.Node, error) {
	nodes, _, err := discoverTargetNodes(ctx, reader, target)
	return nodes, err
}
