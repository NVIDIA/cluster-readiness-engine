// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
)

// DiscoverGPUNodes discovers GPU nodes matching a target, validates that all
// nodes share the same nvidia.com/gpu.product, and returns them with the
// product string. If target is nil all GPU nodes are selected.
func DiscoverGPUNodes(
	ctx context.Context, c client.Client, target *burninv1alpha1.TargetSpec,
) ([]corev1.Node, string, error) {
	nodes, err := controller.DiscoverTargetNodes(ctx, c, target)
	if err != nil {
		return nil, "", fmt.Errorf("node discovery: %w", err)
	}
	if len(nodes) == 0 {
		return nil, "", fmt.Errorf("no GPU nodes match target")
	}

	product, err := UniformGPUProduct(nodes)
	if err != nil {
		return nil, "", err
	}
	return nodes, product, nil
}

// UniformGPUProduct returns the nvidia.com/gpu.product label shared by all
// nodes, or an error when the label is missing or heterogeneous.
func UniformGPUProduct(nodes []corev1.Node) (string, error) {
	products := map[string]int{}
	for i := range nodes {
		if p := nodes[i].Labels["nvidia.com/gpu.product"]; p != "" {
			products[p]++
		}
	}
	switch len(products) {
	case 0:
		return "", fmt.Errorf("no nodes have nvidia.com/gpu.product label")
	case 1:
		for p := range products {
			return p, nil
		}
	}
	var seen []string
	for p, n := range products {
		seen = append(seen, fmt.Sprintf("%s (%d)", p, n))
	}
	return "", fmt.Errorf("heterogeneous GPUs: %s", strings.Join(seen, ", "))
}
