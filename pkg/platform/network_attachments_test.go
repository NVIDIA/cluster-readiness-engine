// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// applyNetworkAttachmentsInput is the shape of each case's input.yaml: a
// resolved networkAttachments list and the dependency list a resolved
// Workflow would carry.
type applyNetworkAttachmentsInput struct {
	NetworkAttachments []nvcrev1alpha1.NetworkAttachment `json:"networkAttachments,omitempty"`
	Dependencies       []nvcrev1alpha1.DependencySpec    `json:"dependencies"`
}

// networkAttachmentsDependency is the golden-file view of one dependency after
// the helper ran: rawUnchanged guards the pass-through paths byte for byte,
// and resource carries the whole object so an edit outside the annotation and
// the worker container's resources shows up too.
type networkAttachmentsDependency struct {
	Index        int    `json:"index"`
	Kind         string `json:"kind"`
	RawUnchanged bool   `json:"rawUnchanged"`
	Resource     any    `json:"resource"`
}

// TestApplyNetworkAttachmentsToDependencies drives the helper both
// Certification paths call from ApplyResolvedWorkflowTransforms and both
// WorkloadRun paths call on the runtime they build. The cases cover the
// annotation replacing a catalog value while sibling annotations survive,
// per-resource counts adding up, the list adding to a quantity the platform
// already rendered for the same resource (limits and requests, string or
// number, limit only) while the count is the whole request when nothing was
// rendered (OCI with a list, mlnxPerNode: 0), the MPI launcher left alone, the worker
// container chosen by name and else the first, and the paths that must leave
// a dependency alone byte for byte (an empty list, a non-TrainingRuntime).
func TestApplyNetworkAttachmentsToDependencies(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "apply-network-attachments-deps",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in applyNetworkAttachmentsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		before := make([][]byte, len(in.Dependencies))
		for i := range in.Dependencies {
			before[i] = bytes.Clone(in.Dependencies[i].Raw)
		}
		if err := ApplyNetworkAttachmentsToDependencies(in.Dependencies, in.NetworkAttachments); err != nil {
			return err
		}
		out := make([]networkAttachmentsDependency, 0, len(in.Dependencies))
		for i := range in.Dependencies {
			var obj map[string]any
			if err := json.Unmarshal(in.Dependencies[i].Raw, &obj); err != nil {
				return err
			}
			kind, _ := obj[keyKind].(string)
			out = append(out, networkAttachmentsDependency{
				Index:        i,
				Kind:         kind,
				RawUnchanged: bytes.Equal(before[i], in.Dependencies[i].Raw),
				Resource:     obj,
			})
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestAddResourceQuantity covers the quantity forms a rendered limits or
// requests map can hold after JSON decoding, and the values it refuses rather
// than guess at.
func TestAddResourceQuantity(t *testing.T) {
	cases := []struct {
		name     string
		existing any
		present  bool
		qty      int64
		want     string
		wantErr  bool
	}{
		{name: "absent", present: false, qty: 2, want: "2"},
		{name: "null", existing: nil, present: true, qty: 1, want: "1"},
		{name: "string", existing: "8", present: true, qty: 1, want: "9"},
		{name: "number", existing: float64(8), present: true, qty: 2, want: "10"},
		{name: "json number", existing: json.Number("4"), present: true, qty: 1, want: "5"},
		{name: "fraction", existing: float64(1.5), present: true, qty: 1, wantErr: true},
		{name: "unparseable", existing: "eight", present: true, qty: 1, wantErr: true},
		{name: "wrong type", existing: true, present: true, qty: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := addResourceQuantity(tc.existing, tc.present, tc.qty)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("addResourceQuantity(%v) = %q, want an error", tc.existing, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("addResourceQuantity(%v): %v", tc.existing, err)
			}
			if got != tc.want {
				t.Fatalf("addResourceQuantity(%v) = %q, want %q", tc.existing, got, tc.want)
			}
		})
	}
}

// TestApplyNetworkAttachmentsBadQuantityLeavesDependency checks that a rendered
// quantity the helper cannot add to fails the call and leaves the dependency's
// bytes as they were.
func TestApplyNetworkAttachmentsBadQuantityLeavesDependency(t *testing.T) {
	raw := []byte(`{"apiVersion":"trainer.kubeflow.org/v1alpha1","kind":"TrainingRuntime",` +
		`"spec":{"template":{"spec":{"replicatedJobs":[{"name":"node","template":{"spec":{"template":` +
		`{"spec":{"containers":[{"name":"node","resources":{"limits":{"nvidia.com/mlnxnics":"eight"}}}]}}}}}]}}}}`)
	deps := []nvcrev1alpha1.DependencySpec{{}}
	deps[0].Raw = bytes.Clone(raw)
	atts := []nvcrev1alpha1.NetworkAttachment{{Network: "nvcre/ib0", ResourceName: "nvidia.com/mlnxnics"}}
	if err := ApplyNetworkAttachmentsToDependencies(deps, atts); err == nil {
		t.Fatal("ApplyNetworkAttachmentsToDependencies succeeded, want an error for an unparseable quantity")
	}
	if !bytes.Equal(deps[0].Raw, raw) {
		t.Fatalf("dependency changed on error:\n got %s\nwant %s", deps[0].Raw, raw)
	}
}
