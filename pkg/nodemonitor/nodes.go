// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodemonitor

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
)

const (
	// NVCREJobLabel is the label used to identify pods belonging to an NVCRE Job.
	NVCREJobLabel = podread.JobLabel

	// PodNVCREJobIndexField is the field index for pod lookups by NVCRE job label.
	// This enables efficient cache-based queries when discovering nodes for a Job.
	PodNVCREJobIndexField = podread.JobIndexField
)

// NodeDiscoverer finds nodes running pods for a given workload.
type NodeDiscoverer struct {
	client client.Client
	pods   *podread.Reader
}

// NewNodeDiscoverer creates a new node discoverer. c reads Nodes; pods reads
// the Job's pods (ADR-102).
func NewNodeDiscoverer(c client.Client, pods *podread.Reader) *NodeDiscoverer {
	return &NodeDiscoverer{client: c, pods: pods}
}

// DiscoverNodesForJob finds all nodes running pods associated with an NVCRE Job.
// It uses a field index on the nvcre.nvidia.com/job label for efficient cache-based lookups.
// Only returns nodes where pods are Running or Pending (i.e., scheduled).
//
// Use this for questions about live health. For questions about where a job ran,
// including after its pods terminated, use DiscoverPlacedNodesForJob.
func (d *NodeDiscoverer) DiscoverNodesForJob(ctx context.Context, namespace, jobName string) ([]string, error) {
	return d.discoverNodes(ctx, namespace, jobName, func(pod *corev1.Pod) bool {
		return pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending
	})
}

// DiscoverPlacedNodesForJob finds every node a Job's pods were bound to,
// whatever phase those pods are now in.
//
// This is the attribution question, not the health question, and the two need
// different filters. A job that fails fast leaves behind Failed pods, and a job
// whose pods have already been cleaned up to Succeeded leaves those; both still
// record the node they ran on in spec.nodeName. Filtering to Running or Pending
// the way DiscoverNodesForJob does would report no nodes in exactly the case
// attribution exists for, so a failed run would name no failed nodes.
//
// A pod that was never bound has no placement to report and is skipped.
func (d *NodeDiscoverer) DiscoverPlacedNodesForJob(ctx context.Context, namespace, jobName string) ([]string, error) {
	return d.discoverNodes(ctx, namespace, jobName, func(*corev1.Pod) bool { return true })
}

// discoverNodes lists a Job's pods and returns the distinct nodes they are bound
// to, in sorted order. Callers supply the phase filter; the read, the binding
// check and the dedupe are shared.
//
// The result is sorted rather than returned in map order, because callers
// persist it into status and compare it against goldens. Go randomizes map
// iteration per run, so an unsorted result would reshuffle a status field
// between reconciles and make golden comparisons flaky.
func (d *NodeDiscoverer) discoverNodes(
	ctx context.Context, namespace, jobName string, keep func(*corev1.Pod) bool,
) ([]string, error) {
	pods, err := d.pods.JobPods(ctx, namespace, jobName)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods for Job %s: %w", jobName, err)
	}

	nodeSet := make(map[string]struct{})
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName == "" || !keep(pod) {
			continue
		}
		nodeSet[pod.Spec.NodeName] = struct{}{}
	}

	nodes := make([]string, 0, len(nodeSet))
	for nodeName := range nodeSet {
		nodes = append(nodes, nodeName)
	}
	sort.Strings(nodes)

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
