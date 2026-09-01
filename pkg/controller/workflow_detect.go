// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

const (
	platformOnPrem = platform.OnPrem
	platformForge  = platform.Forge
	gpuArchUnknown = "unknown"
)

// nodePlatform determines the cloud platform from a single node's ProviderID
// and labels. Most platforms are identified by providerID prefix alone.
// KubeVirt-based platforms (e.g., TogetherAI) require an additional label
// check since multiple providers may use the kubevirt:// prefix. Forge clusters
// run no cloud-controller-manager and ship an empty providerID, so they are
// detected via the kubernetes.io/hostname label (which Forge bootstrap controls
// by convention) or via the nke.nvidia.com/site-name label (NKE-managed Forge
// clusters whose hostnames do not contain "forge").
func nodePlatform(n corev1.Node) string {
	providerID := n.Spec.ProviderID
	if providerID == "" {
		if h := n.Labels["kubernetes.io/hostname"]; strings.Contains(h, "forge") {
			return platformForge
		}
		if _, ok := n.Labels["nke.nvidia.com/site-name"]; ok {
			return platformForge
		}
		return platformOnPrem
	}
	switch {
	case strings.HasPrefix(providerID, "aws://"):
		return platform.AWS
	case strings.HasPrefix(providerID, "gce://"):
		return platform.GCP
	case strings.HasPrefix(providerID, "azure://"):
		return platform.Azure
	case strings.HasPrefix(providerID, "ocid1."):
		return platform.OCI
	case strings.HasPrefix(providerID, "openstack://"):
		if _, ok := n.Status.Allocatable[corev1.ResourceName("nscale.com/rdmashare")]; ok {
			return platform.NScale
		}
		return platformOnPrem
	case strings.HasPrefix(providerID, "kubevirt://"):
		if _, ok := n.Labels["node-role.together.ai/worker"]; ok {
			return platform.TogetherAI
		}
		return platformOnPrem
	case strings.HasPrefix(providerID, "metal3://"):
		return platform.Mistral
	default:
		return platformOnPrem
	}
}

// detectPlatform determines the cloud platform from the first node's ProviderID.
func detectPlatform(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return platformOnPrem
	}
	return nodePlatform(nodes[0])
}

// detectPlatformConsistent checks that all nodes report the same platform.
// Returns the platform and an error if heterogeneous platforms are detected.
func detectPlatformConsistent(nodes []corev1.Node) (string, error) {
	if len(nodes) == 0 {
		return platformOnPrem, nil
	}
	counts := map[string]int{}
	for _, n := range nodes {
		counts[nodePlatform(n)]++
	}
	if len(counts) > 1 {
		return "", fmt.Errorf("heterogeneous platforms detected: %v", counts)
	}
	return nodePlatform(nodes[0]), nil
}

// nodeGPUArchitecture determines the GPU architecture from a single node's labels.
// It reads nvidia.com/gpu.product and delegates to gpu.ParseProduct.
// Returns "unknown" when the label is absent.
func nodeGPUArchitecture(n corev1.Node) string {
	if arch := gpu.ParseProduct(n.Labels["nvidia.com/gpu.product"]); arch != "" {
		return arch
	}
	return gpuArchUnknown
}

// detectGPUArchitecture determines the GPU architecture reported by the most
// nodes via gpu.MajorityArchitecture, so the WorkloadRun controller and the
// CLI paths that build on the exported DetectGPUArchitecture agree with the
// Workflow and Certification controllers on heterogeneous targets (issue
// #248). Unlabeled nodes do not vote; "unknown" is returned only when no node
// carries the nvidia.com/gpu.product label.
func detectGPUArchitecture(nodes []corev1.Node) string {
	if arch := gpu.MajorityArchitecture(nodes); arch != "" {
		return arch
	}
	return gpuArchUnknown
}

