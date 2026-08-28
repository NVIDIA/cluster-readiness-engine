// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/naming"
)

const (
	scopeJob = "job"
	kindPVC  = "PersistentVolumeClaim"
)

// errDependencyNotReady is returned when a job-scoped dependency (e.g. ComputeDomain)
// was just created and its external controller hasn't yet created required sub-resources
// (e.g. the channel ResourceClaimTemplate). The caller should requeue to give the
// external controller time to reconcile.
var errDependencyNotReady = errors.New("job-scoped dependency not ready: waiting for external controller")

// resourceNameRegex matches valid Kubernetes resource names (RFC 1123 subdomain).
var resourceNameRegex = regexp.MustCompile(`^[a-z0-9][-a-z0-9.]*[a-z0-9]$`)

// classifyDependencies splits dependencies into workflow-scoped and job-scoped
// by walking the job template's string values. A dependency is job-scoped if its
// metadata.name appears (directly or transitively) as a string value reachable
// from the job template. Cluster-scoped resources (kind starting with "Cluster")
// are never job-scoped because per-job copies would collide globally.
func classifyDependencies(deps []nvcrev1alpha1.DependencySpec, jobSpecJSON []byte) (workflowDeps, jobDeps []nvcrev1alpha1.DependencySpec) {
	if len(deps) == 0 {
		return nil, nil
	}

	// Build name→dep index and collect dep names for filtering.
	nameIndex := make(map[string]int, len(deps))
	depNames := make(map[string]bool, len(deps))
	for i, dep := range deps {
		name := extractMetadataName(dep.Raw)
		if name != "" {
			nameIndex[name] = i
			depNames[name] = true
		}
	}

	// Build reverse index: resource-name string → dep indices that contain it.
	// This lets the BFS promote deps that share references (e.g., ComputeDomain
	// and TrainingRuntime both referencing the same ResourceClaimTemplate name).
	sharedRefIndex := make(map[string][]int)
	for i, dep := range deps {
		if strings.HasPrefix(extractKind(dep.Raw), "Cluster") {
			continue
		}
		for _, s := range collectAllStrings(dep.Raw) {
			if isResourceName(s) && !depNames[s] {
				sharedRefIndex[s] = append(sharedRefIndex[s], i)
			}
		}
	}

	// Seed: deps whose metadata.name appears in jobTemplate string values.
	visited := make(map[int]bool, len(deps))
	queue := make([]int, 0)
	promote := func(idx int) {
		if visited[idx] || strings.HasPrefix(extractKind(deps[idx].Raw), "Cluster") {
			return
		}
		visited[idx] = true
		queue = append(queue, idx)
	}

	for _, s := range collectAllStrings(jobSpecJSON) {
		if idx, ok := nameIndex[s]; ok {
			promote(idx)
		}
	}

	// BFS: transitively promote deps via name references and shared resource strings.
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]

		for _, s := range collectAllStrings(deps[idx].Raw) {
			// Direct name reference: dep A's JSON contains dep B's name.
			if refIdx, ok := nameIndex[s]; ok {
				promote(refIdx)
			}
			// Shared reference: dep A and dep C both contain string X.
			for _, peerIdx := range sharedRefIndex[s] {
				promote(peerIdx)
			}
		}
	}

	for i, dep := range deps {
		if visited[i] {
			jobDeps = append(jobDeps, dep)
		} else {
			workflowDeps = append(workflowDeps, dep)
		}
	}
	return workflowDeps, jobDeps
}

// detectCrossRefs finds resource-name-shaped strings that appear in 2+ job-scoped
// deps but aren't any dep's metadata.name. These are internal names (e.g., a
// ComputeDomain channel template name) that need per-job suffixing.
func detectCrossRefs(jobDeps []nvcrev1alpha1.DependencySpec) map[string]bool {
	// Collect all dep metadata.names
	depNames := make(map[string]bool, len(jobDeps))
	for _, dep := range jobDeps {
		if name := extractMetadataName(dep.Raw); name != "" {
			depNames[name] = true
		}
	}

	// Count occurrences of each resource-name-shaped string across deps
	stringCounts := make(map[string]int)
	for _, dep := range jobDeps {
		// Track unique strings per dep to avoid counting duplicates within one dep
		seen := make(map[string]bool)
		for _, s := range collectAllStrings(dep.Raw) {
			if seen[s] || !isResourceName(s) || depNames[s] {
				continue
			}
			seen[s] = true
			stringCounts[s]++
		}
	}

	// Strings in 2+ deps are cross-refs
	crossRefs := make(map[string]bool)
	for s, count := range stringCounts {
		if count >= 2 {
			crossRefs[s] = true
		}
	}
	return crossRefs
}

