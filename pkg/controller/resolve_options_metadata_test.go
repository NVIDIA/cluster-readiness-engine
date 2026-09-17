// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// burnIn is the global label value both cases carry through ResolveOptions.
const burnIn nvcrev1alpha1.WorkloadLabelValue = "burn-in"

const envLabelKey = "environment"

// TestResolveOptionsWorkloadMetadataDoesNotAliasGlobal pins the one thing that
// makes the per-key workload-label merge safe. ResolveOptions starts with
// `resolved := *global`, a struct copy that leaves every map, slice and
// pointer aliased to the Certification's own spec. Workload labels are the
// only option merged rather than replaced, so without detaching them first the
// merge would write a category's labels into the Certification it read from —
// and since the controller resolves categories in a loop, the first category's
// labels would leak into every later one.
//
// This is an aliasing invariant with no structured output worth snapshotting:
// the golden-file cases in pkg/workload and pkg/certification cover what the
// merge produces, and this covers what it must not touch.
func TestResolveOptionsWorkloadMetadataDoesNotAliasGlobal(t *testing.T) {
	global := &nvcrev1alpha1.CategoryOptions{
		WorkloadMetadata: &nvcrev1alpha1.WorkloadMetadata{
			Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{
				envLabelKey: burnIn,
			},
		},
	}
	override := &nvcrev1alpha1.CategoryOptions{
		WorkloadMetadata: &nvcrev1alpha1.WorkloadMetadata{
			Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{
				"kueue.x-k8s.io/queue-name": "nccl-queue",
			},
		},
	}

	resolved := ResolveOptions(global, override)

	if got := len(resolved.WorkloadMetadata.Labels); got != 2 {
		t.Fatalf("resolved labels = %d entries, want 2 (global merged with per-category)",
			got)
	}
	if len(global.WorkloadMetadata.Labels) != 1 {
		t.Errorf("global label map gained the per-category key: %v",
			global.WorkloadMetadata.Labels)
	}
	if len(override.WorkloadMetadata.Labels) != 1 {
		t.Errorf("per-category label map gained the global key: %v",
			override.WorkloadMetadata.Labels)
	}

	// Writing through the result must not reach either source.
	resolved.WorkloadMetadata.Labels["added"] = "x"
	if _, present := global.WorkloadMetadata.Labels["added"]; present {
		t.Error("resolved label map aliases the global map")
	}
	if _, present := override.WorkloadMetadata.Labels["added"]; present {
		t.Error("resolved label map aliases the per-category map")
	}
}

// TestResolveOptionsWorkloadMetadataWithoutOverride covers the early return.
// ResolveOptions bails out before the per-field merges when a category has no
// options at all, so the detaching has to happen before that point — otherwise
// the common case would be the aliased one.
func TestResolveOptionsWorkloadMetadataWithoutOverride(t *testing.T) {
	global := &nvcrev1alpha1.CategoryOptions{
		WorkloadMetadata: &nvcrev1alpha1.WorkloadMetadata{
			Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{
				envLabelKey: burnIn,
			},
		},
	}

	resolved := ResolveOptions(global, nil)

	if got := resolved.WorkloadMetadata.Labels[envLabelKey]; got != burnIn {
		t.Fatalf("global label not carried through: environment = %q", got)
	}
	resolved.WorkloadMetadata.Labels["added"] = "x"
	if _, present := global.WorkloadMetadata.Labels["added"]; present {
		t.Error("resolved label map aliases the global map on the no-override path")
	}

	// A Certification with no workload labels anywhere must resolve to an
	// absent field, not an empty object, so its Workflows render as before.
	if md := ResolveOptions(&nvcrev1alpha1.CategoryOptions{}, nil).WorkloadMetadata; md != nil {
		t.Errorf("unconfigured workloadMetadata resolved to %+v, want nil", md)
	}
}
