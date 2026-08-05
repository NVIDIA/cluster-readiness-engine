// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
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
func BuildOverrideContext(spec *burninv1alpha1.WorkflowSpec, orch *burninv1alpha1.OrchestrationStatus, nodes []corev1.Node) OverrideContext {
	return buildOverrideContext(spec, orch, nodes)
}

// ApplyOverridesWithTracking is the exported version of applyOverridesWithTracking for use by CLI tools.
func ApplyOverridesWithTracking(spec *burninv1alpha1.WorkflowSpec, octx OverrideContext) ([]burninv1alpha1.AppliedOverride, error) {
	return applyOverridesWithTracking(spec, octx)
}

// DiscoverTargetNodes is the exported version of discoverTargetNodes for use by CLI tools.
func DiscoverTargetNodes(ctx context.Context, reader client.Reader, target *burninv1alpha1.TargetSpec) ([]corev1.Node, error) {
	return discoverTargetNodes(ctx, reader, target)
}
