// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodemonitor

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// NVCREJobLabel is the label used to identify pods belonging to an NVCRE Job.
	NVCREJobLabel = "nvcre.nvidia.com/job"

	// PodNVCREJobIndexField is the field index for pod lookups by NVCRE job label.
	// This enables efficient cache-based queries when discovering nodes for a Job.
	PodNVCREJobIndexField = "metadata.labels.nvcre.nvidia.com/job"
)

// NodeDiscoverer finds nodes running pods for a given workload.
type NodeDiscoverer struct {
	client client.Client
}

// NewNodeDiscoverer creates a new node discoverer.
func NewNodeDiscoverer(c client.Client) *NodeDiscoverer {
	return &NodeDiscoverer{client: c}
}

// DiscoverNodesForJob finds all nodes running pods associated with an NVCRE Job.
// It uses a field index on the nvcre.nvidia.com/job label for efficient cache-based lookups.
// Only returns nodes where pods are Running or Pending (i.e., scheduled).
func (d *NodeDiscoverer) DiscoverNodesForJob(ctx context.Context, namespace, jobName string) ([]string, error) {
	podList := &corev1.PodList{}

	// Use field index if available, fall back to label selector
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{PodNVCREJobIndexField: jobName},
	}

	if err := d.client.List(ctx, podList, listOpts...); err != nil {
		// Fall back to label selector if field index is not available
		labelSelector := client.MatchingLabels{NVCREJobLabel: jobName}
		if err := d.client.List(ctx, podList, client.InNamespace(namespace), labelSelector); err != nil {
			return nil, fmt.Errorf("failed to list pods for Job %s: %w", jobName, err)
		}
	}

	// Extract unique node names from scheduled pods
	nodeSet := make(map[string]struct{})
	for _, pod := range podList.Items {
		// Only consider pods that are assigned to a node and are running or pending
		if pod.Spec.NodeName != "" &&
			(pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending) {
			nodeSet[pod.Spec.NodeName] = struct{}{}
		}
	}

	nodes := make([]string, 0, len(nodeSet))
	for nodeName := range nodeSet {
		nodes = append(nodes, nodeName)
	}

	return nodes, nil
}

// GetNode retrieves a Node object by name.
func (d *NodeDiscoverer) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	node := &corev1.Node{}
	if err := d.client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", name, err)
	}
	return node, nil
}

// GetNodes retrieves multiple Node objects by name.
// Returns the nodes that were found and any errors encountered.
// Continues to fetch remaining nodes even if some fail.
func (d *NodeDiscoverer) GetNodes(ctx context.Context, names []string) ([]*corev1.Node, []error) {
	nodes := make([]*corev1.Node, 0, len(names))
	var errs []error

	for _, name := range names {
		node, err := d.GetNode(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		nodes = append(nodes, node)
	}

	return nodes, errs
}
