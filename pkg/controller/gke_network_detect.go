// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// GKE TCPXO network auto-detection for the GCP H100 override (issue #432).
//
// The TCPXO patch attaches the pod to the node's GPU NIC networks through the
// networking.gke.io/interfaces annotation, and GKE resolves every name in it
// to a Network object at admission. Whoever provisions the cluster picks those
// names (AICR uses "<deployment-id>-gpu-nic-N", Google's samples "vpc1".."vpc8"),
// so they are read from the target nodes instead of assumed. Detection never
// guesses: unless exactly GKETCPXONICsPerNode networks are allocatable on every
// target node, the catalog default (gpu-nic0..gpu-nic7) is rendered and the
// controller emits a Warning event saying why.

const (
	// gkeNetworkResourcePrefix is the extended resource GKE advertises on
	// every node attached to a multi-network Network, named after the Network
	// object. GKE also advertises "<name>.IP" alongside it.
	gkeNetworkResourcePrefix = "networking.gke.io.networks/"
	gkeNetworkIPSuffix       = ".IP"
	gkeDefaultNetwork        = "default"

	// annotationGKENorthInterfaces maps each additional network to its IP on
	// the node: [{"network": ..., "ipAddress": ...}].
	annotationGKENorthInterfaces = "networking.gke.io/north-interfaces"
	// annotationGKENICInfo maps each host NIC's IP to its name and PCI
	// address: [{"birthIP": ..., "birthName": "eth1", "pciAddress": ...}].
	annotationGKENICInfo = "networking.gke.io/nic-info"
)

// isGCPH100 reports whether the detected platform/architecture pair is the one
// the GCP H100 TCPXO override matches. Detection is gated on it so no other
// platform ever sees a detected list or a detection event.
func isGCPH100(platformName, gpuArch string) bool {
	return platformName == platform.GCP && gpuArch == "h100"
}

// gkeNetworkDetection is the outcome of one GKE TCPXO network detection pass,
// as resolved by resolveGKETCPXONetworks.
type gkeNetworkDetection struct {
	// Names are the detected networks in pod interface order (eth1, eth2, ...).
	// Empty when detection did not run or refused to pick.
	Names []string
	// Shared is the sorted list of networks allocatable on every target node.
	// It explains a refusal in gkeNetworkDetectionMessage.
	Shared []string
	// HostOrdered is true when Names follows the host NIC each network is
	// attached to (from the node annotations), false when it is sorted by name.
	HostOrdered bool
	// Ran is true only when the GCP H100 gate matched, which is exactly when
	// an empty Names should be surfaced to the user.
	Ran bool
}

// detectGKETCPXONetworks reads the GPU NIC networks from node allocatable.
// A network qualifies only when it is allocatable at a positive quantity on
// every node: GKE injects a limit of one of each attached network into the
// pod, so a node that lacks the network, or lists it at zero, never schedules
// it. The result is used only when exactly GKETCPXONICsPerNode networks
// qualify: fewer means the nodes are not A3 Mega or are missing networks, and
// more cannot be told apart from non-GPU networks attached to the same node
// pool.
//
// The order follows the host: when every node's annotations place every
// network on an ethN interface and all nodes agree, networks are ordered by
// that N, so the pod's eth1..eth8 carry the same networks as the host's.
// Otherwise they are sorted by name.
func detectGKETCPXONetworks(nodes []corev1.Node) gkeNetworkDetection {
	d := gkeNetworkDetection{Ran: true}
	if len(nodes) == 0 {
		return d
	}
	count := map[string]int{}
	for i := range nodes {
		for res, qty := range nodes[i].Status.Allocatable {
			name, ok := strings.CutPrefix(string(res), gkeNetworkResourcePrefix)
			if !ok || name == gkeDefaultNetwork || strings.HasSuffix(name, gkeNetworkIPSuffix) || qty.Sign() <= 0 {
				continue
			}
			count[name]++
		}
	}
	for name, c := range count {
		if c == len(nodes) {
			d.Shared = append(d.Shared, name)
		}
	}
	slices.Sort(d.Shared)
	if len(d.Shared) != catalog.GKETCPXONICsPerNode {
		return d
	}

	var order []string
	for i := range nodes {
		o := hostNICOrder(nodes[i], d.Shared)
		if o == nil || (order != nil && !slices.Equal(o, order)) {
			d.Names = slices.Clone(d.Shared)
			return d
		}
		order = o
	}
	d.Names = order
	d.HostOrdered = true
	return d
}

