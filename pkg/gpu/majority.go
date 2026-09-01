// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import corev1 "k8s.io/api/core/v1"

// productLabel is the node label that reports the GPU product installed on a
// node, e.g. "NVIDIA-H100-80GB-HBM3".
const productLabel = "nvidia.com/gpu.product"

// MajorityArchitecture returns the GPU architecture reported by the most
// nodes, parsed from each node's nvidia.com/gpu.product label via
// ParseProduct. It is the single source of truth for architecture detection:
// the controllers and every CLI path (render, cluster info, workloadrun)
// derive gpusPerNode, MNNVL, and override matching from its result, so a
// mixed-architecture target resolves to the same architecture everywhere
// (issue #248).
//
// Only labeled nodes vote. A node without the label (or with an empty value)
// never outvotes labeled nodes, so a fleet where the GPU feature-discovery
// labeler has not reached every node still detects the architecture the
// labeled nodes report. The function returns "" only when no node is labeled
// (including an empty input); callers map that to their own "unknown"
// sentinel or fallback.
//
// The winner is derived by walking the slice in order with a strictly-greater
// count comparison (the counts map is only indexed, never iterated), so the
// result is deterministic for a given node order and ties resolve to the
// architecture whose earliest node appears first. Callers pass name-sorted
// nodes, which makes the result stable for a given cluster. This is the exact
// tie-break the Workflow and Certification controllers adopted for issue #77.
func MajorityArchitecture(nodes []corev1.Node) string {
	counts := map[string]int{}
	for _, n := range nodes {
		if arch := ParseProduct(n.Labels[productLabel]); arch != "" {
			counts[arch]++
		}
	}
	primary := ""
	for _, n := range nodes {
		arch := ParseProduct(n.Labels[productLabel])
		if arch == "" {
			continue
		}
		if primary == "" || counts[arch] > counts[primary] {
			primary = arch
		}
	}
	return primary
}