// orderDependencies returns dependencies in topological creation order.
// If dep A's JSON contains dep B's metadata.name, A depends on B (create B first).
// Dependencies with no inter-references maintain their original order.
func orderDependencies(deps []nvcrev1alpha1.DependencySpec) []nvcrev1alpha1.DependencySpec {
	if len(deps) <= 1 {
		return deps
	}

	// Build name→index mapping
	nameToIdx := make(map[string]int, len(deps))
	for i, dep := range deps {
		if name := extractMetadataName(dep.Raw); name != "" {
			nameToIdx[name] = i
		}
	}

	// Build adjacency list: edges[i] = set of deps that i depends on
	edges := make([]map[int]bool, len(deps))
	inDegree := make([]int, len(deps))
	for i := range deps {
		edges[i] = make(map[int]bool)
	}

	for i, dep := range deps {
		for _, s := range collectAllStrings(dep.Raw) {
			if j, ok := nameToIdx[s]; ok && j != i {
				if !edges[i][j] {
					edges[i][j] = true
					inDegree[i]++ // i depends on j, so i has higher in-degree
				}
			}
		}
	}

	// Kahn's algorithm: items with 0 in-degree are created first.
	// But our edges are reversed: edges[i] = deps i depends on.
	// Reframe: if i depends on j, then j must come before i.
	// Build forward edges: forwardEdges[j] = set of indices that depend on j.
	forwardEdges := make([][]int, len(deps))
	forwardInDegree := make([]int, len(deps))
	for i, depSet := range edges {
		for j := range depSet {
			forwardEdges[j] = append(forwardEdges[j], i)
			forwardInDegree[i]++
		}
	}

	// Stable topological sort using original indices as tiebreaker
	var queue []int
	for i, d := range forwardInDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	sort.Ints(queue)

	result := make([]nvcrev1alpha1.DependencySpec, 0, len(deps))
	for len(queue) > 0 {
		idx := queue[0]
		queue = queue[1:]
		result = append(result, deps[idx])

		var newReady []int
		for _, dependent := range forwardEdges[idx] {
			forwardInDegree[dependent]--
			if forwardInDegree[dependent] == 0 {
				newReady = append(newReady, dependent)
			}
		}
		sort.Ints(newReady)
		queue = append(queue, newReady...)
	}

	// If cycle detected (shouldn't happen), append remaining deps in original order
	if len(result) < len(deps) {
		added := make(map[int]bool, len(result))
		for i, dep := range deps {
			for j, r := range result {
				_ = j
				if extractMetadataName(r.Raw) == extractMetadataName(dep.Raw) {
					added[i] = true
					break
				}
			}
		}
		for i, dep := range deps {
			if !added[i] {
				result = append(result, dep)
			}
		}
	}

	return result
}

// reverseDependencyRefs returns dependency refs in reverse order.
// Since DependencyRefs are stored in creation (topological) order by
// orderDependencies(), reversing gives a safe deletion order: resources
// that depend on others are deleted first, followed by their dependencies.
func reverseDependencyRefs(refs []nvcrev1alpha1.DependencyResourceRef) []nvcrev1alpha1.DependencyResourceRef {
	if len(refs) <= 1 {
		return refs
	}
	reversed := make([]nvcrev1alpha1.DependencyResourceRef, len(refs))
	for i, ref := range refs {
		reversed[len(refs)-1-i] = ref
	}
	return reversed
}

// depJobSuffix computes the name suffix for a job-scoped dependency.
// It mirrors getGroupJobName logic: for a single group and single iteration,
// the suffix is "-job"; otherwise it's "-{groupName}-iter-{iteration}".
func depJobSuffix(totalGroups int, multipleIterations bool, groupName string, iteration int, workflowName string) string {
	// Extract a short hash from the workflow name to prevent name collisions
	// between sequential workflows for the same category variant.
	wfHash := naming.ExtractHash(workflowName)
	if !multipleIterations && totalGroups <= 1 && iteration <= 1 {
		return fmt.Sprintf("-%s-job", wfHash)
	}
	return fmt.Sprintf("-%s-%s-iter-%d", wfHash, groupName, iteration)
}

