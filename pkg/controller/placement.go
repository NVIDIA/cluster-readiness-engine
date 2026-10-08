// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// hostnameLabelKey is the node label every pinned job matches on.
const hostnameLabelKey = "kubernetes.io/hostname"

// gpuProductLabelKey carries the unnormalized GPU model, e.g. "NVIDIA-H100-80GB-HBM3".
const gpuProductLabelKey = "nvidia.com/gpu.product"

// ValidatePlacement rejects orchestration settings that contradict Unpinned.
//
// Two of the three checks reject; the third deliberately does not. diagnose and
// topology.strictDomain are explicit requests for a particular number of jobs,
// and a user who wrote either of them alongside Unpinned asked for two
// incompatible things. topology.topologyKey is different: the GB200/GB300
// catalog override injects it (see pkg/catalog/entries/training/nemotron5-8b.yaml),
// and overrides are applied before this runs, so rejecting it would fail a
// Nemotron run on GB200 with a message about a field the user never wrote.
// Unpinned does not partition, so there is nothing for a topology key to do and
// ignoring it is both safe and the only shippable option. See ADR-089.
func ValidatePlacement(orch *nvcrev1alpha1.OrchestrationSpec) error {
	if orch == nil || !nvcrev1alpha1.IsUnpinned(orch.Placement) {
		return nil
	}
	if orch.Diagnose != nil {
		return fmt.Errorf("orchestration.placement: Unpinned is incompatible with orchestration.diagnose: " +
			"diagnose isolates faults by partitioning nodes into groups and bisecting the failures, " +
			"which requires the node pinning Unpinned removes; drop one of the two")
	}
	if orch.Topology != nil && orch.Topology.StrictDomain {
		return fmt.Errorf("orchestration.placement: Unpinned is incompatible with " +
			"orchestration.topology.strictDomain: strictDomain runs one job per topology domain, " +
			"Unpinned runs exactly one job; to confine a single unpinned job to one domain, " +
			"name the domain label in target.matchExpressions instead")
	}
	return nil
}

// ValidateWRPlacement is the WorkloadRun-tier half of ValidatePlacement.
//
// It runs before buildWROrchestration lowers testScale, which is the only
// reason it exists as a separate check. By the time the OrchestrationSpec
// exists, intra-rack has already become Topology{StrictDomain: true} and
// intra-node has vanished entirely into NodesPerJobForScale, so a message from
// the lower tier would name a field the user never wrote.
//
// full-scale is deliberately not rejected. It has always claimed "all target
// nodes in a single group" and never done it; Unpinned is what finally makes
// that claim true, so the pair agrees rather than conflicts.
func ValidateWRPlacement(orch *nvcrev1alpha1.WorkloadOrchestration) error {
	if orch == nil || !nvcrev1alpha1.IsUnpinned(orch.Placement) {
		return nil
	}
	switch orch.TestScale {
	case nvcrev1alpha1.TestScaleIntraNode:
		return fmt.Errorf("orchestration.placement: Unpinned is incompatible with "+
			"orchestration.testScale %q: intra-node runs one job per target node, "+
			"Unpinned runs exactly one job; drop one of the two", orch.TestScale)
	case "intra-rack":
		return fmt.Errorf("orchestration.placement: Unpinned is incompatible with "+
			"orchestration.testScale %q: intra-rack runs one job per topology domain, "+
			"Unpinned runs exactly one job; to confine a single unpinned job to one domain, "+
			"name the domain label in target.matchExpressions instead", orch.TestScale)
	}
	return nil
}

// placementOrDefault renders the placement mode for logs and reports, resolving
// the empty string to its meaning rather than printing a blank.
func placementOrDefault(placement string) string {
	if placement == "" {
		return nvcrev1alpha1.PlacementPinned
	}
	return placement
}

// IgnoredTopologyKey returns the topology key that Unpinned is dropping, or ""
// when nothing is being ignored. Callers log it so the behavior is visible
// rather than silent.
func IgnoredTopologyKey(orch *nvcrev1alpha1.OrchestrationSpec) string {
	if orch == nil || !nvcrev1alpha1.IsUnpinned(orch.Placement) {
		return ""
	}
	if orch.Topology == nil {
		return ""
	}
	return orch.Topology.TopologyKey
}

