// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
)

// The NVIDIA DRA GPU driver (kubernetes-sigs/dra-driver-nvidia-gpu,
// GpuInfo.Attributes in cmd/gpu-kubelet-plugin/deviceinfo.go) publishes
// node-local ResourceSlices (spec.driver gpu.nvidia.com, spec.nodeName set)
// whose GPU devices carry an unqualified productName string attribute holding
// the NVML device name, e.g. "NVIDIA GB300". Renaming the driver or an
// attribute, or qualifying a key as gpu.nvidia.com/<name>, silently disables
// these fallbacks.
//
// Each device also carries a type attribute: gpu, mig, or vfio (v0.5.0).
// gpu devices and mig devices (CommonAttributesMig, from the parent GPU) carry
// the NVML name, but vfio passthrough devices (VfioDeviceInfo.GetDevice) carry
// the go-nvlib nvpci DeviceName PCI-IDs name, e.g. "GH100 [H100 SXM5 80GB]",
// which parses to the wrong architecture. vfio devices are skipped; any other
// or missing type is read. Only gpu devices are whole GPUs, so CountDRAGPUs
// counts those alone.
const (
	gpuResourceSliceDriver = "gpu.nvidia.com"
	productNameAttribute   = "productName"
	deviceTypeAttribute    = "type"
	fullGPUDeviceType      = "gpu"
	vfioDeviceType         = "vfio"

	// resourceSliceListTimeout bounds each uncached ResourceSlice List
	// against a slow API server. A 403 or an unserved resource.k8s.io/v1
	// fails immediately without it.
	resourceSliceListTimeout = 5 * time.Second
)

// augmentGPUProductLabels sets nvidia.com/gpu.product on any node in nodes
// that lacks it, from its gpu.nvidia.com ResourceSlice productName device
// attribute (DRA-only platforms run no device plugin/GFD DaemonSet to write
// gpu.product; architecture lives in ResourceSlice attributes instead).
// The NVML name is sanitized as GFD sanitizes it (gpu.ProductLabelValue): the
// same hardware must yield a label byte-identical to an unshared GFD label in
// a mixed GFD/DRA fleet, since UniformGPUProduct compares raw values. Against a
// time-sliced GFD label ("-SHARED" suffix) UniformGPUProduct reports
// heterogeneous; architecture detection is unaffected.
// Mutates the caller's in-memory Node copies only — never persisted back to
// the API server. Every existing label-based consumer (detectGPUArchConsistent,
// DetectGPUArchitecture, UniformGPUProduct, catalog.GPUArchFromNodeSelector,
// NIC detection) keeps working completely unchanged, because all of them only
// ever read the label. One ResourceSlice
// List call total, skipped entirely when every node already carries the
// label (every non-DRA cluster).
//
// Returns the names of the nodes whose label it synthesized. Reading the label
// is safe for every consumer above, but matching on it is not: a synthesized
// value exists only on these in-memory copies, so a node affinity built from it
// matches nothing on the API server and leaves every pod Pending. Callers that
// turn labels into scheduling constraints must consult this list first.
func augmentGPUProductLabels(ctx context.Context, reader client.Reader, nodes []corev1.Node) []string {
	needsLookup := false
	for i := range nodes {
		if nodes[i].Labels[gpu.ProductLabel] == "" {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return nil
	}

	listCtx, cancel := context.WithTimeout(ctx, resourceSliceListTimeout)
	defer cancel()

	var slices resourcev1.ResourceSliceList
	if err := reader.List(listCtx, &slices); err != nil {
		logf.FromContext(ctx).Info("list gpu.nvidia.com resourceslices for architecture fallback failed",
			"error", err)
		return nil
	}

	productByNode := make(map[string]string, len(nodes))
	for _, rs := range slices.Items {
		if rs.Spec.Driver != gpuResourceSliceDriver || rs.Spec.NodeName == nil {
			continue
		}
		for _, d := range rs.Spec.Devices {
			if t := d.Attributes[deviceTypeAttribute].StringValue; t != nil && *t == vfioDeviceType {
				continue
			}
			if attr, ok := d.Attributes[productNameAttribute]; ok && attr.StringValue != nil {
				productByNode[*rs.Spec.NodeName] = gpu.ProductLabelValue(*attr.StringValue)
				break
			}
		}
	}

	var synthesized []string
	for i := range nodes {
		if nodes[i].Labels[gpu.ProductLabel] != "" {
			continue
		}
		if product, ok := productByNode[nodes[i].Name]; ok {
			if nodes[i].Labels == nil {
				nodes[i].Labels = map[string]string{}
			}
			nodes[i].Labels[gpu.ProductLabel] = product
			synthesized = append(synthesized, nodes[i].Name)
		}
	}
	return synthesized
}

// CountDRAGPUs returns the number of full GPUs each node publishes in its
// gpu.nvidia.com ResourceSlices, keyed by node name. MIG and VFIO devices are
// excluded because they are not whole GPUs. Only the newest generation of
// each pool counts, since older slices of a pool are stale by the
// ResourcePool contract.
//
// It serves the CLI only, as an observation to compare against the catalog
// default; rendering still sizes claims from gpu-defaults.yaml. No controller
// calls it, so it has no unexported twin in workflow_detect_export.go.
func CountDRAGPUs(ctx context.Context, reader client.Reader) (map[string]int32, error) {
	listCtx, cancel := context.WithTimeout(ctx, resourceSliceListTimeout)
	defer cancel()

	var slices resourcev1.ResourceSliceList
	if err := reader.List(listCtx, &slices); err != nil {
		return nil, err
	}

	newest := map[string]int64{}
	for _, rs := range slices.Items {
		if rs.Spec.Driver != gpuResourceSliceDriver {
			continue
		}
		if g, ok := newest[rs.Spec.Pool.Name]; !ok || rs.Spec.Pool.Generation > g {
			newest[rs.Spec.Pool.Name] = rs.Spec.Pool.Generation
		}
	}

	counts := map[string]int32{}
	for _, rs := range slices.Items {
		if rs.Spec.Driver != gpuResourceSliceDriver || rs.Spec.NodeName == nil ||
			rs.Spec.Pool.Generation < newest[rs.Spec.Pool.Name] {
			continue
		}
		for _, d := range rs.Spec.Devices {
			if attr, ok := d.Attributes[deviceTypeAttribute]; ok &&
				attr.StringValue != nil && *attr.StringValue == fullGPUDeviceType {
				counts[*rs.Spec.NodeName]++
			}
		}
	}
	return counts, nil
}
