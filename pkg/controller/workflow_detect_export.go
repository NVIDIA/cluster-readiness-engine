// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// DetectPlatform is the exported version of detectPlatform for use by CLI tools.
func DetectPlatform(nodes []corev1.Node) string {
	return detectPlatform(nodes)
}

// DetectGPUArchitecture is the exported version of detectGPUArchitecture for
// use by CLI tools. Like the controllers, it reports the architecture that
// the most labeled nodes carry (gpu.MajorityArchitecture), so render, cluster
// info, and workloadrun previews agree with what a reconcile of the same
// target would detect.
func DetectGPUArchitecture(nodes []corev1.Node) string {
	return detectGPUArchitecture(nodes)
}

// ResolveNICResourceName is the exported version of resolveNICResourceName
// for use by the CLI dry-run paths, so "certification render --dry-run" and
// "workloadrun render --dry-run" resolve the NIC resource exactly as a
// reconcile of the same target would: field wins, detection only for on-prem
// GB200/GB300, and only a single candidate allocatable at the resolved
// mlnxPerNode count on every node is used. mlnxPerNode must be the caller's
// fully resolved value (field or catalog default).
//
// refusalMessage is non-empty exactly when detection ran and refused to pick
// (name == ""); it is the same text the controllers emit as the
// NICResourceDetection event, for the CLI to print to stderr.
func ResolveNICResourceName(
	field *string, platformName, gpuArch string, nodes []corev1.Node, mlnxPerNode int32,
) (name, refusalMessage string, detectionRan bool) {
	d := resolveNICResourceName(field, platformName, gpuArch, nodes, mlnxPerNode)
	if d.Ran && d.Name == "" {
		refusalMessage = nicDetectionMessage(d)
	}
	return d.Name, refusalMessage, d.Ran
}

// ResolveGKETCPXONetworks is the exported version of resolveGKETCPXONetworks
// for use by the CLI dry-run path, so "certification render --dry-run"
// attaches the same GKE networks a reconcile of the same target would:
// detection only for GCP H100, and only exactly GKETCPXONICsPerNode networks
// allocatable on every node are used.
//
// names is empty when detection did not run or refused, and the catalog
// default is rendered. refusalMessage is non-empty exactly when detection ran
// and refused; it is the same text the controller emits as the
// GKENetworkDetection event, for the CLI to print to stderr.
func ResolveGKETCPXONetworks(
	platformName, gpuArch string, nodes []corev1.Node,
) (names []string, refusalMessage string, detectionRan bool) {
	d := resolveGKETCPXONetworks(platformName, gpuArch, nodes)
	if d.Ran && len(d.Names) == 0 {
		refusalMessage = gkeNetworkDetectionMessage(d)
	}
	return d.Names, refusalMessage, d.Ran
}

// ResolveTCPXOPluginVersion is the exported version of
// resolveTCPXOPluginVersion for use by the CLI dry-run path, so
// "certification render --dry-run" renders the same GCP H100 images a
// reconcile of the same target would: detection only for GCP H100, and only
// one plugin release tag on every node is used. reader must be able to list
// kube-system pods.
//
// version is the release every target node runs, mapped or not, for
// catalog.BuildConfig.TCPXOPluginVersion; empty when detection did not run or
// found none. fallbackMessage is non-empty exactly when detection ran and
// found no mapped release, so a fallback profile renders; it is the same text
// the controller emits as the TCPXOPluginDetection event, for the CLI to
// print to stderr.
func ResolveTCPXOPluginVersion(
	ctx context.Context, reader client.Reader, platformName, gpuArch string, nodes []corev1.Node,
) (version, fallbackMessage string, detectionRan bool) {
	d := resolveTCPXOPluginVersion(ctx, reader, nil, platformName, gpuArch, nodes)
	if d.Ran && !d.Exact {
		fallbackMessage = tcpxoPluginDetectionMessage(d)
	}
	return d.Version, fallbackMessage, d.Ran
}

// BuildOverrideContext is the exported version of buildOverrideContext for use by CLI tools.
func BuildOverrideContext(spec *nvcrev1alpha1.WorkflowSpec, orch *nvcrev1alpha1.OrchestrationStatus, nodes []corev1.Node) OverrideContext {
	return buildOverrideContext(spec, orch, nodes)
}

// ApplyOverridesWithTracking is the exported version of applyOverridesWithTracking for use by CLI tools.
func ApplyOverridesWithTracking(spec *nvcrev1alpha1.WorkflowSpec, octx OverrideContext) ([]nvcrev1alpha1.AppliedOverride, error) {
	return applyOverridesWithTracking(spec, octx)
}

// ApplyWRPreTemplateOverrides is the exported version of
// applyWRPreTemplateOverrides for use by CLI tools, so the "workloadrun
// render" preview bakes the same platform mpirun args into the spec that the
// controller bakes at reconcile time.
func ApplyWRPreTemplateOverrides(spec *nvcrev1alpha1.WorkloadRunSpec, overrides []platform.WorkloadRunOverride, octx OverrideContext) {
	applyWRPreTemplateOverrides(spec, overrides, octx)
}

// DiscoverTargetNodes is the exported version of discoverTargetNodes for use by CLI tools.
//
// It drops the cordoned-node list that discoverTargetNodes also returns. Only
// the Workflow reconciler records coverage on status; CLI callers want the
// nodes a run would actually use. Widen this if a CLI ever needs to report what
// was skipped.
//
// reader also serves the ResourceSlice fallback List, which is safe because CLI
// clients are uncached client.New clients.
func DiscoverTargetNodes(ctx context.Context, reader client.Reader, target *nvcrev1alpha1.TargetSpec) ([]corev1.Node, error) {
	nodes, _, _, err := discoverTargetNodes(ctx, reader, reader, target)
	return nodes, err
}

// DiscoverTargetNodesWithSynthesized is DiscoverTargetNodes plus the names of
// the nodes whose nvidia.com/gpu.product label was synthesized from
// ResourceSlice attributes rather than read off the stored Node.
//
// render needs the distinction for the same reason the reconciler does: a
// synthesized label exists only on the returned copies, so an affinity term
// built from it matches nothing on a real API server.
func DiscoverTargetNodesWithSynthesized(
	ctx context.Context, reader client.Reader, target *nvcrev1alpha1.TargetSpec,
) (nodes []corev1.Node, synthesizedProducts []string, err error) {
	nodes, _, synthesizedProducts, err = discoverTargetNodes(ctx, reader, reader, target)
	return nodes, synthesizedProducts, err
}
