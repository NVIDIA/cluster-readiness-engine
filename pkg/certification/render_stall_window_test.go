// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// ptr returns a pointer to v, for the *int32 option fields.
func ptr(v int32) *int32 { return &v }

// TestRenderStartupStallWindow covers the startup-stall window option end to
// end: the training catalog entries hardcode 1200 s in the Job template, so the
// option is worthless unless the RenderOptions merge and the catalog.BuildConfig
// plumbing both carry it into the generated Job. The three cases are the entry
// default, a spec-level override, and a per-category override that must win over
// the spec-level value (the documented precedence for CategoryOptions).
func TestRenderStartupStallWindow(t *testing.T) {
	trainingCategory := nvcrev1alpha1.CertificateCategory{Domain: "training", Variant: "nemotron5-8b"}
	cases := []struct {
		name   string
		global *int32
		cat    *int32
		want   int32
	}{
		{name: "entry-default-when-unset", want: 1200},
		{name: "spec-level-override", global: ptr(3600), want: 3600},
		{name: "per-category-wins", global: ptr(3600), cat: ptr(2400), want: 2400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cat := trainingCategory
			if tc.cat != nil {
				cat.Options = &nvcrev1alpha1.CategoryOptions{StartupStallTimeoutSeconds: tc.cat}
			}
			cert := &nvcrev1alpha1.Certification{
				Spec: nvcrev1alpha1.CertificationSpec{
					Target: nvcrev1alpha1.TargetSpec{
						NodeSelector: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
					},
					CategoryOptions: nvcrev1alpha1.CategoryOptions{StartupStallTimeoutSeconds: tc.global},
					Categories:      []nvcrev1alpha1.CertificateCategory{cat},
				},
			}
			workflows, err := renderCertification(cert, "aws")
			if err != nil {
				t.Fatalf("renderCertification: %v", err)
			}
			if len(workflows) != 1 {
				t.Fatalf("rendered %d workflows, want 1", len(workflows))
			}
			got := workflows[0].Spec.JobTemplate.Spec.StartupStallTimeoutSeconds
			if got == nil {
				t.Fatalf("startupStallTimeoutSeconds missing from the generated Job spec")
			}
			if *got != tc.want {
				t.Errorf("startupStallTimeoutSeconds = %d, want %d", *got, tc.want)
			}
			// The stall multiplier is the other half of the gate and must stay
			// the entry's value (3); this option never touches it.
			if sm := workflows[0].Spec.JobTemplate.Spec.StallMultiplier; sm == nil || *sm != 3 {
				t.Errorf("stallMultiplier = %v, want 3 (option must not change it)", sm)
			}
		})
	}
}