// hostNICOrder returns networks ordered by the host NIC each is attached to on
// node (eth1 before eth2), joining the north-interfaces and nic-info
// annotations on IP. It returns nil when either annotation is missing or
// malformed, or when any network does not land on its own ethN interface.
func hostNICOrder(node corev1.Node, networks []string) []string {
	var north []struct {
		Network   string `json:"network"`
		IPAddress string `json:"ipAddress"`
	}
	var nics []struct {
		BirthIP   string `json:"birthIP"`
		BirthName string `json:"birthName"`
	}
	if json.Unmarshal([]byte(node.Annotations[annotationGKENorthInterfaces]), &north) != nil ||
		json.Unmarshal([]byte(node.Annotations[annotationGKENICInfo]), &nics) != nil {
		return nil
	}
	ethByIP := map[string]int{}
	for _, nic := range nics {
		suffix, ok := strings.CutPrefix(nic.BirthName, "eth")
		if idx, err := strconv.Atoi(suffix); ok && err == nil {
			ethByIP[nic.BirthIP] = idx
		}
	}
	ethByNetwork := map[string]int{}
	for _, n := range north {
		if idx, ok := ethByIP[n.IPAddress]; ok {
			ethByNetwork[n.Network] = idx
		}
	}
	seen := map[int]bool{}
	for _, n := range networks {
		idx, ok := ethByNetwork[n]
		if !ok || seen[idx] {
			return nil
		}
		seen[idx] = true
	}
	ordered := slices.Clone(networks)
	slices.SortFunc(ordered, func(a, b string) int {
		return cmp.Compare(ethByNetwork[a], ethByNetwork[b])
	})
	return ordered
}

// resolveGKETCPXONetworks resolves the GKE networks the GCP H100 TCPXO patch
// attaches, the way every consumer (the Certification controller and the CLI
// dry-run path) must agree on: detection runs only for the GCP H100 target.
func resolveGKETCPXONetworks(platformName, gpuArch string, nodes []corev1.Node) gkeNetworkDetection {
	if !isGCPH100(platformName, gpuArch) {
		return gkeNetworkDetection{}
	}
	return detectGKETCPXONetworks(nodes)
}

// gkeNetworkDetectionMessage renders the user-facing explanation for a
// detection pass that refused to pick: which networks every target node
// shares, how many TCPXO needs, and the fallback that is rendered instead.
// Used verbatim as the GKENetworkDetection event message by the controller
// and printed by the CLI dry-run path.
func gkeNetworkDetectionMessage(d gkeNetworkDetection) string {
	defaults := catalog.DefaultGKETCPXONetworks()
	fallback := fmt.Sprintf("rendering the default networks %s..%s instead;"+
		" GKE rejects the pods unless Network objects with those names exist",
		defaults[0], defaults[len(defaults)-1])
	if len(d.Shared) == 0 {
		return fmt.Sprintf("GKE TCPXO network auto-detection found no %s* resource"+
			" allocatable on every target node, but TCPXO needs %d GPU NIC networks"+
			" per node (a3-megagpu-8g); %s.", gkeNetworkResourcePrefix, catalog.GKETCPXONICsPerNode, fallback)
	}
	return fmt.Sprintf("GKE TCPXO network auto-detection found %d networks allocatable on every"+
		" target node (%s), but TCPXO needs exactly %d GPU NIC networks per node"+
		" (a3-megagpu-8g); %s.", len(d.Shared), strings.Join(d.Shared, ", "),
		catalog.GKETCPXONICsPerNode, fallback)
}