// buildReplacementMap builds a map of old→new name replacements for job-scoped deps.
// It always includes metadata.name, and also includes auto-detected cross-reference
// names (shared strings across 2+ deps). Structural names internal to a resource
// (e.g., replicatedJob names, container names) are NOT suffixed.
func buildReplacementMap(deps []nvcrev1alpha1.DependencySpec, suffix string) map[string]string {
	replacements := make(map[string]string)

	// Auto-detect cross-references
	crossRefs := detectCrossRefs(deps)

	for _, dep := range deps {
		metaName := extractMetadataName(dep.Raw)
		if metaName == "" {
			continue
		}

		newName := naming.Truncate(metaName+suffix, naming.MaxK8sNameLen)
		replacements[metaName] = newName

		// Add auto-detected cross-reference names (shared across 2+ deps).
		// Only metadata.name and cross-refs need suffixing — structural names
		// internal to a resource (e.g., replicatedJob names, container names,
		// volume names) must NOT be suffixed even if they appear in the job spec.
		for _, s := range collectAllStrings(dep.Raw) {
			if s == metaName {
				continue
			}
			if crossRefs[s] {
				if _, already := replacements[s]; !already {
					replacements[s] = naming.Truncate(s+suffix, naming.MaxK8sNameLen)
				}
			}
		}
	}

	return replacements
}

// suffixDependencyObject applies name replacements to a raw dependency JSON,
// returning the modified unstructured object.
func suffixDependencyObject(raw []byte, replacements map[string]string) (*unstructured.Unstructured, error) {
	data := string(raw)
	for old, newVal := range replacements {
		data = strings.ReplaceAll(data, `"`+old+`"`, `"`+newVal+`"`)
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(data), &obj.Object); err != nil {
		return nil, fmt.Errorf("failed to unmarshal suffixed dependency: %w", err)
	}
	return obj, nil
}

// suffixJobSpec applies name replacements to a job spec via JSON round-trip.
func suffixJobSpec(spec *nvcrev1alpha1.JobSpec, replacements map[string]string) (*nvcrev1alpha1.JobSpec, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal job spec: %w", err)
	}

	s := string(data)
	for old, newVal := range replacements {
		s = strings.ReplaceAll(s, `"`+old+`"`, `"`+newVal+`"`)
	}

	result := &nvcrev1alpha1.JobSpec{}
	if err := json.Unmarshal([]byte(s), result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal suffixed job spec: %w", err)
	}
	return result, nil
}

// ensureJobDependencies creates per-job dependency copies and returns the patched job spec.
// Idempotent: if refs for this group+iteration already exist in status, it skips creation
// and only patches the job spec with the name replacements.
func (r *WorkflowReconciler) ensureJobDependencies(
	ctx context.Context,
	workflow *nvcrev1alpha1.Workflow,
	group *nvcrev1alpha1.GroupStatus,
	orch *nvcrev1alpha1.OrchestrationStatus,
	spec *nvcrev1alpha1.JobSpec,
) (*nvcrev1alpha1.JobSpec, []nvcrev1alpha1.DependencyResourceRef, error) {
	// Marshal job spec for classification
	jobSpecJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal job spec: %w", err)
	}

	_, jobDeps := classifyDependencies(workflow.Spec.Dependencies, jobSpecJSON)
	if len(jobDeps) == 0 {
		return spec, nil, nil
	}

	// Order job deps for creation
	jobDeps = orderDependencies(jobDeps)

	multiIter := hasMultipleIterations(workflow.Spec.Orchestration)
	suffix := depJobSuffix(orch.TotalGroups, multiIter, group.Name, orch.CurrentIteration, workflow.Name)

	replacements := buildReplacementMap(jobDeps, suffix)

	if len(replacements) == 0 {
		return spec, nil, nil
	}

	// Idempotency: check if refs for this group+iteration already exist
	alreadyCreated := false
	for _, ref := range workflow.Status.DependencyRefs {
		if ref.Scope == scopeJob && ref.GroupName == group.Name && ref.Iteration == orch.CurrentIteration {
			alreadyCreated = true
			break
		}
	}

	var refs []nvcrev1alpha1.DependencyResourceRef
	hasNewComputeDomain := false
	if !alreadyCreated {
		// Create suffixed dependency objects
		for _, dep := range jobDeps {
			obj, err := suffixDependencyObject(dep.Raw, replacements)
			if err != nil {
				return nil, nil, err
			}

			ref, created, err := r.createDependencyResource(ctx, nil, workflow, dep, obj)
			if err != nil {
				return nil, nil, err
			}
			if created && extractKind(dep.Raw) == "ComputeDomain" {
				hasNewComputeDomain = true
			}
			ref.Scope = scopeJob
			ref.GroupName = group.Name
			ref.Iteration = orch.CurrentIteration
			refs = append(refs, *ref)
		}
	}

	// If a ComputeDomain was just created, requeue to give the external ComputeDomain
	// controller time to create the channel ResourceClaimTemplate. Without this delay,
	// the TrainJob creates pods that reference a non-existent template, and Kubernetes
	// does not retry FailedResourceClaimCreation — pods stay stuck permanently.
	if hasNewComputeDomain {
		logf.FromContext(ctx).Info("ComputeDomain just created, requeueing to wait for channel ResourceClaimTemplate")
		return nil, refs, errDependencyNotReady
	}

	// Always patch the job spec (even if refs already existed)
	patchedSpec, err := suffixJobSpec(spec, replacements)
	if err != nil {
		return nil, nil, err
	}

	return patchedSpec, refs, nil
}