// excludedNodeNames returns the names in all that are absent from kept, in the
// order they appear in all. discoverTargetNodes sorts by name, so that order is
// already name order and no further sort is needed here.
func excludedNodeNames(all, kept []corev1.Node) []string {
	keptNames := make(map[string]bool, len(kept))
	for i := range kept {
		keptNames[kept[i].Name] = true
	}
	var out []string
	for i := range all {
		if !keptNames[all[i].Name] {
			out = append(out, all[i].Name)
		}
	}
	return out
}

// gpuCapacityExclusion names a node dropped for reporting fewer allocatable
// GPUs than the workload requests per node, along with what it does report so
// exclusion messages can say "has X, needs Y" instead of just naming the node.
type gpuCapacityExclusion struct {
	Node string
	// AllocatableGPUs is the node's reported allocatable nvidia.com/gpu count.
	AllocatableGPUs int64
}

// filterNodesByGPUCapacity splits nodes into those that can satisfy a per-node
// request of gpusPerNode and those that report fewer allocatable
// nvidia.com/gpu. An under-capacity node can never schedule the workload's
// pods, so partitioning it into a group leaves that group Pending until the
// run times out with nothing naming the cause (issue #82).
//
// A node that does not report allocatable nvidia.com/gpu at all is kept, not
// excluded: the resource is advertised asynchronously by the device plugin, so
// its absence means "unknown", and dropping a node over a plugin restart would
// spuriously shrink the run and flip a clean PASSED to INCOMPLETE.
//
// A reported count of zero is treated the same way. The kubelet does not
// remove an extended resource when its device plugin endpoint goes away — it
// zeroes the count — so on a GPU-labeled node a zero is the restart window in
// a different encoding, not a statement about the hardware. Excluding on it
// would turn a self-healing Pending (pods schedule once the plugin
// re-advertises) into a terminal failure or a permanent INCOMPLETE. Only a
// positive count below the request is a stable "this node has fewer GPUs"
// report, which is the failure issue #82 is about.
//
// A gpusPerNode of zero or less means the workload requests no GPUs, so
// nothing is filtered.
func filterNodesByGPUCapacity(nodes []corev1.Node, gpusPerNode int32) ([]corev1.Node, []gpuCapacityExclusion) {
	if gpusPerNode <= 0 {
		return nodes, nil
	}
	kept := make([]corev1.Node, 0, len(nodes))
	var excluded []gpuCapacityExclusion
	for _, n := range nodes {
		q, ok := n.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
		if ok {
			if count, exact := q.AsInt64(); exact && count > 0 && count < int64(gpusPerNode) {
				excluded = append(excluded, gpuCapacityExclusion{Node: n.Name, AllocatableGPUs: count})
				continue
			}
		}
		kept = append(kept, n)
	}
	return kept, excluded
}

// maxAllocatableGPUs returns the largest allocatable GPU count among the
// exclusions, so an all-nodes-too-small failure can name the best the fleet
// has to offer next to what the workload asked for.
func maxAllocatableGPUs(excluded []gpuCapacityExclusion) int64 {
	var best int64
	for _, e := range excluded {
		best = max(best, e.AllocatableGPUs)
	}
	return best
}

// workloadGPUsPerNode returns the number of GPUs each node must supply for the
// workflow's pods to schedule. It is read back from the rendered spec rather
// than re-derived from architecture defaults so it is exactly the value the
// pod resources will request: an operator-supplied gpusPerNode override or an
// override patch that rewrites the resources block is already baked in here,
// where a re-derivation from gpu.ParseProduct defaults would disagree with it.
//
// The request can live in different places depending on how the spec was
// built — trainer.resourcesPerNode on a hand-written Workflow, or the
// TrainingRuntime dependency that templates the worker pods for catalog
// entries and WorkloadRuns — so this walks the job template and every
// dependency and returns the largest nvidia.com/gpu quantity found. Every
// dependency is walked, not only the runtime named by trainJob.runtimeRef:
// rendered specs today ship exactly the runtime the job template references,
// so the two are the same set, and if a spec ever carried an unreferenced
// GPU-requesting dependency this errs toward over-requiring (excluding a node
// that might have scheduled) rather than under-requiring (partitioning a node
// whose pods can never schedule, the silent hang this filter exists to
// prevent). Returns 0 when nothing requests GPUs, which callers treat as "no
// per-node GPU requirement".
func workloadGPUsPerNode(spec *nvcrev1alpha1.WorkflowSpec) int32 {
	var found int64
	if b, err := json.Marshal(&spec.JobTemplate); err == nil {
		var doc any
		if json.Unmarshal(b, &doc) == nil {
			found = max(found, maxGPURequest(doc))
		}
	}
	for i := range spec.Dependencies {
		var doc any
		if json.Unmarshal(spec.Dependencies[i].Raw, &doc) == nil {
			found = max(found, maxGPURequest(doc))
		}
	}
	if found > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(found)
}

