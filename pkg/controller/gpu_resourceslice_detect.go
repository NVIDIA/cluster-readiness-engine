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
// the NVML device name, e.g. "NVIDIA GB300". Renaming the driver or the
// attribute, or qualifying the key as gpu.nvidia.com/productName, silently
// disables this fallback.
const (
	gpuResourceSliceDriver = "gpu.nvidia.com"
	productNameAttribute   = "productName"

	// resourceSliceListTimeout bounds the one uncached ResourceSlice List
	// against a slow API server. A 403 or an unserved resource.k8s.io/v1
	// fails immediately without it.
	resourceSliceListTimeout = 5 * time.Second
)

// augmentGPUProductLabels sets nvidia.com/gpu.product on any node in nodes
// that lacks it, from its gpu.nvidia.com ResourceSlice productName device
// attribute (DRA-only platforms run no device plugin/GFD DaemonSet to write
// gpu.product; architecture lives in ResourceSlice attributes instead).
// The NVML name is sanitized as GFD sanitizes it (gpu.ProductLabelValue): the
// same hardware must yield byte-identical labels in a mixed GFD/DRA fleet,
// since UniformGPUProduct compares raw values.
// Mutates the caller's in-memory Node copies only — never persisted back to
// the API server. Every existing label-based consumer (detectGPUArchConsistent,
// DetectGPUArchitecture, UniformGPUProduct, catalog.GPUArchFromNodeSelector,
// NIC detection) keeps working completely unchanged, because all of them only
// ever read the label. One ResourceSlice
// List call total, skipped entirely when every node already carries the
// label (every non-DRA cluster).
func augmentGPUProductLabels(ctx context.Context, reader client.Reader, nodes []corev1.Node) {
	needsLookup := false
	for i := range nodes {
		if nodes[i].Labels[gpu.ProductLabel] == "" {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, resourceSliceListTimeout)
	defer cancel()

	var slices resourcev1.ResourceSliceList
	if err := reader.List(listCtx, &slices); err != nil {
		logf.FromContext(ctx).Info("list gpu.nvidia.com resourceslices for architecture fallback failed",
			"error", err)
		return
	}

	productByNode := make(map[string]string, len(nodes))
	for _, rs := range slices.Items {
		if rs.Spec.Driver != gpuResourceSliceDriver || rs.Spec.NodeName == nil {
			continue
		}
		for _, d := range rs.Spec.Devices {
			if attr, ok := d.Attributes[productNameAttribute]; ok && attr.StringValue != nil {
				productByNode[*rs.Spec.NodeName] = gpu.ProductLabelValue(*attr.StringValue)
				break
			}
		}
	}

	for i := range nodes {
		if nodes[i].Labels[gpu.ProductLabel] != "" {
			continue
		}
		if product, ok := productByNode[nodes[i].Name]; ok {
			if nodes[i].Labels == nil {
				nodes[i].Labels = map[string]string{}
			}
			nodes[i].Labels[gpu.ProductLabel] = product
		}
	}
}
