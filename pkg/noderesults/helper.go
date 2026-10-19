// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderesults

import (
	"sort"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// NodesWithFailureDetails wraps a list of node names into FailedNode entries with the
// given reason and message. The result is sorted by node name.
//
// Every node receives the same message. That is correct when the evidence is
// group-scoped (a collective failed, and the run knows only that it failed), and
// it is the shape every caller but the node-health detector needs. A caller that
// holds genuine per-node evidence should use NodesWithPerNodeDetails instead
// rather than fanning one message across nodes it does not describe.
func NodesWithFailureDetails(names []string, reason nvcrev1alpha1.NodeFailureReason,
	message string) []nvcrev1alpha1.FailedNode {
	return NodesWithPerNodeDetails(names, reason, func(string) string { return message })
}

// NodesWithPerNodeDetails wraps a list of node names into FailedNode entries with
// the given reason, calling msgFor to build each node's own message. The result is
// sorted by node name.
//
// FailedNodes is a list-map keyed on (name, reason), so a node carries at most one
// message per reason; a caller producing two messages for the same pair keeps
// whichever the merge order lands on.
func NodesWithPerNodeDetails(names []string, reason nvcrev1alpha1.NodeFailureReason,
	msgFor func(string) string) []nvcrev1alpha1.FailedNode {
	if len(names) == 0 {
		return nil
	}
	out := make([]nvcrev1alpha1.FailedNode, 0, len(names))
	for _, name := range names {
		message := ""
		if msgFor != nil {
			message = msgFor(name)
		}
		out = append(out, nvcrev1alpha1.FailedNode{Name: name, Reason: reason, Message: message})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FailedNodeNames extracts the sorted, deduped node names from FailedNode entries.
func FailedNodeNames(nodes []nvcrev1alpha1.FailedNode) []string {
	if len(nodes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(nodes))
	var names []string
	for _, fn := range nodes {
		if fn.Name == "" {
			continue
		}
		if _, ok := seen[fn.Name]; ok {
			continue
		}
		seen[fn.Name] = struct{}{}
		names = append(names, fn.Name)
	}
	sort.Strings(names)
	return names
}