// maxGPURequest walks a decoded JSON document for resource requirement blocks
// and returns the largest nvidia.com/gpu quantity found. Only values inside a
// "requests" or "limits" map count, so a label or topology key that happens to
// mention the resource name is never misread as a request.
func maxGPURequest(doc any) int64 {
	var found int64
	switch v := doc.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "requests" || key == "limits" {
				if rl, ok := val.(map[string]any); ok {
					if q, ok := rl["nvidia.com/gpu"]; ok {
						found = max(found, gpuQuantityValue(q))
					}
				}
			}
			found = max(found, maxGPURequest(val))
		}
	case []any:
		for _, item := range v {
			found = max(found, maxGPURequest(item))
		}
	}
	return found
}

// gpuQuantityValue parses a JSON resource quantity — a string like "8" or a
// bare number — into a GPU count. Anything unparseable or non-positive is 0.
func gpuQuantityValue(v any) int64 {
	switch q := v.(type) {
	case string:
		if qty, err := resource.ParseQuantity(q); err == nil {
			if n, ok := qty.AsInt64(); ok && n > 0 {
				return n
			}
		}
	case float64:
		if q > 0 {
			return int64(q)
		}
	}
	return 0
}

// detectGPUArchConsistent detects the GPU architecture and filters out nodes
// with a different architecture if the target set is heterogeneous.
//
// The primary architecture comes from the same gpu.MajorityArchitecture vote
// every detection path shares: the architecture with the most labeled nodes
// wins, so a single odd node can never shrink the certification to itself
// (issue #77), and an unlabeled node can never outvote labeled ones (issue
// #248). Ties go to the architecture whose earliest node appears first in the
// input, which discoverTargetNodes has already sorted by name, so the result
// is deterministic for a given node set (the property PR #57 established).
// "unknown" is primary only when no node carries the label, in which case
// every node is "unknown" and nothing is filtered; the Certification
// controller then falls back to its nodeSelector-based detection.
//
// Returns the primary architecture and the (potentially filtered) node list.
func detectGPUArchConsistent(nodes []corev1.Node) (string, []corev1.Node) {
	if len(nodes) == 0 {
		return gpuArchUnknown, nodes
	}
	primary := detectGPUArchitecture(nodes)
	filtered := make([]corev1.Node, 0, len(nodes))
	for _, n := range nodes {
		if nodeGPUArchitecture(n) == primary {
			filtered = append(filtered, n)
		}
	}
	if len(filtered) == len(nodes) {
		// Homogeneous: hand back the input slice untouched.
		return primary, nodes
	}
	return primary, filtered
}

// OverrideContext holds all detected values used for WhenSpec matching.
type OverrideContext struct {
	Platform        string
	GPUArchitecture string
	WorkloadKind    string
	TopologyMode    string
	DomainCount     int
	Config          *apiextensionsv1.JSON
}

