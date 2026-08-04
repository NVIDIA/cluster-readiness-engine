// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package orchestration

import (
	"fmt"
	"sort"
)

// PartitionInput holds inputs for node partitioning.
type PartitionInput struct {
	Nodes        []NodeInfo // discovered target nodes
	NodesPerJob  int        // from adapter.NodesRequired()
	TopologyKey  string     // from spec.orchestration.topology.topologyKey (empty = no topology)
	StrictDomain bool       // when true, one group per domain at its natural size (no borrowing)
}

// NodeInfo describes a node for partitioning.
type NodeInfo struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Group is the output of partitioning — a set of nodes for one Job.
type Group struct {
	Name     string   `json:"name"`
	Nodes    []string `json:"nodes"`
	Domains  []string `json:"domains,omitempty"`
	Overflow bool     `json:"overflow,omitempty"`
}

// domainEntry pairs a topology domain name with its nodes.
type domainEntry struct {
	name  string
	nodes []NodeInfo
}

// PartitionNodes divides nodes into groups.
// When TopologyKey is set, groups are packed into the minimum number of topology domains.
// When TopologyKey is empty, nodes are chunked by name order.
// Every target node appears in at least one group (coverage guarantee).
func PartitionNodes(input PartitionInput) ([]Group, error) {
	if input.NodesPerJob < 1 {
		return nil, fmt.Errorf("nodesPerJob must be >= 1, got %d", input.NodesPerJob)
	}
	if len(input.Nodes) == 0 {
		return nil, fmt.Errorf("no nodes provided")
	}

	// Sort nodes by name once at the boundary so all partition strategies
	// receive deterministic input. Upstream callers may pass nodes in
	// arbitrary order (e.g., client.List returns items unsorted). Copy first
	// to avoid mutating the caller's slice.
	sorted := make([]NodeInfo, len(input.Nodes))
	copy(sorted, input.Nodes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	input.Nodes = sorted

	if input.TopologyKey != "" {
		return partitionTopology(input)
	}
	return partitionSimple(input)
}

// partitionSimple chunks nodes by name order without topology awareness.
// Assumes input.Nodes is already sorted by name (guaranteed by PartitionNodes).
func partitionSimple(input PartitionInput) ([]Group, error) {
	names := make([]string, len(input.Nodes))
	for i, n := range input.Nodes {
		names[i] = n.Name
	}

	npj := input.NodesPerJob

	// Create full groups.
	fullCount := len(names) / npj
	groupCap := fullCount
	if len(names)%npj > 0 {
		groupCap++
	}
	groups := make([]Group, 0, groupCap)
	for i := range fullCount {
		groups = append(groups, Group{
			Name:  fmt.Sprintf("group-%d", i),
			Nodes: names[i*npj : (i+1)*npj],
		})
	}

	remainder := len(names) % npj
	if remainder > 0 {
		if fullCount == 0 {
			// Not enough nodes for even one full group — create a single partial group.
			groups = append(groups, Group{
				Name:  "group-0",
				Nodes: names,
			})
		} else {
			// Create overflow group: remainder nodes + borrow from last full group to fill.
			borrowCount := npj - remainder
			lastFull := &groups[fullCount-1]
			borrowed := lastFull.Nodes[len(lastFull.Nodes)-borrowCount:]

			overflowNodes := make([]string, 0, npj)
			overflowNodes = append(overflowNodes, names[fullCount*npj:]...)
			overflowNodes = append(overflowNodes, borrowed...)
			sort.Strings(overflowNodes)

			groups = append(groups, Group{
				Name:     fmt.Sprintf("group-%d-overflow", fullCount),
				Nodes:    overflowNodes,
				Overflow: true,
			})
		}
	}

	return groups, nil
}

// partitionTopology groups nodes by topology domain and creates groups that
// pack into the minimum number of domains.
// Assumes input.Nodes is already sorted by name (guaranteed by PartitionNodes).
func partitionTopology(input PartitionInput) ([]Group, error) {
	npj := input.NodesPerJob
	key := input.TopologyKey

	// Group nodes by domain.
	domainMap := make(map[string][]NodeInfo)
	for _, n := range input.Nodes {
		domain := n.Labels[key]
		if domain == "" {
			return nil, fmt.Errorf("node %q missing topology label %q", n.Name, key)
		}
		domainMap[domain] = append(domainMap[domain], n)
	}

	// Build the domains slice. Nodes within each domain are already in sorted
	// order because input.Nodes is pre-sorted by PartitionNodes. Map iteration
	// order is random though, so the domains slice itself still needs sorting.
	domains := make([]domainEntry, 0, len(domainMap))
	for name, nodes := range domainMap {
		domains = append(domains, domainEntry{name: name, nodes: nodes})
	}
	// Sort domains by size descending, then by name for determinism.
	sort.Slice(domains, func(i, j int) bool {
		if len(domains[i].nodes) != len(domains[j].nodes) {
			return len(domains[i].nodes) > len(domains[j].nodes)
		}
		return domains[i].name < domains[j].name
	})

	// Strict domain mode: one group per domain at its natural size.
	// Used by intra-rack to guarantee groups never cross domains.
	if input.StrictDomain {
		var groups []Group
		for i, d := range domains {
			names := make([]string, len(d.nodes))
			for j, n := range d.nodes {
				names[j] = n.Name
			}
			groups = append(groups, Group{
				Name:    fmt.Sprintf("group-%d", i),
				Nodes:   names,
				Domains: []string{d.name},
			})
		}
		return groups, nil
	}

	// Build pools of unassigned nodes per domain.
	pools := make(map[string][]string) // domain -> unassigned node names
	for _, d := range domains {
		names := make([]string, len(d.nodes))
		for i, n := range d.nodes {
			names[i] = n.Name
		}
		pools[d.name] = names
	}

	// Compute the minimum number of domains needed to fill a group.
	// ceil(nodesPerJob / largestDomainSize) — prevents spreading a job across
	// more racks than necessary.
	maxDomainSize := len(domains[0].nodes) // domains sorted desc by size
	maxDomainsPerGroup := (npj + maxDomainSize - 1) / maxDomainSize

	// Greedy allocation: for each group, pick domains with the most unassigned nodes.
	var groups []Group
	groupIdx := 0

	for {
		// Find domains with unassigned nodes, sorted by pool size desc.
		type poolEntry struct {
			domain string
			count  int
		}
		var available []poolEntry
		for _, d := range domains {
			if len(pools[d.name]) > 0 {
				available = append(available, poolEntry{domain: d.name, count: len(pools[d.name])})
			}
		}
		sort.Slice(available, func(i, j int) bool {
			if available[i].count != available[j].count {
				return available[i].count > available[j].count
			}
			return available[i].domain < available[j].domain
		})

		// Count total unassigned.
		totalUnassigned := 0
		for _, a := range available {
			totalUnassigned += a.count
		}
		if totalUnassigned == 0 {
			break
		}

		// If not enough for a full group, this becomes the overflow group.
		if totalUnassigned < npj {
			break
		}

		// Check if the top maxDomainsPerGroup domains can fill a group.
		// If not, remaining nodes go to overflow.
		topCapacity := 0
		for i, a := range available {
			if i >= maxDomainsPerGroup {
				break
			}
			topCapacity += a.count
		}
		if topCapacity < npj {
			break
		}

		// Take nodes greedily from domains with the most unassigned,
		// limited to maxDomainsPerGroup domains per group.
		var groupNodes []string
		var groupDomains []string
		remaining := npj
		domainsUsed := 0

		for _, a := range available {
			if remaining <= 0 {
				break
			}
			if domainsUsed >= maxDomainsPerGroup {
				break
			}
			pool := pools[a.domain]
			take := min(remaining, len(pool))
			groupNodes = append(groupNodes, pool[:take]...)
			pools[a.domain] = pool[take:]
			groupDomains = append(groupDomains, a.domain)
			remaining -= take
			domainsUsed++
		}

		sort.Strings(groupNodes)
		sort.Strings(groupDomains)

		groups = append(groups, Group{
			Name:    fmt.Sprintf("group-%d", groupIdx),
			Nodes:   groupNodes,
			Domains: groupDomains,
		})
		groupIdx++
	}

	// Handle overflow: create overflow groups for untested nodes, each
	// constrained to maxDomainsPerGroup racks. Reuse already-tested nodes
	// from the same racks to fill to npj.
	nodeToLabel := make(map[string]string)
	for _, n := range input.Nodes {
		nodeToLabel[n.Name] = n.Labels[key]
	}

	// Build per-domain pools: untested nodes first, then reusable (already assigned).
	untested := make(map[string]bool)
	for _, d := range domains {
		for _, n := range pools[d.name] {
			untested[n] = true
		}
	}

	if len(untested) > 0 && len(groups) == 0 {
		// Not enough nodes for even one full group — create a single partial group.
		var partialNodes []string
		for n := range untested {
			partialNodes = append(partialNodes, n)
		}
		sort.Strings(partialNodes)
		groups = append(groups, Group{
			Name:    "group-0",
			Nodes:   partialNodes,
			Domains: domainsForNodes(partialNodes, nodeToLabel),
		})
		return groups, nil
	}

	if len(untested) > 0 {
		groups = buildOverflowGroups(groups, groupIdx, domains, untested, npj)
	}

	return groups, nil
}

// buildOverflowGroups creates overflow groups for untested nodes, each constrained
// to maxDomains racks. Reuses already-tested nodes from the same racks to fill to npj.
func buildOverflowGroups(
	groups []Group, groupIdx int,
	domains []domainEntry, untested map[string]bool,
	npj int,
) []Group {
	// Build rack pools: all nodes per domain, with untested nodes first.
	type rackPool struct {
		domain   string
		nodes    []string // untested first, then reusable
		untested int      // count of untested nodes in this pool
	}

	allAssigned := make(map[string]bool)
	for _, g := range groups {
		for _, n := range g.Nodes {
			allAssigned[n] = true
		}
	}

	rackPools := make([]rackPool, len(domains))
	for i, d := range domains {
		rackPools[i].domain = d.name
		var reusable []string
		for _, n := range d.nodes {
			if untested[n.Name] {
				rackPools[i].nodes = append(rackPools[i].nodes, n.Name)
				rackPools[i].untested++
			} else {
				reusable = append(reusable, n.Name)
			}
		}
		// Untested nodes first, then reusable.
		rackPools[i].nodes = append(rackPools[i].nodes, reusable...)
	}

	overflowIdx := 0
	for {
		// Count remaining untested.
		totalUntested := 0
		for i := range rackPools {
			rackPools[i].untested = 0
			for _, n := range rackPools[i].nodes {
				if untested[n] {
					rackPools[i].untested++
				}
			}
			totalUntested += rackPools[i].untested
		}
		if totalUntested == 0 {
			break
		}

		// Sort: prioritize racks with the most untested nodes, then by total capacity.
		sort.Slice(rackPools, func(i, j int) bool {
			if rackPools[i].untested != rackPools[j].untested {
				return rackPools[i].untested > rackPools[j].untested
			}
			if len(rackPools[i].nodes) != len(rackPools[j].nodes) {
				return len(rackPools[i].nodes) > len(rackPools[j].nodes)
			}
			return rackPools[i].domain < rackPools[j].domain
		})

		// Pick racks to fill the group, starting with those that have untested
		// nodes. Use the minimum number of racks needed to reach npj.
		var groupNodes []string
		var groupDomains []string

		for i := range rackPools {
			if len(groupNodes) >= npj {
				break
			}
			if len(rackPools[i].nodes) == 0 {
				continue
			}

			take := min(npj-len(groupNodes), len(rackPools[i].nodes))
			taken := rackPools[i].nodes[:take]
			rackPools[i].nodes = rackPools[i].nodes[take:]
			groupNodes = append(groupNodes, taken...)
			groupDomains = append(groupDomains, rackPools[i].domain)
		}

		if len(groupNodes) == 0 {
			break
		}

		// Mark taken untested nodes as tested.
		for _, n := range groupNodes {
			delete(untested, n)
		}

		sort.Strings(groupNodes)
		sort.Strings(groupDomains)

		groups = append(groups, Group{
			Name:     fmt.Sprintf("group-%d-overflow", groupIdx+overflowIdx),
			Nodes:    groupNodes,
			Domains:  groupDomains,
			Overflow: true,
		})
		overflowIdx++
	}

	return groups
}

// domainsForNodes returns sorted unique domain labels for the given nodes.
func domainsForNodes(nodes []string, nodeToLabel map[string]string) []string {
	set := make(map[string]bool)
	for _, n := range nodes {
		set[nodeToLabel[n]] = true
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}
