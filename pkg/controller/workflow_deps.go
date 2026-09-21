// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/naming"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

const (
	scopeJob = "job"
	kindPVC  = "PersistentVolumeClaim"

	// labelWorkflowTracking is the tracking label stamped on every dependency
	// resource the Workflow controller creates.
	labelWorkflowTracking = "nvcre.nvidia.com/workflow"

	// annotationWorkflowUID records the UID of the Workflow that created a
	// dependency resource. It is the creation identity used to verify ownership
	// of dependencies that cannot carry an owner reference to the Workflow:
	// cluster-scoped resources, and job-scoped copies that are created before
	// the Job that later owns them.
	annotationWorkflowUID = "nvcre.nvidia.com/workflow-uid"
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

// marshalJobSpecForDependencyClassification removes metadata values before
// collecting references. Workload metadata and metadata inside runtime patches
// are copied verbatim onto generated objects and never name dependencies;
// allowing an arbitrary label or annotation value to seed classification would
// make that dependency job-scoped even though the Job spec does not reference
// the suffixed copy. Real references elsewhere in a runtime patch, such as a
// PVC claimName, remain in the classification input.
func marshalJobSpecForDependencyClassification(spec *nvcrev1alpha1.JobSpec) ([]byte, error) {
	classifiable := spec.DeepCopy()
	classifiable.WorkloadMetadata = nil
	clearRuntimePatchMetadata(classifiable)
	return json.Marshal(classifiable)
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

// dependencyOwnedByWorkflow reports whether obj carries a UID-strong creation
// identity for the given Workflow: an owner reference with the Workflow's UID,
// or the workflow-uid annotation recorded at creation. This is the adoption
// check for AlreadyExists on create — a match means the object is this
// Workflow's own earlier create surfacing through cache lag or a crash-retry,
// so proceeding is safe. Anything else is a foreign object and must not be
// adopted.
func dependencyOwnedByWorkflow(obj metav1.Object, workflow *nvcrev1alpha1.Workflow) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == workflow.GetUID() {
			return true
		}
	}
	return obj.GetAnnotations()[annotationWorkflowUID] == string(workflow.GetUID())
}

// dependencyOwnedForCleanup reports whether a tracked dependency may be
// deleted during cleanup. In addition to the UID-strong identities accepted by
// dependencyOwnedByWorkflow, it accepts the tracking label stamped at creation
// so that dependencies created by controller versions that predate the
// workflow-uid annotation are still cleaned up after an in-place upgrade.
// A genuinely foreign object carries none of these markers and is skipped.
func dependencyOwnedForCleanup(obj metav1.Object, workflow *nvcrev1alpha1.Workflow) bool {
	if dependencyOwnedByWorkflow(obj, workflow) {
		return true
	}
	return obj.GetLabels()[labelWorkflowTracking] == workflow.GetName()
}

// dependencyRefKey returns a map key identifying the object a
// DependencyResourceRef points at.
func dependencyRefKey(ref nvcrev1alpha1.DependencyResourceRef) string {
	return ref.APIVersion + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// scopedDependencyRefKey extends dependencyRefKey with the ref's scope, group,
// and iteration, uniquely identifying the tracking entry itself.
func scopedDependencyRefKey(ref nvcrev1alpha1.DependencyResourceRef) string {
	return fmt.Sprintf("%s|%s|%s|%d", dependencyRefKey(ref), ref.Scope, ref.GroupName, ref.Iteration)
}

// getDependencyObject fetches the object a DependencyResourceRef points at.
// Returns (nil, nil) when the object no longer exists.
func (r *WorkflowReconciler) getDependencyObject(ctx context.Context, ref nvcrev1alpha1.DependencyResourceRef) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetKind(ref.Kind)
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
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

// suffixRaw replaces every quoted occurrence of each replacement key. It is a
// blind string substitution over the whole document, which is why callers must
// keep user-supplied values that are not references out of its way.
func suffixRaw(raw []byte, replacements map[string]string) string {
	data := string(raw)
	for old, newVal := range replacements {
		data = strings.ReplaceAll(data, `"`+old+`"`, `"`+newVal+`"`)
	}
	return data
}

// suffixJobSpec applies name replacements to a job spec via JSON round-trip.
//
// Two things are protected from that substitution, because their values are
// metadata rather than references and can legitimately equal a dependency
// name — a queue named after the runtime it serves, say:
//
//   - workloadMetadata, detached entirely and restored afterwards. Nothing in
//     it ever references a dependency.
//   - the metadata inside trainJob.runtimePatches. These cannot be detached
//     wholesale, because a patch's volumes can carry a real PVC reference that
//     does need renaming, so only their label and annotation maps are put
//     back.
//
// Without this, renaming would silently rewrite the queue a user asked for and
// the Job would be admitted into a queue nobody named.
func suffixJobSpec(spec *nvcrev1alpha1.JobSpec, replacements map[string]string) (*nvcrev1alpha1.JobSpec, error) {
	preserved := spec.WorkloadMetadata
	renameable := spec.DeepCopy()
	renameable.WorkloadMetadata = nil

	data, err := json.Marshal(renameable)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal job spec: %w", err)
	}

	result := &nvcrev1alpha1.JobSpec{}
	if err := json.Unmarshal([]byte(suffixRaw(data, replacements)), result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal suffixed job spec: %w", err)
	}
	result.WorkloadMetadata = preserved.DeepCopy()
	restoreRuntimePatchMetadata(spec, result)
	return result, nil
}

