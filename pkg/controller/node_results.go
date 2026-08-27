// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

const (
	succeededNodesPrefix = "succeeded-nodes-"
	failedNodesPrefix    = "failed-nodes-"
)

// succeededNodesForWorkflow returns the full set of nodes that passed for a Workflow
// that has reached terminal success.
func succeededNodesForWorkflow(workflow *crev1alpha1.Workflow) []string {
	orch := workflow.Status.Orchestration
	if orch == nil {
		return nil
	}
	// Fault-isolation (diagnose) accumulates the passing nodes in HealthyNodes across
	// rounds; the per-round Groups are only the last round, so use HealthyNodes.
	if orch.Diagnose != nil {
		return orch.Diagnose.HealthyNodes
	}
	var nodes []string
	for _, g := range orch.Groups {
		if g.Phase == crev1alpha1.GroupSucceeded {
			nodes = append(nodes, g.Nodes...)
		}
	}
	return nodes
}

// sortMergedFailedNodes merges incoming FailedNode entries into existing,
// deduplicating by the (name, reason) pair and sorting the result by name then
// reason.
func sortMergedFailedNodes(existing, incoming []crev1alpha1.FailedNode) []crev1alpha1.FailedNode {
	seen := make(map[[2]string]struct{}, len(existing)+len(incoming))
	merged := make([]crev1alpha1.FailedNode, 0, len(existing)+len(incoming))
	add := func(nodes []crev1alpha1.FailedNode) {
		for _, fn := range nodes {
			if fn.Name == "" {
				continue
			}
			key := [2]string{fn.Name, string(fn.Reason)}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, fn)
		}
	}
	add(existing)
	add(incoming)
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Name != merged[j].Name {
			return merged[i].Name < merged[j].Name
		}
		return merged[i].Reason < merged[j].Reason
	})
	return merged
}

// nodeResultsCMName returns a deterministic ConfigMap name for the given prefix
// and Workflow UID, e.g. "succeeded-nodes-b619c4c1".
func nodeResultsCMName(prefix string, workflowUID string) string {
	uid := workflowUID
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return prefix + uid
}

// recordSucceededNodes merges the given nodes into the Workflow's succeeded-nodes
// ConfigMap and sets workflow.Status.SucceededNodesRef.
func (r *WorkflowReconciler) recordSucceededNodes(ctx context.Context, workflow *crev1alpha1.Workflow, nodes []string) error {
	return recordNodeResults(ctx, r, workflow, nodeResultsTarget[string]{
		namePrefix: succeededNodesPrefix,
		dataKey:    noderesults.SucceededNodesConfigMapKey,
		merge:      mergeSucceededNodesCSV,
		setRef: func(w *crev1alpha1.Workflow, ref *corev1.TypedLocalObjectReference) {
			w.Status.SucceededNodesRef = ref
		},
	}, nodes)
}

// recordFailedNodes merges the given failed nodes (name, reason, message) into the
// Workflow's failed-nodes ConfigMap and sets workflow.Status.FailedNodesRef.
func (r *WorkflowReconciler) recordFailedNodes(ctx context.Context, workflow *crev1alpha1.Workflow, nodes []crev1alpha1.FailedNode) error {
	return recordNodeResults(ctx, r, workflow, nodeResultsTarget[crev1alpha1.FailedNode]{
		namePrefix: failedNodesPrefix,
		dataKey:    noderesults.FailedNodesConfigMapKey,
		merge:      mergeFailedNodesJSON,
		setRef:     func(w *crev1alpha1.Workflow, ref *corev1.TypedLocalObjectReference) { w.Status.FailedNodesRef = ref },
	}, nodes)
}

// nodeResultsTarget describes one of the two node-result ConfigMaps a Workflow
// maintains. The succeeded and failed variants differ only in these four values;
// everything else — naming, ownership, create-or-update, status wiring — is
// identical and lives in recordNodeResults.
type nodeResultsTarget[T any] struct {
	namePrefix string
	dataKey    string
	// merge folds newEntries into the existing encoded payload (which may be
	// empty) and returns the re-encoded bytes.
	merge  func(existing []byte, newEntries []T) ([]byte, error)
	setRef func(*crev1alpha1.Workflow, *corev1.TypedLocalObjectReference)
}

// recordNodeResults merges entries into the Workflow's node-result ConfigMap,
// creating it if absent, and points the corresponding status ref at it.
//
// Results are held in a ConfigMap rather than inline in status because a
// large-cluster run can list thousands of node names, which would push the
// Workflow object past the etcd value limit.
func recordNodeResults[T any](
	ctx context.Context,
	r *WorkflowReconciler,
	workflow *crev1alpha1.Workflow,
	target nodeResultsTarget[T],
	entries []T,
) error {
	if len(entries) == 0 {
		return nil
	}

	cmName := nodeResultsCMName(target.namePrefix, string(workflow.UID))
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: workflow.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := controllerutil.SetControllerReference(workflow, cm, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on %s: %w", cmName, err)
		}
		if cm.BinaryData == nil {
			cm.BinaryData = make(map[string][]byte)
		}
		merged, err := target.merge(cm.BinaryData[target.dataKey], entries)
		if err != nil {
			return fmt.Errorf("failed to merge %s entries: %w", target.dataKey, err)
		}
		cm.BinaryData[target.dataKey] = merged
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to write node-results ConfigMap %s: %w", cmName, err)
	}

	target.setRef(workflow, &corev1.TypedLocalObjectReference{Kind: "ConfigMap", Name: cmName})
	return nil
}

// mergeSucceededNodesCSV decodes the existing gzip-compressed comma-separated node
// list (may be empty), unions in newNodes, dedupes and sorts, and returns the
// re-encoded gzip-CSV bytes.
func mergeSucceededNodesCSV(existing []byte, newNodes []string) ([]byte, error) {
	set := make(map[string]struct{})
	if len(existing) > 0 {
		decoded, err := gzip.GunzipString(existing)
		if err != nil {
			return nil, fmt.Errorf("failed to decode existing succeeded-nodes entry: %w", err)
		}
		for n := range strings.SplitSeq(decoded, ",") {
			if n = strings.TrimSpace(n); n != "" {
				set[n] = struct{}{}
			}
		}
	}
	for _, n := range newNodes {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = struct{}{}
		}
	}

	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return gzip.GzipString(strings.Join(names, ","))
}

// mergeFailedNodesJSON decodes the existing gzip-compressed failed-nodes JSON (may be
// empty), unions in newNodes (deduping by the (name, reason) pair via
// SortMergedFailedNodes), and returns the re-encoded gzip-JSON bytes.
func mergeFailedNodesJSON(existing []byte, newNodes []crev1alpha1.FailedNode) ([]byte, error) {
	var existingNodes []crev1alpha1.FailedNode
	if len(existing) > 0 {
		decoded, err := gzip.GunzipString(existing)
		if err != nil {
			return nil, fmt.Errorf("failed to decode existing failed-nodes entry: %w", err)
		}
		existingNodes, err = noderesults.FailedNodesFromJSON([]byte(decoded))
		if err != nil {
			return nil, err
		}
	}
	merged := sortMergedFailedNodes(existingNodes, newNodes)
	jsonBytes, err := noderesults.FailedNodesToJSON(merged)
	if err != nil {
		return nil, err
	}
	return gzip.GzipString(string(jsonBytes))
}