// TargetNodeAffinityTerms converts a TargetSpec into node selector requirements
// for the pod spec.
//
// The target has always been a discovery-side filter only: it chose which nodes
// the controller considered and never reached the pods. Pinning masked that,
// since pods landed on hosts the filter had already approved. Unpinned removes
// the pin, so the target has to be carried onto the pods for real. It is applied
// in both modes, because the gap is a defect either way and one placement
// contract is easier to reason about than two.
//
// Two discovery-side filters are deliberately omitted because the scheduler
// enforces them natively: cordoned nodes are unschedulable, and under-capacity
// nodes fail the GPU resource request. The architecture filter is not native,
// which is why gpuProducts is carried explicitly.
//
// nodeNames is excluded here because Pinned already expresses it through the
// hostname term; the Unpinned caller adds it separately. taintSelectors is
// excluded because it selects nodes by taint and is answered by tolerations.
//
// Requirements are returned for a single NodeSelectorTerm. Terms OR together
// while expressions within a term AND, and the target's documented semantics are
// AND, so the caller must not spread these across multiple terms.
func TargetNodeAffinityTerms(target *nvcrev1alpha1.TargetSpec, gpuProducts []string) []corev1.NodeSelectorRequirement {
	var reqs []corev1.NodeSelectorRequirement

	if target != nil {
		// Sorted so the rendered pod spec is deterministic across map iterations.
		keys := make([]string, 0, len(target.NodeSelector))
		for k := range target.NodeSelector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			reqs = append(reqs, corev1.NodeSelectorRequirement{
				Key:      k,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{target.NodeSelector[k]},
			})
		}
		reqs = append(reqs, target.MatchExpressions...)
	}

	// An empty In list matches no node, so omit the term rather than deadlock
	// scheduling on a fleet with no gpu.product labels.
	if products := dedupeSorted(gpuProducts); len(products) > 0 {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key:      gpuProductLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   products,
		})
	}

	return dedupeRequirements(reqs)
}

// dedupeRequirements drops requirements that are exactly equal to an earlier
// one, preserving first-occurrence order.
//
// The case that makes this worth doing is common rather than exotic: a target
// that pins nvidia.com/gpu.product in its nodeSelector produces the identical
// requirement the gpuProducts term adds, so the pod spec would carry the same
// expression twice. ANDing a requirement with itself changes nothing, but it is
// noise in every rendered manifest and every golden file.
//
// Equality is exact on key, operator and value set. Deliberately not "same key
// wins": a target of gpu.product In [h100, gb200] on a fleet where discovery
// settled on gb200 alone is a genuinely broader constraint, and dropping the
// narrower gpuProducts term would let pods land on the wrong architecture.
func dedupeRequirements(reqs []corev1.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	if len(reqs) < 2 {
		return reqs
	}
	seen := make(map[string]struct{}, len(reqs))
	out := make([]corev1.NodeSelectorRequirement, 0, len(reqs))
	for _, r := range reqs {
		key := fmt.Sprintf("%s\x00%s\x00%q", r.Key, r.Operator, r.Values)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// BuildNodeAffinity assembles the job's required node affinity from the group's
// hostnames and the target.
//
// Under Pinned, hostnames is the group's node list and the target terms are
// strictly redundant: every one of those hosts already satisfied the target at
// discovery time. They are added anyway so the constraint is re-evaluated by the
// scheduler at bind time rather than trusted from discovery time, which closes
// the window where a node's labels change in between.
//
// Under Unpinned, hostnames is empty unless the target named explicit nodeNames,
// and the target terms are the only thing keeping pods inside the target set.
//
// Returns nil when there is nothing to constrain, so the caller leaves the pod's
// affinity untouched rather than writing an empty required term that matches
// every node.
func BuildNodeAffinity(hostnames []string, target *nvcrev1alpha1.TargetSpec, gpuProducts []string) *corev1.NodeAffinity {
	var reqs []corev1.NodeSelectorRequirement
	if len(hostnames) > 0 {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key:      hostnameLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   hostnames,
		})
	}
	reqs = append(reqs, TargetNodeAffinityTerms(target, gpuProducts)...)
	if len(reqs) == 0 {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			// One term, not one per requirement: terms OR, expressions AND.
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: reqs}},
		},
	}
}

// PinnedHostnames returns the hostnames a job should be constrained to.
//
// Under Pinned that is the group's node list. Under Unpinned the scheduler
// chooses, so the result is empty unless the target named explicit nodeNames, in
// which case that list is an explicit user-supplied host constraint and is
// honored.
func PinnedHostnames(placement string, groupNodes []string, target *nvcrev1alpha1.TargetSpec) []string {
	if !nvcrev1alpha1.IsUnpinned(placement) {
		return groupNodes
	}
	if target != nil && len(target.NodeNames) > 0 {
		return target.NodeNames
	}
	return nil
}

// dedupeSorted returns the distinct non-empty values in sorted order.
func dedupeSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// DistinctGPUProducts reads the nvidia.com/gpu.product label off the nodes that
// survived discovery. The values are stored verbatim because
// OrchestrationStatus.DetectedGPUArchitecture is normalized and lossy
// ("NVIDIA-H100-80GB-HBM3" becomes "h100") and so cannot be turned back into a
// label match.
//
// Exported for pkg/render, which has no OrchestrationStatus to read the recorded
// values back from and derives them from its own discovery instead.
func DistinctGPUProducts(nodes []corev1.Node) []string {
	values := make([]string, 0, len(nodes))
	for _, n := range nodes {
		values = append(values, n.Labels[gpuProductLabelKey])
	}
	return dedupeSorted(values)
}