// restoreRuntimePatchMetadata copies the label and annotation maps out of
// original's runtimePatches over renamed's. Renaming changes only string
// values, never the shape, so the two slices line up by index at every level.
func restoreRuntimePatchMetadata(original, renamed *nvcrev1alpha1.JobSpec) {
	from, to := original.Workload.TrainJob, renamed.Workload.TrainJob
	if from == nil || to == nil || len(from.RuntimePatches) != len(to.RuntimePatches) {
		return
	}
	for i := range from.RuntimePatches {
		src, dst := from.RuntimePatches[i].TrainingRuntimeSpec, to.RuntimePatches[i].TrainingRuntimeSpec
		if src == nil || dst == nil || src.Template == nil || dst.Template == nil {
			continue
		}
		restoreObjectMeta(src.Template.Metadata, dst.Template.Metadata)
		if src.Template.Spec == nil || dst.Template.Spec == nil ||
			len(src.Template.Spec.ReplicatedJobs) != len(dst.Template.Spec.ReplicatedJobs) {
			continue
		}
		for j := range src.Template.Spec.ReplicatedJobs {
			srcJob := src.Template.Spec.ReplicatedJobs[j].Template
			dstJob := dst.Template.Spec.ReplicatedJobs[j].Template
			if srcJob == nil || dstJob == nil {
				continue
			}
			restoreObjectMeta(srcJob.Metadata, dstJob.Metadata)
			if srcJob.Spec == nil || dstJob.Spec == nil ||
				srcJob.Spec.Template == nil || dstJob.Spec.Template == nil {
				continue
			}
			restoreObjectMeta(srcJob.Spec.Template.Metadata, dstJob.Spec.Template.Metadata)
		}
	}
}

// clearRuntimePatchMetadata removes only the label and annotation maps that
// suffixJobSpec restores after blind name substitution. The rest of each patch
// stays visible to dependency classification because it can carry real object
// references, including PVC claim names.
func clearRuntimePatchMetadata(spec *nvcrev1alpha1.JobSpec) {
	trainJob := spec.Workload.TrainJob
	if trainJob == nil {
		return
	}
	for i := range trainJob.RuntimePatches {
		patch := trainJob.RuntimePatches[i].TrainingRuntimeSpec
		if patch == nil || patch.Template == nil {
			continue
		}
		clearObjectMeta(patch.Template.Metadata)
		if patch.Template.Spec == nil {
			continue
		}
		for j := range patch.Template.Spec.ReplicatedJobs {
			job := patch.Template.Spec.ReplicatedJobs[j].Template
			if job == nil {
				continue
			}
			clearObjectMeta(job.Metadata)
			if job.Spec == nil || job.Spec.Template == nil {
				continue
			}
			clearObjectMeta(job.Spec.Template.Metadata)
		}
	}
}

func clearObjectMeta(metadata *metav1.ObjectMeta) {
	if metadata == nil {
		return
	}
	metadata.Labels = nil
	metadata.Annotations = nil
}

// restoreObjectMeta copies src's labels and annotations onto dst, leaving
// everything else renaming produced alone.
func restoreObjectMeta(src, dst *metav1.ObjectMeta) {
	if src == nil || dst == nil {
		return
	}
	if src.Labels != nil {
		dst.Labels = maps.Clone(src.Labels)
	}
	if src.Annotations != nil {
		dst.Annotations = maps.Clone(src.Annotations)
	}
}

