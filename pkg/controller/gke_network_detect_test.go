// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// The GCP H100 TCPXO patch attaches the pod to the node's GPU NIC networks by
// name, and GKE rejects the pod at admission when a name has no Network object
// (issue #432). These cases pin the detection rule: the gate (gcp + h100) keeps
// other platforms untouched, ".IP" and "default" resources are ignored, a
// network must be allocatable on every node, exactly eight must qualify, and
// the order follows the host NIC each network sits on when every node's
// annotations agree, else the names sort. Anything but eight resolves to no
// names, with the message the controller emits as a GKENetworkDetection event.
func TestResolveGKETCPXONetworks(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-gke-tcpxo-networks",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Platform        string `yaml:"platform"`
			GPUArchitecture string `yaml:"gpuArchitecture"`
			Nodes           []struct {
				Name        string            `yaml:"name"`
				Allocatable map[string]string `yaml:"allocatable"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{
				Name:        n.Name,
				Annotations: n.Annotations,
				Status:      corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
			}
			for res, qty := range n.Allocatable {
				parsed, err := resource.ParseQuantity(qty)
				if err != nil {
					return err
				}
				node.Status.Allocatable[corev1.ResourceName(res)] = parsed
			}
			nodes = append(nodes, node)
		}

		d := resolveGKETCPXONetworks(input.Platform, input.GPUArchitecture, nodes)
		networks := d.Names
		if networks == nil {
			networks = []string{}
		}
		shared := d.Shared
		if shared == nil {
			shared = []string{}
		}

		out := struct {
			DetectionRan bool     `json:"detectionRan"`
			Networks     []string `json:"networks"`
			HostOrdered  bool     `json:"hostOrdered"`
			Shared       []string `json:"shared"`
			// Message is set exactly when the controller emits the
			// GKENetworkDetection event / the dry-run note: detection ran
			// and refused to pick.
			Message string `json:"message,omitempty"`
		}{DetectionRan: d.Ran, Networks: networks, HostOrdered: d.HostOrdered, Shared: shared}
		if d.Ran && len(d.Names) == 0 {
			out.Message = gkeNetworkDetectionMessage(d)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
