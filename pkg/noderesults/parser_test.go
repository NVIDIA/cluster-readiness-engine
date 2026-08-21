// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderesults

import (
	"testing"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

func TestFailedNodesToJSON(t *testing.T) {
	nodes := []crev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: crev1alpha1.NodeFailureWorkloadFailed, Message: "boom"},
	}
	b, err := FailedNodesToJSON(nodes)
	if err != nil {
		t.Fatalf("FailedNodesToJSON: %v", err)
	}
	got, err := FailedNodesFromJSON(b)
	if err != nil {
		t.Fatalf("FailedNodesFromJSON: %v", err)
	}
	if len(got) != 1 || got[0] != nodes[0] {
		t.Fatalf("round-trip got %+v, want %+v", got, nodes)
	}
}

func TestFailedNodesToJSONNil(t *testing.T) {
	b, err := FailedNodesToJSON(nil)
	if err != nil {
		t.Fatalf("FailedNodesToJSON(nil): %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("got %q, want []", string(b))
	}
}

func TestFailedNodesFromJSONEmpty(t *testing.T) {
	got, err := FailedNodesFromJSON(nil)
	if err != nil {
		t.Fatalf("FailedNodesFromJSON(nil): %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