// buildOverrideContext creates an OverrideContext from available workflow data.
// Pass nil for nodes when they haven't been discovered yet.
func buildOverrideContext(spec *nvcrev1alpha1.WorkflowSpec, orch *nvcrev1alpha1.OrchestrationStatus, nodes []corev1.Node) OverrideContext {
	octx := OverrideContext{
		Platform:        orch.DetectedPlatform,
		GPUArchitecture: orch.DetectedGPUArchitecture,
		WorkloadKind:    detectWorkloadKind(&spec.JobTemplate.Spec.Workload),
		TopologyMode:    "simple",
	}
	if spec.Orchestration.Topology != nil {
		octx.TopologyMode = "topology"
		octx.DomainCount = countDomains(nodes, spec.Orchestration.Topology.TopologyKey)
	}
	return octx
}

// detectWorkloadKind returns the workload kind based on which field is set.
func detectWorkloadKind(spec *nvcrev1alpha1.WorkloadSpec) string {
	switch {
	case spec.TrainJob != nil:
		return "TrainJob"
	default:
		return ""
	}
}

// countDomains counts the number of distinct topology domains from node labels.
func countDomains(nodes []corev1.Node, topologyKey string) int {
	if topologyKey == "" || len(nodes) == 0 {
		return 0
	}
	domains := make(map[string]struct{})
	for _, n := range nodes {
		if domain := n.Labels[topologyKey]; domain != "" {
			domains[domain] = struct{}{}
		}
	}
	return len(domains)
}

