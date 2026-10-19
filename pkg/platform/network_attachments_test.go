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
// per-resource counts adding up, the MPI launcher left alone, the worker
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