// preparedJob is the outcome of per-job dependency preparation: the Job spec
// that will be submitted, the dependency set that spec actually references,
// and the refs to record for cleanup.
//
// EffectiveDependencies exists so callers can check the final manifests rather
// than the Workflow's templates. Job-scoped dependencies are copied under
// suffixed names and the spec's references are rewritten to match, so after
// this step the Workflow's own dependency list no longer describes the
// runtimes this Job will use.
type preparedJob struct {
	Spec                  *nvcrev1alpha1.JobSpec
	EffectiveDependencies []nvcrev1alpha1.DependencySpec

	// alreadyCreated records that this group's copies exist from an earlier
	// reconcile, so creation is a no-op while the spec is still patched.
	alreadyCreated bool
	// originals and renamed are the job-scoped dependencies before and after
	// renaming, paired by index. Creation needs both: the original carries the
	// kind and ordering metadata, the renamed copy is what gets created.
	originals []nvcrev1alpha1.DependencySpec
	renamed   []nvcrev1alpha1.DependencySpec
}

// prepareJobDependencies computes everything about a group's dependencies
// without touching the API: the renamed copies, the Job spec whose references
// point at them, and the set those references resolve to.
//
// It is deliberately free of side effects. Creating the copies is
// createJobDependencies' job, so a caller can validate the final manifests
// before anything is written — otherwise a conflict detected after preparation
// would already have left a runtime dependency behind in the cluster.
func prepareJobDependencies(
	workflow *nvcrev1alpha1.Workflow,
	group *nvcrev1alpha1.GroupStatus,
	orch *nvcrev1alpha1.OrchestrationStatus,
	spec *nvcrev1alpha1.JobSpec,
) (preparedJob, error) {
	// Nothing is renamed on the pass-through paths below, so the Workflow's
	// own dependency list is already the effective one.
	unchanged := preparedJob{Spec: spec, EffectiveDependencies: workflow.Spec.Dependencies}

	// Marshal only fields that can reference dependencies. Metadata values can
	// legitimately equal a dependency name but never refer to that object.
	jobSpecJSON, err := marshalJobSpecForDependencyClassification(spec)
	if err != nil {
		return preparedJob{}, fmt.Errorf("failed to marshal job spec: %w", err)
	}

	workflowDeps, jobDeps := classifyDependencies(workflow.Spec.Dependencies, jobSpecJSON)
	if len(jobDeps) == 0 {
		return unchanged, nil
	}

	// Order job deps for creation
	jobDeps = orderDependencies(jobDeps)

	multiIter := hasMultipleIterations(workflow.Spec.Orchestration)
	suffix := depJobSuffix(orch.TotalGroups, multiIter, group.Name, orch.CurrentIteration, workflow.Name)

	replacements := buildReplacementMap(jobDeps, suffix)

	if len(replacements) == 0 {
		return unchanged, nil
	}

	// Idempotency: check if refs for this group+iteration already exist
	alreadyCreated := false
	for _, ref := range workflow.Status.DependencyRefs {
		if ref.Scope == scopeJob && ref.GroupName == group.Name && ref.Iteration == orch.CurrentIteration {
			alreadyCreated = true
			break
		}
	}

	// Renamed copies of the job-scoped dependencies, built whether or not this
	// reconcile creates them, so the idempotent path still reports the set the
	// patched spec references.
	// Rename each dependency, then put back the scheduling fields the
	// substitution may have rewritten because a queue value happened to equal
	// a dependency name. Preservation carries through whatever the operator
	// configured, including a conflicting override, which validation then
	// rejects; re-applying the configured intent here instead would overwrite
	// that override and run the Job in a queue nobody chose.
	suffixed := make([]nvcrev1alpha1.DependencySpec, len(jobDeps))
	for i, dep := range jobDeps {
		renamed, err := platform.PreserveSchedulingFields(
			dep.Raw, []byte(suffixRaw(dep.Raw, replacements)), workflow.Spec.GangScheduler)
		if err != nil {
			return preparedJob{}, fmt.Errorf("failed to rename job dependency: %w", err)
		}
		suffixed[i] = nvcrev1alpha1.DependencySpec{Raw: renamed}
	}

	effective := make([]nvcrev1alpha1.DependencySpec, 0, len(workflow.Spec.Dependencies))
	effective = append(effective, workflowDeps...)
	effective = append(effective, suffixed...)

	// The job spec is patched whether or not this reconcile creates anything,
	// so the idempotent path still describes the copies that already exist.
	patchedSpec, err := suffixJobSpec(spec, replacements)
	if err != nil {
		return preparedJob{}, err
	}

	return preparedJob{
		Spec:                  patchedSpec,
		EffectiveDependencies: effective,
		alreadyCreated:        alreadyCreated,
		originals:             jobDeps,
		renamed:               suffixed,
	}, nil
}