// matchesWhen evaluates a WhenSpec against the override context.
// An empty WhenSpec always matches. Returns an error if CEL evaluation fails.
func matchesWhen(when nvcrev1alpha1.WhenSpec, octx OverrideContext) (bool, error) {
	if when.Platform != nil && !matchesStringSpec(*when.Platform, octx.Platform) {
		return false, nil
	}
	if when.GPUArchitecture != nil && !matchesStringSpec(*when.GPUArchitecture, octx.GPUArchitecture) {
		return false, nil
	}
	if when.WorkloadKind != "" && when.WorkloadKind != octx.WorkloadKind {
		return false, nil
	}
	if when.Topology != nil {
		if when.Topology.Mode != "" && when.Topology.Mode != octx.TopologyMode {
			return false, nil
		}
		if when.Topology.DomainCount != nil && !matchesIntSpec(*when.Topology.DomainCount, octx.DomainCount) {
			return false, nil
		}
	}
	if when.Config != nil && !matchesConfig(when.Config, octx.Config) {
		return false, nil
	}
	if when.Expression != "" {
		match, err := evaluateExpression(when.Expression, octx)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

// matchesStringSpec evaluates a StringMatchSpec against a value.
func matchesStringSpec(spec nvcrev1alpha1.StringMatchSpec, value string) bool {
	if spec.Equals != "" && spec.Equals != value {
		return false
	}
	if len(spec.In) > 0 {
		found := slices.Contains(spec.In, value)
		if !found {
			return false
		}
	}
	if len(spec.NotIn) > 0 {
		if slices.Contains(spec.NotIn, value) {
			return false
		}
	}
	return true
}

// matchesIntSpec evaluates an IntMatchSpec against a value.
func matchesIntSpec(spec nvcrev1alpha1.IntMatchSpec, value int) bool {
	if spec.Equals != nil && *spec.Equals != value {
		return false
	}
	if spec.GreaterThan != nil && value <= *spec.GreaterThan {
		return false
	}
	if spec.LessThan != nil && value >= *spec.LessThan {
		return false
	}
	return true
}

// matchesConfig checks if whenConfig is a subset of actualConfig.
// Every key/value in whenConfig must exist and match in actualConfig.
// Nested maps are checked recursively.
func matchesConfig(whenConfig, actualConfig *apiextensionsv1.JSON) bool {
	if whenConfig == nil {
		return true
	}
	if actualConfig == nil {
		return false
	}
	var whenMap, actualMap map[string]any
	if err := json.Unmarshal(whenConfig.Raw, &whenMap); err != nil {
		return false
	}
	if err := json.Unmarshal(actualConfig.Raw, &actualMap); err != nil {
		return false
	}
	return isSubset(whenMap, actualMap)
}

// isSubset checks if every key/value in subset exists and matches in superset.
func isSubset(subset, superset map[string]any) bool {
	for key, wantVal := range subset {
		gotVal, exists := superset[key]
		if !exists {
			return false
		}
		if wantMap, ok := wantVal.(map[string]any); ok {
			gotMap, ok := gotVal.(map[string]any)
			if !ok {
				return false
			}
			if !isSubset(wantMap, gotMap) {
				return false
			}
		} else if !reflect.DeepEqual(wantVal, gotVal) {
			return false
		}
	}
	return true
}

// evaluateExpression compiles and evaluates a CEL expression against the override context.
func evaluateExpression(expr string, octx OverrideContext) (bool, error) {
	env, err := cel.NewEnv(
		cel.Variable("platform", cel.StringType),
		cel.Variable("gpuArchitecture", cel.StringType),
		cel.Variable("workloadKind", cel.StringType),
		cel.Variable("topologyMode", cel.StringType),
		cel.Variable("domainCount", cel.IntType),
	)
	if err != nil {
		return false, fmt.Errorf("create CEL env: %w", err)
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("compile CEL expression: %w", issues.Err())
	}
	if ast.OutputType() != cel.BoolType {
		return false, fmt.Errorf("CEL expression must return bool, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("create CEL program: %w", err)
	}
	out, _, err := prg.Eval(map[string]any{
		"platform":        octx.Platform,
		"gpuArchitecture": octx.GPUArchitecture,
		"workloadKind":    octx.WorkloadKind,
		"topologyMode":    octx.TopologyMode,
		"domainCount":     octx.DomainCount,
	})
	if err != nil {
		return false, fmt.Errorf("eval CEL expression: %w", err)
	}
	result, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression returned %T, expected bool", out.Value())
	}
	return result, nil
}

// applyOverrides applies all matching overrides to the workflow spec in-memory.
// For jobTemplate, it uses Kubernetes strategic merge patch (struct tags respected).
// If jobTemplatePatch is present, it applies an RFC 6902 JSON Patch after the
// strategic merge. For dependencies, overrides matching an existing dependency
// by apiVersion/kind/name are merged using recursive map merge with named array
// merge (arrays of objects with a "name" field are matched and merged by name);
// unmatched overrides are appended.
func applyOverrides(spec *nvcrev1alpha1.WorkflowSpec, octx OverrideContext) error {
	for i, o := range spec.Overrides {
		matches, err := matchesWhen(o.When, octx)
		if err != nil {
			return fmt.Errorf("override[%d]: evaluating when: %w", i, err)
		}
		if !matches {
			continue
		}

		if o.JobTemplate != nil {
			if err := mergeJobTemplate(&spec.JobTemplate, o.JobTemplate); err != nil {
				return fmt.Errorf("override[%d]: failed to merge jobTemplate: %w", i, err)
			}
		}

		if o.JobTemplatePatch != nil {
			if err := patchJobTemplate(&spec.JobTemplate, o.JobTemplatePatch); err != nil {
				return fmt.Errorf("override[%d]: failed to apply jobTemplatePatch: %w", i, err)
			}
		}

		for _, overrideDep := range o.Dependencies {
			if err := mergeOrAppendDependency(spec, overrideDep); err != nil {
				return fmt.Errorf("override[%d]: failed to merge dependency: %w", i, err)
			}
		}

		if o.Orchestration != nil {
			mergeOrchestration(&spec.Orchestration, o.Orchestration)
		}

	}
	return nil
}

// depKey extracts apiVersion/kind/name from a raw dependency for matching.
func depKey(raw []byte) (string, string, string) {
	var obj struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", "", ""
	}
	return obj.APIVersion, obj.Kind, obj.Metadata.Name
}

// mergeOrAppendDependency merges an override dependency into a matching base
// dependency (by apiVersion/kind/name) using recursive map merge with named
// array merge. If no match is found, the override is appended as a new dependency.
func mergeOrAppendDependency(spec *nvcrev1alpha1.WorkflowSpec, override nvcrev1alpha1.DependencySpec) error {
	oAPI, oKind, oName := depKey(override.Raw)

	for i, base := range spec.Dependencies {
		bAPI, bKind, bName := depKey(base.Raw)
		if bAPI == oAPI && bKind == oKind && bName == oName {
			// Merge the raw resource objects.
			var baseMap, overrideMap map[string]any
			if err := json.Unmarshal(base.Raw, &baseMap); err != nil {
				return fmt.Errorf("unmarshal base dep %s/%s/%s: %w", bAPI, bKind, bName, err)
			}
			if err := json.Unmarshal(override.Raw, &overrideMap); err != nil {
				return fmt.Errorf("unmarshal override dep %s/%s/%s: %w", oAPI, oKind, oName, err)
			}
			merged := mergeMaps(baseMap, overrideMap)
			mergedJSON, err := json.Marshal(merged)
			if err != nil {
				return fmt.Errorf("marshal merged dep %s/%s/%s: %w", oAPI, oKind, oName, err)
			}
			spec.Dependencies[i].Raw = mergedJSON

			return nil
		}
	}

	// No match found — append as a new dependency.
	spec.Dependencies = append(spec.Dependencies, override)
	return nil
}

// mergeJobTemplate applies a strategic merge patch from override onto base.
// Uses Kubernetes strategic merge patch which respects struct tags (e.g.,
// corev1.Container.Env is merged by name). Fields without tags (e.g.,
// Kubeflow Trainer.Env) fall back to list replacement, matching ADR-012's
// documented "lists replace" semantics.
func mergeJobTemplate(base *nvcrev1alpha1.JobTemplateSpec, override *apiextensionsv1.JSON) error {
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("marshal base: %w", err)
	}
	merged, err := strategicpatch.StrategicMergePatch(baseJSON, override.Raw, base)
	if err != nil {
		return fmt.Errorf("strategic merge: %w", err)
	}
	// Zero out base before unmarshaling so fields deleted by the patch
	// (via null) don't retain their previous values.
	*base = nvcrev1alpha1.JobTemplateSpec{}
	return json.Unmarshal(merged, base)
}

