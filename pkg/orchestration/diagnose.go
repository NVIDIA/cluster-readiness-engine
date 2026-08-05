// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"fmt"
	"math"
	"sort"
)

// DiagnoseScreenInput holds inputs for Stage 1 screening group generation.
type DiagnoseScreenInput struct {
	// Nodes is the full set of target nodes with their topology labels.
	Nodes []NodeInfo `json:"nodes"`
	// TopologyKey is the node label key for topology-aware grouping.
	// When empty, nodes are grouped into disjoint chunks of ceil(sqrt(N)).
	TopologyKey string `json:"topologyKey,omitempty"`
}

// ScreenGroups generates Stage 1a screening groups for adaptive fault isolation.
// With a topology key, each unique domain value becomes one group.
// Without a topology key, nodes are split into disjoint groups of ceil(sqrt(N)).
func ScreenGroups(input DiagnoseScreenInput) ([]Group, error) {
	if len(input.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes provided")
	}

	if input.TopologyKey != "" {
		return screenByTopology(input.Nodes, input.TopologyKey)
	}
	return screenBySize(input.Nodes)
}

// screenByTopology groups nodes by their topology domain label value.
// Each unique label value becomes one screening group.
func screenByTopology(nodes []NodeInfo, topologyKey string) ([]Group, error) {
	domains := map[string][]string{}
	for _, n := range nodes {
		domain := n.Labels[topologyKey]
		if domain == "" {
			domain = "unknown"
		}
		domains[domain] = append(domains[domain], n.Name)
	}

	// Sort domain names for deterministic output.
	domainNames := make([]string, 0, len(domains))
	for d := range domains {
		domainNames = append(domainNames, d)
	}
	sort.Strings(domainNames)

	groups := make([]Group, 0, len(domainNames))
	for i, d := range domainNames {
		nodeNames := domains[d]
		sort.Strings(nodeNames)
		groups = append(groups, Group{
			Name:    fmt.Sprintf("screen-%d", i),
			Nodes:   nodeNames,
			Domains: []string{d},
		})
	}
	return groups, nil
}

// screenBySize groups nodes into disjoint chunks of ceil(sqrt(N)).
// This is optimal for unknown defective count: minimizes the maximum
// of Stage 1 groups and Stage 2 work.
func screenBySize(nodes []NodeInfo) ([]Group, error) {
	n := len(nodes)
	k := max(int(math.Ceil(math.Sqrt(float64(n)))), 2)

	// Sort nodes by name for deterministic output.
	sorted := make([]NodeInfo, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var groups []Group
	for i := 0; i < n; i += k {
		end := min(i+k, n)
		var nodeNames []string
		for _, node := range sorted[i:end] {
			nodeNames = append(nodeNames, node.Name)
		}
		groups = append(groups, Group{
			Name:  fmt.Sprintf("screen-%d", len(groups)),
			Nodes: nodeNames,
		})
	}
	return groups, nil
}

// BothHalvesPassResult represents a bisection parent group where both
// child halves passed individually, indicating an infrastructure fault.
type BothHalvesPassResult struct {
	// Domain is the topology domain of the parent group (if any).
	Domain string
	// HalfA is the node list of the first child that passed.
	HalfA []string
	// HalfB is the node list of the second child that passed.
	HalfB []string
}

// DetectBothHalvesPass identifies bisection sibling pairs where both halves
// succeeded. Sibling groups are identified by the naming convention:
// bisect-R-Ia and bisect-R-Ib share parent I.
type GroupResult struct {
	Name    string
	Nodes   []string
	Domains []string
	Passed  bool
}

// DetectBothHalvesPass finds pairs of sibling bisection groups where both
// passed, indicating a potential infrastructure fault at the boundary.
func DetectBothHalvesPass(groups []GroupResult) []BothHalvesPassResult {
	// Index groups by their parent prefix (everything before the last character).
	type sibling struct {
		a, b *GroupResult
	}
	parents := map[string]*sibling{}

	for i := range groups {
		g := &groups[i]
		name := g.Name
		if len(name) < 2 {
			continue
		}
		suffix := name[len(name)-1]
		if suffix != 'a' && suffix != 'b' {
			continue
		}
		parent := name[:len(name)-1]
		s, ok := parents[parent]
		if !ok {
			s = &sibling{}
			parents[parent] = s
		}
		if suffix == 'a' {
			s.a = g
		} else {
			s.b = g
		}
	}

	var results []BothHalvesPassResult
	// Sort parent keys for deterministic output.
	parentKeys := make([]string, 0, len(parents))
	for k := range parents {
		parentKeys = append(parentKeys, k)
	}
	sort.Strings(parentKeys)

	for _, k := range parentKeys {
		s := parents[k]
		if s.a == nil || s.b == nil {
			continue
		}
		if s.a.Passed && s.b.Passed {
			domain := ""
			if len(s.a.Domains) > 0 {
				domain = s.a.Domains[0]
			}
			results = append(results, BothHalvesPassResult{
				Domain: domain,
				HalfA:  s.a.Nodes,
				HalfB:  s.b.Nodes,
			})
		}
	}
	return results
}

// BuildCrossBoundaryGroups creates two non-overlapping mixed groups from
// two halves of a bisection group. Mix-1 takes the first half of each side,
// Mix-2 takes the second half. Both can run in parallel (no node overlap).
func BuildCrossBoundaryGroups(halfA, halfB []string, probeIdx int) []Group {
	midA := len(halfA) / 2
	midB := len(halfB) / 2
	if midA == 0 {
		midA = 1
	}
	if midB == 0 {
		midB = 1
	}

	mix1 := make([]string, 0, midA+midB)
	mix1 = append(mix1, halfA[:midA]...)
	mix1 = append(mix1, halfB[:midB]...)

	mix2 := make([]string, 0, len(halfA)-midA+len(halfB)-midB)
	mix2 = append(mix2, halfA[midA:]...)
	mix2 = append(mix2, halfB[midB:]...)

	groups := []Group{
		{Name: fmt.Sprintf("cross-%d-mix0", probeIdx), Nodes: mix1},
	}
	if len(mix2) >= 2 {
		groups = append(groups, Group{Name: fmt.Sprintf("cross-%d-mix1", probeIdx), Nodes: mix2})
	}
	return groups
}

// BuildConfirmationGroups creates one group per suspect node, each paired
// with a known-healthy node for confirmation testing.
func BuildConfirmationGroups(suspectNodes []string, healthyNodes []string) []Group {
	if len(healthyNodes) == 0 || len(suspectNodes) == 0 {
		return nil
	}
	// Rotate healthy reference nodes so confirmation groups don't all share
	// the same node. This allows parallel execution (no node overlap).
	groups := make([]Group, len(suspectNodes))
	for i, suspect := range suspectNodes {
		healthyRef := healthyNodes[i%len(healthyNodes)]
		groups[i] = Group{
			Name:  fmt.Sprintf("confirm-%d", i),
			Nodes: []string{suspect, healthyRef},
		}
	}
	return groups
}
