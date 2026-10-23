// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// categoryNetworkAttachments records, per category, what the controller does
// with networkAttachments once spec-level and per-category options merge: the
// Workflow name, the fabric wiring that survives ResolveOptions, and the
// error CheckResolvedNetworkAttachments raises for an exclusion that no single
// options block shows.
type categoryNetworkAttachments struct {
	Category           string   `json:"category"`
	Workflow           string   `json:"workflow"`
	NetworkAttachments []string `json:"networkAttachments,omitempty"`
	NicResourceName    string   `json:"nicResourceName,omitempty"`
	RDMADeviceAccess   string   `json:"rdmaDeviceAccess,omitempty"`
	Error              string   `json:"error,omitempty"`
}

// TestCategoryNetworkAttachments covers issue #400 on the controller path,
// from the Certification spec alone: exclusions that only show up after the
// merge (a spec-level rdmaDeviceAccess: hostPath with a per-category list, and
// the reverse), the list and nicResourceName replacing each other across
// levels, and Workflow names that stay distinct when two categories reach the
// fabric differently.
func TestCategoryNetworkAttachments(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "category-network-attachments",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cert nvcrev1alpha1.Certification
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cert); err != nil {
			return err
		}
		r := &CertificationReconciler{}
		out := make([]categoryNetworkAttachments, 0, len(cert.Spec.Categories))
		for _, category := range cert.Spec.Categories {
			opts := ResolveOptions(&cert.Spec.CategoryOptions, category.Options)
			row := categoryNetworkAttachments{
				Category:         category.Domain + "/" + category.Variant,
				Workflow:         r.getWorkflowName(&cert, category),
				RDMADeviceAccess: opts.RDMADeviceAccess,
			}
			for _, a := range opts.NetworkAttachments {
				row.NetworkAttachments = append(row.NetworkAttachments, a.Network+"="+a.ResourceName)
			}
			if opts.NicResourceName != nil {
				row.NicResourceName = *opts.NicResourceName
			}
			if err := CheckResolvedNetworkAttachments(&opts); err != nil {
				row.Error = err.Error()
			}
			out = append(out, row)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