// patchJobTemplate applies an RFC 6902 JSON Patch to the base jobTemplate.
// This enables precise operations like removing a specific env var by index,
// testing a precondition before patching, or adding at a specific array position.
func patchJobTemplate(base *nvcrev1alpha1.JobTemplateSpec, patch *apiextensionsv1.JSON) error {
	decodedPatch, err := jsonpatch.DecodePatch(patch.Raw)
	if err != nil {
		return fmt.Errorf("invalid JSON Patch: %w", err)
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("marshal base: %w", err)
	}
	patched, err := decodedPatch.Apply(baseJSON)
	if err != nil {
		return fmt.Errorf("apply JSON Patch: %w", err)
	}
	*base = nvcrev1alpha1.JobTemplateSpec{}
	return json.Unmarshal(patched, base)
}

// mergeMaps recursively merges override into base.
// Null values delete the key. Maps merge recursively. Named slices merge by
// "name" key. Everything else (scalars, unnamed slices) replaces.
func mergeMaps(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base))
	maps.Copy(result, base)
	for key, val := range override {
		if val == nil {
			delete(result, key)
			continue
		}
		result[key] = mergeValue(result[key], val)
	}
	return result
}

// mergeValue merges a single override value into a base value.
func mergeValue(base, override any) any {
	if baseMap, ok := base.(map[string]any); ok {
		if overrideMap, ok := override.(map[string]any); ok {
			return mergeMaps(baseMap, overrideMap)
		}
	}
	if baseSlice, ok := base.([]any); ok {
		if overrideSlice, ok := override.([]any); ok {
			if merged, ok := mergeNamedSlices(baseSlice, overrideSlice); ok {
				return merged
			}
		}
	}
	return override
}