// createJobDependencies creates the copies prepareJobDependencies computed.
// Callers must have validated the prepared manifests first: everything here
// writes to the cluster.
//
// Refs are returned even on failure so the caller can record them. On a
// terminal error, such as a name collision on a later dependency, there is no
// retry that would re-adopt the copies already created, and untracked copies
// would leak permanently.
func (r *WorkflowReconciler) createJobDependencies(
	ctx context.Context,
	workflow *nvcrev1alpha1.Workflow,
	group *nvcrev1alpha1.GroupStatus,
	orch *nvcrev1alpha1.OrchestrationStatus,
	prepared preparedJob,
) ([]nvcrev1alpha1.DependencyResourceRef, error) {
	if prepared.alreadyCreated {
		return nil, nil
	}

	var refs []nvcrev1alpha1.DependencyResourceRef
	hasNewComputeDomain := false
	for i, dep := range prepared.originals {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(prepared.renamed[i].Raw, &obj.Object); err != nil {
			return refs, fmt.Errorf("failed to unmarshal suffixed dependency: %w", err)
		}

		ref, created, err := r.createDependencyResource(ctx, nil, workflow, dep, obj)
		if err != nil {
			return refs, err
		}
		if created && extractKind(dep.Raw) == "ComputeDomain" {
			hasNewComputeDomain = true
		}
		ref.Scope = scopeJob
		ref.GroupName = group.Name
		ref.Iteration = orch.CurrentIteration
		refs = append(refs, *ref)
	}

	// If a ComputeDomain was just created, requeue to give the external ComputeDomain
	// controller time to create the channel ResourceClaimTemplate. Without this delay,
	// the TrainJob creates pods that reference a non-existent template, and Kubernetes
	// does not retry FailedResourceClaimCreation — pods stay stuck permanently.
	if hasNewComputeDomain {
		logf.FromContext(ctx).Info("ComputeDomain just created, requeueing to wait for channel ResourceClaimTemplate")
		return refs, errDependencyNotReady
	}

	return refs, nil
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
	// Each delete is gated on ownership: the object is fetched and verified to
	// carry this Workflow's creation identity before it is removed. A foreign
	// object with a colliding name is skipped (and dropped from tracking) — we
	// never delete what we did not create.
	for _, ref := range reverseDependencyRefs(toDelete) {
		obj, err := r.getDependencyObject(ctx, ref)
		if err != nil {
			log.Error(err, "Failed to fetch scoped dependency resource before deletion", "name", ref.Name)
			// Keep the ref so we can retry later
			remaining = append(remaining, ref)
			continue
		}

		foreign := obj != nil && !dependencyOwnedForCleanup(obj, workflow)
		if foreign {
			log.Info("Skipping scoped dependency resource not owned by this Workflow",
				"scope", scope, "group", groupName, "iteration", iteration,
				"kind", ref.Kind, "name", ref.Name)
		}

		if ref.Kind == kindPVC {
			// Delete PVC first, then check if PV is Released and patchable.
			// If PV is still Bound, keep the ref for retry on next reconcile.
			// A foreign PVC is neither deleted nor is its backing PV patched.
			if foreign {
				continue
			}
			if obj != nil {
				// Stamp the backing PV with our creation identity before the PVC
				// disappears — cleanupPVForPVC only patches PVs that carry it.
				if err := r.markPVOwnedByWorkflow(ctx, workflow, obj); err != nil {
					log.Error(err, "Failed to mark PV for owned PVC before deletion", "name", ref.Name)
					remaining = append(remaining, ref)
					continue
				}
				log.Info("Deleting scoped dependency resource", "scope", scope, "group", groupName, "iteration", iteration,
					"kind", ref.Kind, "name", ref.Name)
				// The UID precondition closes the window between the ownership
				// check above and this delete: if the owned object was replaced
				// by a same-named foreign one in between, the API server rejects
				// the delete with a conflict and we leave the newcomer alone.
				if err := r.Delete(ctx, obj, client.Preconditions{UID: new(obj.GetUID())}); err != nil &&
					!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					log.Error(err, "Failed to delete scoped dependency resource", "name", ref.Name)
					remaining = append(remaining, ref)
					continue
				}
			}
			if !r.cleanupPVForPVC(ctx, workflow, ref.Namespace, ref.Name) {
				remaining = append(remaining, ref)
			}
			continue
		}

		if foreign || obj == nil {
			continue
		}

		log.Info("Deleting scoped dependency resource", "scope", scope, "group", groupName, "iteration", iteration,
			"kind", ref.Kind, "name", ref.Name)
		// UID precondition: never delete a same-named object that replaced the
		// one whose ownership was verified above.
		if err := r.Delete(ctx, obj, client.Preconditions{UID: new(obj.GetUID())}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
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
