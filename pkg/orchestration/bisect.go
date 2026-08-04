// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"fmt"
	"sort"
)

// BisectInput holds inputs for one round of binary-search fault isolation.
type BisectInput struct {
	FailedGroups []Group           `json:"failedGroups"`
	MinGroupSize int               `json:"minGroupSize"`
	Round        int               `json:"round"`
	TopologyKey  string            `json:"topologyKey,omitempty"`
	NodeLabels   map[string]string `json:"nodeLabels,omitempty"` // node name → topology domain value
}

// BisectResult holds the output of one bisection round.
type BisectResult struct {
	Groups    []Group `json:"groups"`
	Converged bool    `json:"converged"`
}

// Bisect splits each failed group in half for the next round.
// Groups at or below minGroupSize are kept as-is (convergence).
// When TopologyKey is set, splits are done by topology domains to preserve locality.
// When TopologyKey is empty, nodes are split by name order.
func Bisect(input BisectInput) BisectResult {
	if len(input.FailedGroups) == 0 {
		return BisectResult{Converged: true}
	}

	minSize := max(input.MinGroupSize, 1)

	var groups []Group
	allConverged := true
	groupIdx := 0

	for i, fg := range input.FailedGroups {
		nodes := make([]string, len(fg.Nodes))
		copy(nodes, fg.Nodes)
		sort.Strings(nodes)

		// Already at or below minimum — can't split further.
		if len(nodes) <= minSize {
			groups = append(groups, Group{
				Name:  fmt.Sprintf("bisect-%d-%d", input.Round, i),
				Nodes: nodes,
			})
			groupIdx++
			continue
		}

		// Need to split — not converged.
		allConverged = false

		if input.TopologyKey != "" && len(input.NodeLabels) > 0 {
			// Topology-aware split: group nodes by domain, split domains at midpoint.
			splitGroups := bisectByTopology(nodes, input.NodeLabels, minSize, input.Round, groupIdx)
			groups = append(groups, splitGroups...)
			groupIdx += len(splitGroups)
		} else {
			// Simple split: sort by name, split at midpoint.
			mid := len(nodes) / 2
			groups = append(groups, Group{
				Name:  fmt.Sprintf("bisect-%d-%da", input.Round, i),
				Nodes: nodes[:mid],
			})
			groups = append(groups, Group{
				Name:  fmt.Sprintf("bisect-%d-%db", input.Round, i),
				Nodes: nodes[mid:],
			})
			groupIdx += 2
		}
	}

	return BisectResult{
		Groups:    groups,
		Converged: allConverged,
	}
}

// bisectByTopology splits nodes by topology domains rather than individual nodes.
// Domains are sorted by name and split at the midpoint. Each half gets all nodes
// from its domains. If only one domain exists, falls back to node-level split.
func bisectByTopology(nodes []string, nodeLabels map[string]string, _ int, round int, baseIdx int) []Group {
	// Group nodes by domain.
	domainNodes := make(map[string][]string)
	for _, node := range nodes {
		domain := nodeLabels[node]
		if domain == "" {
			domain = "__unknown__"
		}
		domainNodes[domain] = append(domainNodes[domain], node)
	}

	// Sort domains by name for determinism.
	domains := make([]string, 0, len(domainNodes))
	for d := range domainNodes {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	// Sort nodes within each domain.
	for _, d := range domains {
		sort.Strings(domainNodes[d])
	}

	// If only one domain, fall back to node-level split within it.
	if len(domains) == 1 {
		allNodes := domainNodes[domains[0]]
		mid := len(allNodes) / 2
		return []Group{
			{Name: fmt.Sprintf("bisect-%d-%da", round, baseIdx), Nodes: allNodes[:mid]},
			{Name: fmt.Sprintf("bisect-%d-%db", round, baseIdx), Nodes: allNodes[mid:]},
		}
	}

	// Split domain list at midpoint.
	mid := len(domains) / 2
	firstHalf := domains[:mid]
	secondHalf := domains[mid:]

	collectNodes := func(doms []string) []string {
		count := 0
		for _, d := range doms {
			count += len(domainNodes[d])
		}
		result := make([]string, 0, count)
		for _, d := range doms {
			result = append(result, domainNodes[d]...)
		}
		sort.Strings(result)
		return result
	}

	return []Group{
		{Name: fmt.Sprintf("bisect-%d-%da", round, baseIdx), Nodes: collectNodes(firstHalf)},
		{Name: fmt.Sprintf("bisect-%d-%db", round, baseIdx), Nodes: collectNodes(secondHalf)},
	}
}
