// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestResolveNodesPerJobPlacement pins the opt-out contract at the one place it
// can be broken: the size resolver.
//
// Under Pinned, nodesPerJob is a chunk size for a sweep that covers the whole
// target, so the controller is free to clamp it to what is available and snap it
// to what the model constraints allow. Under Unpinned it is the job the operator
// asked for, and every one of those adjustments becomes a silent substitution of
// a different job for the one requested. So each adjustment has a rejecting twin
// here, and the Pinned cases sit beside them to prove the default path still
// clamps and still snaps.
//
// The auto-select case is the one that matters most. With nodesPerJob unset the
// Pinned path takes every matching node, which is exactly the span-the-cluster
// behavior Unpinned exists to escape; it would survive the placement flag
// untouched if this resolver did not reject it.
func TestResolveNodesPerJobPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-nodes-per-job",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// AvailableNodes is the count that survived the target, architecture
			// and capacity filters, which is what the resolver actually sees.
			AvailableNodes int    `yaml:"availableNodes"`
			NodesPerJob    *int32 `yaml:"nodesPerJob"`
			Placement      string `yaml:"placement"`
			// Constraint emulates an entry's MaxValidNodes, matching the vocabulary
			// already used by resolve-nodes-per-job-after-arch-filter:
			//   "none" leaves it nil (any count valid)
			//   "even" accepts only even counts, as TP×PP divisibility does
			//   "min4" accepts only counts >= 4, as minGPUs does
			Constraint string `yaml:"constraint"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, input.AvailableNodes)
		for i := range input.AvailableNodes {
			nodes = append(nodes, corev1.Node{Name: nodeName(i)})
		}

		var entry *catalog.Entry
		switch input.Constraint {
		case "even":
			entry = &catalog.Entry{MaxValidNodes: func(available, _ int32, _ string) int32 {
				return available - available%2
			}}
		case "min4":
			entry = &catalog.Entry{MaxValidNodes: func(available, _ int32, _ string) int32 {
				if available < 4 {
					return 0
				}
				return available
			}}
		}

		opts := nvcrev1alpha1.CategoryOptions{
			NodesPerJob: input.NodesPerJob,
			Placement:   input.Placement,
		}
		cat := nvcrev1alpha1.CertificateCategory{Domain: testDomainCommunication, Variant: testVariantNCCLAllReduce}

		out := struct {
			NodesPerJob int32  `json:"nodesPerJob"`
			Error       string `json:"error,omitempty"`
		}{}

		npj, err := resolveNodesPerJob(nodes, cat, opts, entry, 8, "h100")
		if err != nil {
			out.Error = err.Error()
		}
		out.NodesPerJob = npj

		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// nodeName gives the synthetic nodes stable, sorted names. The resolver only
// counts them, but a named node reads better in a failure message than an
// anonymous one.
func nodeName(i int) string {
	return "node-" + string(rune('a'+i%26))
}
