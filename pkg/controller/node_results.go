// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

const (
	succeededNodesPrefix = "succeeded-nodes-"
	failedNodesPrefix    = "failed-nodes-"
)

// succeededNodesForWorkflow returns the nodes of the groups that are Succeeded in
// the final iteration (or, in diagnose mode, the healthy nodes accumulated across
// rounds). On terminal success that is every node that passed. On terminal
// failure setFinalStatus goes through succeededNodesExcludingFailed instead,
// because a node can pass the final iteration after failing an earlier one.
func succeededNodesForWorkflow(workflow *nvcrev1alpha1.Workflow) []string {
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
		if g.Phase == nvcrev1alpha1.GroupSucceeded {
			nodes = append(nodes, g.Nodes...)
		}
	}
	return nodes
}

// succeededNodesExcludingFailed returns the passed set for a Workflow that has
// reached terminal failure: the nodes succeededNodesForWorkflow takes from the
// final iteration, minus every node in the Workflow's failed-nodes ConfigMap.
//
// The ConfigMap merges the failed nodes of every group, iteration and attempt,
// so a node that failed in an earlier iteration and passed the last one is not
// reported as passed. ADR-062 set that contract ("partial passes from a
// category that ultimately failed are never reported as succeeded"); without
// the subtraction a multi-iteration run listed the same node in both refs
// (issue #470).
//
// The ConfigMap is located by FailedNodesRef, or by its deterministic name
// when the ref has not been set on this copy. A missing ConfigMap means no
// node failed with attribution, so nothing is subtracted. Any other read
// error is returned: the terminal status is written only once, and writing a
// passed set that may include failed nodes would publish the contract break
// this guards against.
func (r *WorkflowReconciler) succeededNodesExcludingFailed(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
) ([]string, error) {
	nodes := succeededNodesForWorkflow(workflow)
	if len(nodes) == 0 {
		return nodes, nil
	}
	name := nodeResultsCMName(failedNodesPrefix, string(workflow.UID))
	if ref := workflow.Status.FailedNodesRef; ref != nil && ref.Name != "" {
		name = ref.Name
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: workflow.Namespace, Name: name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nodes, nil
		}
		return nil, fmt.Errorf("failed to read failed-nodes ConfigMap %s: %w", name, err)
	}
	failed, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		return nil, fmt.Errorf("failed to decode failed-nodes ConfigMap %s: %w", name, err)
	}
	exclude := make(map[string]struct{}, len(failed))
	for _, fn := range failed {
		exclude[fn.Name] = struct{}{}
	}
	passed := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if _, ok := exclude[n]; !ok {
			passed = append(passed, n)
		}
	}
	return passed, nil
}

// sortMergedFailedNodes merges incoming FailedNode entries into existing,
// deduplicating by the (name, reason) pair and sorting the result by name then
// reason.
func sortMergedFailedNodes(existing, incoming []nvcrev1alpha1.FailedNode) []nvcrev1alpha1.FailedNode {
	seen := make(map[[2]string]struct{}, len(existing)+len(incoming))
	merged := make([]nvcrev1alpha1.FailedNode, 0, len(existing)+len(incoming))
	add := func(nodes []nvcrev1alpha1.FailedNode) {
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
func (r *WorkflowReconciler) recordSucceededNodes(ctx context.Context, workflow *nvcrev1alpha1.Workflow, nodes []string) error {
	return recordNodeResults(ctx, r, workflow, nodeResultsTarget[string]{
		namePrefix: succeededNodesPrefix,
		dataKey:    noderesults.SucceededNodesConfigMapKey,
		merge:      mergeSucceededNodesCSV,
		setRef: func(w *nvcrev1alpha1.Workflow, ref *corev1.TypedLocalObjectReference) {
			w.Status.SucceededNodesRef = ref
		},
	}, nodes)
}

// recordFailedNodes merges the given failed nodes (name, reason, message) into the
// Workflow's failed-nodes ConfigMap and sets workflow.Status.FailedNodesRef.
func (r *WorkflowReconciler) recordFailedNodes(ctx context.Context, workflow *nvcrev1alpha1.Workflow, nodes []nvcrev1alpha1.FailedNode) error {
	return recordNodeResults(ctx, r, workflow, nodeResultsTarget[nvcrev1alpha1.FailedNode]{
		namePrefix: failedNodesPrefix,
		dataKey:    noderesults.FailedNodesConfigMapKey,
		merge:      mergeFailedNodesJSON,
		setRef:     func(w *nvcrev1alpha1.Workflow, ref *corev1.TypedLocalObjectReference) { w.Status.FailedNodesRef = ref },
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
	setRef func(*nvcrev1alpha1.Workflow, *corev1.TypedLocalObjectReference)
}

// nodeResultsWriteError decides what a failed node-result write does to the
// pass that made it. Only a write the controller has lost its access for ends
// the pass (ADR-092): a Forbidden that a fresh access review confirms, or one
// the review cannot classify. Going on would write a terminal status without
// the results; every caller writes status only after this, so returning first
// leaves the persisted state untouched, and requeueAfterForbiddenWrite records
// the wait. The error carries the review's answer so it is not asked twice.
//
// Every other failure stays best effort, as before ADR-092: it is logged and
// the pass goes on to its terminal status. That includes a Forbidden the
// review allows, which an admission policy or ResourceQuota on ConfigMaps
// returned. No binding can fix such a rejection, so ending the pass would only
// retry it forever; it gets a Warning event on the Workflow, so the lost
// record is visible beyond the controller log.
func (r *WorkflowReconciler) nodeResultsWriteError(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, err error, msg string,
) error {
	if err == nil {
		return nil
	}
	if apierrors.IsForbidden(err) && r.AccessReviewer != nil {
		access := r.reviewNamespaceAccess(ctx, workflow, true).forWrite(err)
		if !access.granted() {
			return &accessDeniedWriteError{err: err, access: access}
		}
		r.eventf(workflow, corev1.EventTypeWarning, ReasonNodeResultsWriteRejected,
			"%s: %v. The write was rejected although the controller has access in the namespace, "+
				"so an admission policy or ResourceQuota refused it; the Workflow continues without this record.",
			msg, err)
	}
	logf.FromContext(ctx).Error(err, msg)
	return nil
}

// accessDeniedWriteError is a forbidden write an access review attributed to
// the controller's own permissions, carrying the review's answer.
type accessDeniedWriteError struct {
	err    error
	access namespaceAccessResult
}

func (e *accessDeniedWriteError) Error() string { return e.err.Error() }

func (e *accessDeniedWriteError) Unwrap() error { return e.err }

// recordNodeResults merges entries into the Workflow's node-result ConfigMap,
// creating it if absent, and points the corresponding status ref at it.
//
// Results are held in a ConfigMap rather than inline in status because a
// large-cluster run can list thousands of node names, which would push the
// Workflow object past the etcd value limit.
func recordNodeResults[T any](
	ctx context.Context,
	r *WorkflowReconciler,
	workflow *nvcrev1alpha1.Workflow,
	target nodeResultsTarget[T],
	entries []T,
) error {
	if len(entries) == 0 {
		return nil
	}

	cmName := nodeResultsCMName(target.namePrefix, string(workflow.UID))
	cm := &corev1.ConfigMap{
		Name:      cmName,
		Namespace: workflow.Namespace,
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

	target.setRef(workflow, &corev1.TypedLocalObjectReference{Kind: kindConfigMap, Name: cmName})
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
func mergeFailedNodesJSON(existing []byte, newNodes []nvcrev1alpha1.FailedNode) ([]byte, error) {
	var existingNodes []nvcrev1alpha1.FailedNode
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
