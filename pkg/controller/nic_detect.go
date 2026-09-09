// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// NIC resource auto-detection for the on-prem GB200/GB300 override (ADR-075).
//
// The override injects an RDMA NIC resource request only when a resource name
// is known, and the name depends on the device plugin a site runs, so there is
// no safe default. The spec field (nicResourceName) always wins; when it is
// unset, detection fills the gap from node allocatable, and it never guesses:
// zero or multiple qualifying candidates means nothing is injected, exactly
// today's field-unset behavior.

const (
	// nicResourcePrefixRDMA admits any extended resource under the rdma/
	// domain (rdma/ib, rdma/shared_ib, rdma/roce_gdr, ...), the convention
	// the k8s-rdma-shared-dev-plugin family uses.
	nicResourcePrefixRDMA = "rdma/"
	// nicResourceMlnxNics is the one exact-name candidate outside the rdma/
	// domain: the name the Mellanox host-device plugin advertises, already
	// consumed by the Azure override.
	nicResourceMlnxNics = "nvidia.com/mlnxnics"
)

// isOnPremNVL72 reports whether the detected platform/architecture pair is
// the one the ADR-075 on-prem GB200/GB300 override matches. NIC detection is
// gated on it so no other platform ever sees a detected name or a detection
// event: nicResourceName is only consumed by that override's dep fragments.
func isOnPremNVL72(platformName, gpuArch string) bool {
	return platformName == platformOnPrem && (gpuArch == "gb200" || gpuArch == "gb300")
}

// detectNICResource inspects node allocatable for the RDMA NIC extended
// resource the on-prem override should request. The candidate set is small
// and deliberate: any resource name starting with "rdma/" plus the exact name
// "nvidia.com/mlnxnics". A candidate qualifies only when its allocatable
// count is greater than zero on every node in nodes; a node that omits the
// resource, or reports zero, disqualifies it, because injecting a request one
// node cannot satisfy leaves that node's pods permanently Pending, the exact
// failure ADR-058 called out.
//
// Returns the detected name and the qualifying candidates, sorted. The name
// is non-empty only when exactly one candidate qualifies; with zero or two or
// more, detection refuses to guess and returns "" so the caller injects
// nothing. An empty node list yields no candidates.
func detectNICResource(nodes []corev1.Node) (string, []string) {
	if len(nodes) == 0 {
		return "", nil
	}
	counts := map[string]int{}
	for i := range nodes {
		for res, qty := range nodes[i].Status.Allocatable {
			name := string(res)
			if !strings.HasPrefix(name, nicResourcePrefixRDMA) && name != nicResourceMlnxNics {
				continue
			}
			if qty.Sign() > 0 {
				counts[name]++
			}
		}
	}
	var qualifying []string
	for name, count := range counts {
		if count == len(nodes) {
			qualifying = append(qualifying, name)
		}
	}
	slices.Sort(qualifying)
	if len(qualifying) == 1 {
		return qualifying[0], qualifying
	}
	return "", qualifying
}

// resolveNICResourceName resolves the effective NIC resource name the way
// every consumer (both controllers and the CLI dry-run paths) must agree on:
// the user-supplied field always wins; otherwise detection runs only for the
// on-prem GB200/GB300 target the override matches, and only a single
// qualifying candidate is used.
//
// detectionRan is true only when the field was unset and the gate matched,
// which is exactly when a zero-or-ambiguous result (name == "") should be
// surfaced to the user via nicDetectionMessage.
func resolveNICResourceName(
	field *string, platformName, gpuArch string, nodes []corev1.Node,
) (name string, candidates []string, detectionRan bool) {
	if field != nil {
		return *field, nil, false
	}
	if !isOnPremNVL72(platformName, gpuArch) {
		return "", nil, false
	}
	name, candidates = detectNICResource(nodes)
	return name, candidates, true
}

// nicDetectionMessage renders the user-facing explanation for a detection
// pass that refused to pick a name: what qualified (or that nothing did) and
// which field resolves it. Used verbatim as the NICResourceDetection event
// message by the controllers and printed by the CLI dry-run paths.
func nicDetectionMessage(candidates []string) string {
	if len(candidates) == 0 {
		return "NIC resource auto-detection found no RDMA extended resource" +
			" (rdma/* or nvidia.com/mlnxnics) allocatable on every target node;" +
			" no NIC resource is requested. Set nicResourceName to request one."
	}
	return fmt.Sprintf("NIC resource auto-detection found multiple candidates"+
		" allocatable on every target node (%s); no NIC resource is requested."+
		" Set nicResourceName to choose one.", strings.Join(candidates, ", "))
}
