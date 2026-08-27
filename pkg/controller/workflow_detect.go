// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
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

// detectGPUArchitecture determines the GPU architecture from the first node's labels.
func detectGPUArchitecture(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return gpuArchUnknown
	}
	return nodeGPUArchitecture(nodes[0])
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

// detectGPUArchConsistent detects the GPU architecture and filters out nodes
// with a different architecture if the target set is heterogeneous.
// Returns the primary architecture and the (potentially filtered) node list.
func detectGPUArchConsistent(nodes []corev1.Node) (string, []corev1.Node) {
	if len(nodes) == 0 {
		return gpuArchUnknown, nodes
	}
	counts := map[string]int{}
	for _, n := range nodes {
		counts[nodeGPUArchitecture(n)]++
	}
	primary := nodeGPUArchitecture(nodes[0])
	if len(counts) <= 1 {
		return primary, nodes
	}
	// Heterogeneous: filter to primary architecture only
	filtered := make([]corev1.Node, 0, counts[primary])
	for _, n := range nodes {
		if nodeGPUArchitecture(n) == primary {
			filtered = append(filtered, n)
		}
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
func buildOverrideContext(spec *crev1alpha1.WorkflowSpec, orch *crev1alpha1.OrchestrationStatus, nodes []corev1.Node) OverrideContext {
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
func detectWorkloadKind(spec *crev1alpha1.WorkloadSpec) string {
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
func matchesWhen(when crev1alpha1.WhenSpec, octx OverrideContext) (bool, error) {
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
func matchesStringSpec(spec crev1alpha1.StringMatchSpec, value string) bool {
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
func matchesIntSpec(spec crev1alpha1.IntMatchSpec, value int) bool {
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
func applyOverrides(spec *crev1alpha1.WorkflowSpec, octx OverrideContext) error {
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
func mergeOrAppendDependency(spec *crev1alpha1.WorkflowSpec, override crev1alpha1.DependencySpec) error {
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
func mergeJobTemplate(base *crev1alpha1.JobTemplateSpec, override *apiextensionsv1.JSON) error {
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
	*base = crev1alpha1.JobTemplateSpec{}
	return json.Unmarshal(merged, base)
}

// patchJobTemplate applies an RFC 6902 JSON Patch to the base jobTemplate.
// This enables precise operations like removing a specific env var by index,
// testing a precondition before patching, or adding at a specific array position.
func patchJobTemplate(base *crev1alpha1.JobTemplateSpec, patch *apiextensionsv1.JSON) error {
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
	*base = crev1alpha1.JobTemplateSpec{}
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
func summarizeWhen(when crev1alpha1.WhenSpec) string {
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
func summarizeStringSpec(spec crev1alpha1.StringMatchSpec) string {
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
func summarizeIntSpec(spec crev1alpha1.IntMatchSpec) string {
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
func applyPreCommands(jt *crev1alpha1.JobTemplateSpec, preCommands []string) {
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
func summarizePatches(o crev1alpha1.OverrideSpec) string {
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
func mergeOrchestration(base *crev1alpha1.OrchestrationSpec, override *crev1alpha1.OrchestrationOverrideSpec) {
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
func applyOverridesWithTracking(spec *crev1alpha1.WorkflowSpec, octx OverrideContext) ([]crev1alpha1.AppliedOverride, error) {
	var applied []crev1alpha1.AppliedOverride
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

		applied = append(applied, crev1alpha1.AppliedOverride{
			Index:   i,
			When:    summarizeWhen(o.When),
			Patches: summarizePatches(o),
			NoOp:    noOp,
		})
	}
	return applied, nil
}
