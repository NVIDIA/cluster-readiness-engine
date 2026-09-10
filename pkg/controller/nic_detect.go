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

// nicDetection is the outcome of one NIC resource auto-detection pass, as
// resolved by resolveNICResourceName.
type nicDetection struct {
	// Name is the detected resource name; non-empty only when exactly one
	// candidate qualifies (or the field was set, which bypasses detection).
	Name string
	// Qualifying is the sorted list of candidates allocatable at Required on
	// every target node.
	Qualifying []string
	// Present is the sorted list of candidate names listed in allocatable on
	// at least one node, at any quantity (including zero). It distinguishes
	// "no candidate resource exists at all" from "candidates exist but none
	// covers the requested count on every node" in nicDetectionMessage.
	Present []string
	// Required is the per-container request count candidates were qualified
	// against: the resolved mlnxPerNode, floored at one.
	Required int64
	// Ran is true only when the field was unset and the on-prem GB200/GB300
	// gate matched, which is exactly when a zero-or-ambiguous result
	// (Name == "") should be surfaced to the user via nicDetectionMessage.
	Ran bool
}

// detectNICResource inspects node allocatable for the RDMA NIC extended
// resource the on-prem override should request. The candidate set is small
// and deliberate: any resource name starting with "rdma/" plus the exact name
// "nvidia.com/mlnxnics". A candidate qualifies only when its allocatable
// count is at least required — the resolved mlnxPerNode the templates will
// request per container — on every node in nodes; a node that omits the
// resource, or reports fewer than required, disqualifies it, because
// injecting a request one node cannot satisfy leaves that node's pods
// permanently Pending, the exact failure ADR-058 called out.
//
// Returns the detected name, the qualifying candidates, and every candidate
// name present in any node's allocatable (all sorted). The name is non-empty
// only when exactly one candidate qualifies; with zero or two or more,
// detection refuses to guess and returns "" so the caller injects nothing.
// An empty node list yields no candidates.
func detectNICResource(nodes []corev1.Node, required int64) (name string, qualifying, present []string) {
	if len(nodes) == 0 {
		return "", nil, nil
	}
	covered := map[string]int{}
	seen := map[string]bool{}
	for i := range nodes {
		for res, qty := range nodes[i].Status.Allocatable {
			rn := string(res)
			if !strings.HasPrefix(rn, nicResourcePrefixRDMA) && rn != nicResourceMlnxNics {
				continue
			}
			seen[rn] = true
			if qty.CmpInt64(required) >= 0 {
				covered[rn]++
			}
		}
	}
	for rn, count := range covered {
		if count == len(nodes) {
			qualifying = append(qualifying, rn)
		}
	}
	for rn := range seen {
		present = append(present, rn)
	}
	slices.Sort(qualifying)
	slices.Sort(present)
	if len(qualifying) == 1 {
		return qualifying[0], qualifying, present
	}
	return "", qualifying, present
}

// resolveNICResourceName resolves the effective NIC resource name the way
// every consumer (both controllers and the CLI dry-run paths) must agree on:
// the user-supplied field always wins; otherwise detection runs only for the
// on-prem GB200/GB300 target the override matches, and only a single
// candidate allocatable at the resolved mlnxPerNode count on every node is
// used. mlnxPerNode must be the caller's fully resolved value (field or
// catalog default), because that is exactly what the dep fragments request
// per container; it is floored at one so a zero count still requires the
// resource to be allocatable at all.
func resolveNICResourceName(
	field *string, platformName, gpuArch string, nodes []corev1.Node, mlnxPerNode int32,
) nicDetection {
	if field != nil {
		return nicDetection{Name: *field}
	}
	if !isOnPremNVL72(platformName, gpuArch) {
		return nicDetection{}
	}
	required := int64(mlnxPerNode)
	if required < 1 {
		required = 1
	}
	name, qualifying, present := detectNICResource(nodes, required)
	return nicDetection{
		Name:       name,
		Qualifying: qualifying,
		Present:    present,
		Required:   required,
		Ran:        true,
	}
}

// nicDetectionMessage renders the user-facing explanation for a detection
// pass that refused to pick a name: whether the ambiguity was multiple
// qualifying candidates, no candidate resource on any node, or candidates
// that exist but cannot cover the requested count on every node — and which
// field resolves it. Used verbatim as the NICResourceDetection event message
// by the controllers and printed by the CLI dry-run paths.
func nicDetectionMessage(d nicDetection) string {
	switch {
	case len(d.Qualifying) > 1:
		return fmt.Sprintf("NIC resource auto-detection found multiple candidates"+
			" allocatable on every target node (%s); no NIC resource is requested."+
			" Set nicResourceName to choose one.", strings.Join(d.Qualifying, ", "))
	case len(d.Present) == 0:
		return "NIC resource auto-detection found no RDMA extended resource" +
			" (rdma/* or nvidia.com/mlnxnics) on any target node;" +
			" no NIC resource is requested. Set nicResourceName to request one."
	default:
		return fmt.Sprintf("NIC resource auto-detection found candidates (%s), but"+
			" none is allocatable at the requested count %d on every target node;"+
			" no NIC resource is requested. Set nicResourceName to request one,"+
			" or adjust mlnxPerNode.", strings.Join(d.Present, ", "), d.Required)
	}
}
