// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderesults

import (
	"sort"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// NodesWithFailureDetails wraps a list of node names into FailedNode entries with the
// given reason and message. The result is sorted by node name.
func NodesWithFailureDetails(names []string, reason burninv1alpha1.NodeFailureReason,
	message string) []burninv1alpha1.FailedNode {
	if len(names) == 0 {
		return nil
	}
	out := make([]burninv1alpha1.FailedNode, 0, len(names))
	for _, name := range names {
		out = append(out, burninv1alpha1.FailedNode{Name: name, Reason: reason, Message: message})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FailedNodeNames extracts the sorted, deduped node names from FailedNode entries.
func FailedNodeNames(nodes []burninv1alpha1.FailedNode) []string {
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
