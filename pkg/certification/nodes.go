// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

const defaultKubeNamespace = "default"

// failedNodesFromRef resolves a nodeResultsRef to its failed-nodes list by fetching
// the referenced ConfigMap and decoding the failed-nodes entry (name, reason,
// message).
func failedNodesFromRef(
	ctx context.Context, c client.Client, namespace string, ref *corev1.TypedLocalObjectReference,
) []crev1alpha1.FailedNode {
	if ref == nil || ref.Name == "" {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
		return nil
	}
	nodes, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		return nil
	}
	return nodes
}

// certFailedNodes returns the deduped union of failed node names across all
// categories, resolved from each category's nodeResultsRef ConfigMap.
func certFailedNodes(ctx context.Context, c client.Client, cert *crev1alpha1.Certification) []string {
	seen := make(map[string]struct{})
	var union []string
	for _, cat := range cert.Status.CategoryStatuses {
		for _, n := range failedNodesFromRef(ctx, c, cert.Namespace, cat.FailedNodesRef) {
			if n.Name == "" {
				continue
			}
			if _, ok := seen[n.Name]; ok {
				continue
			}
			seen[n.Name] = struct{}{}
			union = append(union, n.Name)
		}
	}
	sort.Strings(union)
	return union
}