// mergeNamedSlices merges two slices of objects by matching on the "name" field.
// Returns the merged slice and true if both slices contain named objects.
// Base items are kept in order; matching overrides are merged in; unmatched
// overrides are appended.
func mergeNamedSlices(base, override []any) ([]any, bool) {
	baseNames, baseByName := indexByName(base)
	if baseByName == nil {
		return nil, false
	}
	overrideNames, overrideByName := indexByName(override)
	if overrideByName == nil {
		return nil, false
	}

	matched := make(map[string]bool, len(overrideNames))
	result := make([]any, 0, len(base)+len(override))
	for _, name := range baseNames {
		if ov, ok := overrideByName[name]; ok {
			result = append(result, mergeMaps(baseByName[name], ov))
			matched[name] = true
		} else {
			result = append(result, baseByName[name])
		}
	}
	for _, name := range overrideNames {
		if !matched[name] {
			result = append(result, overrideByName[name])
		}
	}
	return result, true
}

// indexByName extracts objects from a slice and indexes them by "name" field.
// Returns insertion-order names and a lookup map, or nil if any item is not a
// named object.
func indexByName(items []any) ([]string, map[string]map[string]any) {
	if len(items) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(items))
	byName := make(map[string]map[string]any, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, nil
		}
		name, _ := obj["name"].(string)
		if name == "" {
			return nil, nil
		}
		names = append(names, name)
		byName[name] = obj
	}
	return names, byName
}

// summarizeWhen produces a human-readable summary of a WhenSpec.
// Example: "gpuArchitecture=h100, platform in [aws,gcp]"
func summarizeWhen(when nvcrev1alpha1.WhenSpec) string {
	var parts []string
	if when.GPUArchitecture != nil {
		parts = append(parts, "gpuArchitecture="+summarizeStringSpec(*when.GPUArchitecture))
	}
	if when.Platform != nil {
		parts = append(parts, "platform="+summarizeStringSpec(*when.Platform))
	}
	if when.WorkloadKind != "" {
		parts = append(parts, "workloadKind="+when.WorkloadKind)
	}
	if when.Topology != nil {
		if when.Topology.Mode != "" {
			parts = append(parts, "topology.mode="+when.Topology.Mode)
		}
		if when.Topology.DomainCount != nil {
			parts = append(parts, "topology.domainCount="+summarizeIntSpec(*when.Topology.DomainCount))
		}
	}
	if when.Config != nil {
		parts = append(parts, "config=<custom>")
	}
	if when.Expression != "" {
		expr := when.Expression
		if len(expr) > 40 {
			expr = expr[:37] + "..."
		}
		parts = append(parts, "expression="+expr)
	}
	if len(parts) == 0 {
		return "<always>"
	}
	return strings.Join(parts, ", ")
}

// summarizeStringSpec returns a compact string representation of a StringMatchSpec.
func summarizeStringSpec(spec nvcrev1alpha1.StringMatchSpec) string {
	if spec.Equals != "" {
		return spec.Equals
	}
	if len(spec.In) > 0 {
		return "in [" + strings.Join(spec.In, ",") + "]"
	}
	if len(spec.NotIn) > 0 {
		return "notIn [" + strings.Join(spec.NotIn, ",") + "]"
	}
	return "<any>"
}

// summarizeIntSpec returns a compact string representation of an IntMatchSpec.
func summarizeIntSpec(spec nvcrev1alpha1.IntMatchSpec) string {
	var parts []string
	if spec.Equals != nil {
		parts = append(parts, fmt.Sprintf("=%d", *spec.Equals))
	}
	if spec.GreaterThan != nil {
		parts = append(parts, fmt.Sprintf(">%d", *spec.GreaterThan))
	}
	if spec.LessThan != nil {
		parts = append(parts, fmt.Sprintf("<%d", *spec.LessThan))
	}
	return strings.Join(parts, ",")
}

