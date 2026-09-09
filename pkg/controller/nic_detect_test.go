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

// The on-prem GB200/GB300 override injects an RDMA NIC resource request only
// when a name is known, and the name depends on the device plugin a site runs
// (ADR-075). Detection fills the gap from node allocatable but must never
// guess. These cases pin the full rule set: the field always wins, the gate
// (on-prem + gb200/gb300) keeps other platforms untouched, the candidate set
// is exactly rdma/* plus nvidia.com/mlnxnics, a candidate must be allocatable
// (> 0) on every node, and anything but exactly one qualifying candidate
// resolves to "no injection" with the message the controllers emit as a
// NICResourceDetection event.
func TestResolveNICResourceName(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-nic-resource",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Field models spec.nicResourceName; non-nil must bypass
			// detection entirely.
			Field           *string `yaml:"field"`
			Platform        string  `yaml:"platform"`
			GPUArchitecture string  `yaml:"gpuArchitecture"`
			Nodes           []struct {
				Name        string            `yaml:"name"`
				Allocatable map[string]string `yaml:"allocatable"`
			} `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{}
			node.Name = n.Name
			node.Status.Allocatable = corev1.ResourceList{}
			for res, qty := range n.Allocatable {
				parsed, err := resource.ParseQuantity(qty)
				if err != nil {
					return err
				}
				node.Status.Allocatable[corev1.ResourceName(res)] = parsed
			}
			nodes = append(nodes, node)
		}

		name, candidates, ran := resolveNICResourceName(
			input.Field, input.Platform, input.GPUArchitecture, nodes)
		if candidates == nil {
			candidates = []string{}
		}

		out := struct {
			Name         string   `json:"name"`
			DetectionRan bool     `json:"detectionRan"`
			Candidates   []string `json:"candidates"`
			// Message is set exactly when the callers emit the
			// NICResourceDetection event / dry-run note: detection ran and
			// refused to pick.
			Message string `json:"message,omitempty"`
		}{Name: name, DetectionRan: ran, Candidates: candidates}
		if ran && name == "" {
			out.Message = nicDetectionMessage(candidates)
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