// cleanupScopedDependencies deletes dependency resources matching the given scope, group, and iteration,
// and removes them from the workflow status. Matching refs are deleted in reverse topological order
// (reverse of creation order) so that resources depending on others are removed first.
func (r *WorkflowReconciler) cleanupScopedDependencies(ctx context.Context, workflow *nvcrev1alpha1.Workflow, scope, groupName string, iteration int) {
	log := logf.FromContext(ctx)

	// Partition refs into matching (to delete) and non-matching (to keep).
	var toDelete []nvcrev1alpha1.DependencyResourceRef
	var remaining []nvcrev1alpha1.DependencyResourceRef
	for _, ref := range workflow.Status.DependencyRefs {
		if ref.Scope != scope || ref.GroupName != groupName || ref.Iteration != iteration {
			remaining = append(remaining, ref)
			continue
		}
		toDelete = append(toDelete, ref)
	}

	// Delete in reverse topological order (reverse of creation order).
	for _, ref := range reverseDependencyRefs(toDelete) {
		if ref.Kind == kindPVC {
			// Delete PVC first, then check if PV is Released and patchable.
			// If PV is still Bound, keep the ref for retry on next reconcile.
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion(ref.APIVersion)
			obj.SetKind(ref.Kind)
			obj.SetName(ref.Name)
			obj.SetNamespace(ref.Namespace)
			log.Info("Deleting scoped dependency resource", "scope", scope, "group", groupName, "iteration", iteration,
				"kind", ref.Kind, "name", ref.Name)
			if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to delete scoped dependency resource", "name", ref.Name)
				remaining = append(remaining, ref)
			} else if !r.cleanupPVForPVC(ctx, ref.Namespace, ref.Name) {
				remaining = append(remaining, ref)
			}
			continue
		}

		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		obj.SetName(ref.Name)
		obj.SetNamespace(ref.Namespace)

		log.Info("Deleting scoped dependency resource", "scope", scope, "group", groupName, "iteration", iteration,
			"kind", ref.Kind, "name", ref.Name)
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete scoped dependency resource", "name", ref.Name)
			// Keep the ref so we can retry later
			remaining = append(remaining, ref)
		}
	}
	workflow.Status.DependencyRefs = remaining
}

// collectAllStrings recursively walks a JSON value and collects all string values.
func collectAllStrings(data []byte) []string {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var result []string
	walkStrings(raw, &result)
	return result
}

func walkStrings(v any, result *[]string) {
	switch val := v.(type) {
	case string:
		*result = append(*result, val)
	case map[string]any:
		for _, child := range val {
			walkStrings(child, result)
		}
	case []any:
		for _, child := range val {
			walkStrings(child, result)
		}
	}
}

// isResourceName checks if a string looks like a valid Kubernetes resource name.
// It requires at least one lowercase alpha character to avoid matching numeric
// quantity values like "128" that appear in resource specifications.
func isResourceName(s string) bool {
	if len(s) < 3 || len(s) > 253 {
		return false
	}
	if !resourceNameRegex.MatchString(s) {
		return false
	}
	// Require at least one lowercase letter to avoid matching pure-numeric strings.
	for _, c := range s {
		if c >= 'a' && c <= 'z' {
			return true
		}
	}
	return false
}

// extractMetadataName extracts metadata.name from raw JSON.
func extractMetadataName(raw []byte) string {
	var partial struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil {
		return ""
	}
	return partial.Metadata.Name
}

// extractKind extracts kind from raw JSON.
func extractKind(raw []byte) string {
	var partial struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil {
		return ""
	}
	return partial.Kind
}