// applyPreCommands wraps the trainer command in a /bin/bash -c script that
// runs preCommands in order before exec-ing the original command. Applied after
// all jobTemplate patches so it always wraps the final resolved command.
func applyPreCommands(jt *nvcrev1alpha1.JobTemplateSpec, preCommands []string) {
	if len(preCommands) == 0 {
		return
	}
	if jt.Spec.Workload.TrainJob == nil || jt.Spec.Workload.TrainJob.Trainer == nil {
		return
	}
	trainer := jt.Spec.Workload.TrainJob.Trainer

	// Shell-join the existing command + args into an exec line.
	parts := append(trainer.Command, trainer.Args...)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = fmt.Sprintf("%q", p)
	}

	lines := append(preCommands, "exec "+strings.Join(quoted, " "))
	trainer.Command = []string{"/bin/bash", "-c"}
	trainer.Args = []string{strings.Join(lines, "\n")}
}

// summarizePatches returns a compact description of what an override modifies.
// Example: "jobTemplate, 2 dependencies"
func summarizePatches(o nvcrev1alpha1.OverrideSpec) string {
	var parts []string
	if o.JobTemplate != nil {
		parts = append(parts, "jobTemplate")
	}
	if o.JobTemplatePatch != nil {
		parts = append(parts, "jobTemplatePatch")
	}
	if len(o.Dependencies) > 0 {
		parts = append(parts, fmt.Sprintf("%d dependencies", len(o.Dependencies)))
	}
	if o.Orchestration != nil {
		parts = append(parts, "orchestration")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// mergeOrchestration applies non-nil override fields onto the base orchestration spec.
// Nil fields in the override are skipped (base value preserved).
func mergeOrchestration(base *nvcrev1alpha1.OrchestrationSpec, override *nvcrev1alpha1.OrchestrationOverrideSpec) {
	if override.Target != nil {
		base.Target = override.Target
	}
	if override.Topology != nil {
		base.Topology = override.Topology
	}
	if override.Execution != nil {
		base.Execution = *override.Execution
	}
	if override.Iterations != nil {
		base.Iterations = *override.Iterations
	}
}

// applyOverridesWithTracking applies overrides and returns tracking info about which overrides matched.
// This is used by the authoritative call site in discoverAndPartition to populate status and emit events.
func applyOverridesWithTracking(spec *nvcrev1alpha1.WorkflowSpec, octx OverrideContext) ([]nvcrev1alpha1.AppliedOverride, error) {
	var applied []nvcrev1alpha1.AppliedOverride
	for i, o := range spec.Overrides {
		matches, err := matchesWhen(o.When, octx)
		if err != nil {
			return nil, fmt.Errorf("override[%d]: evaluating when: %w", i, err)
		}
		if !matches {
			continue
		}

		// Snapshot before applying to detect no-ops
		beforeJSON, _ := json.Marshal(spec)

		if o.JobTemplate != nil {
			if err := mergeJobTemplate(&spec.JobTemplate, o.JobTemplate); err != nil {
				return nil, fmt.Errorf("override[%d]: failed to merge jobTemplate: %w", i, err)
			}
		}

		if o.JobTemplatePatch != nil {
			if err := patchJobTemplate(&spec.JobTemplate, o.JobTemplatePatch); err != nil {
				return nil, fmt.Errorf("override[%d]: failed to apply jobTemplatePatch: %w", i, err)
			}
		}

		for _, overrideDep := range o.Dependencies {
			if err := mergeOrAppendDependency(spec, overrideDep); err != nil {
				return nil, fmt.Errorf("override[%d]: failed to merge dependency: %w", i, err)
			}
		}

		if o.Orchestration != nil {
			mergeOrchestration(&spec.Orchestration, o.Orchestration)
		}

		afterJSON, _ := json.Marshal(spec)
		noOp := string(beforeJSON) == string(afterJSON)

		applied = append(applied, nvcrev1alpha1.AppliedOverride{
			Index:   i,
			When:    summarizeWhen(o.When),
			Patches: summarizePatches(o),
			NoOp:    noOp,
		})
	}
	return applied, nil
}
