// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// TestBuildRequiresOCISecondaryNetwork pins ADR-103 Decision 4: the catalog
// has no default network, so a caller that requests NICs without naming one
// gets an error instead of an attachment to an empty name.
func TestBuildRequiresOCISecondaryNetwork(t *testing.T) {
	entry := Lookup("communication", "nccl-all-reduce")
	if entry == nil {
		t.Fatal("communication/nccl-all-reduce is not registered")
	}
	target := nvcrev1alpha1.TargetSpec{}

	for _, tc := range []struct {
		name        string
		mlnxPerNode int32
		network     string
		wantErr     bool
	}{
		{name: "nics without a network", mlnxPerNode: 4, wantErr: true},
		{name: "nics with a network", mlnxPerNode: 4, network: DefaultOCISecondaryNetwork},
		{name: "no nics, no network", mlnxPerNode: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := entry.Build(target, BuildConfig{
				NodesPerJob:         2,
				GpusPerNode:         4,
				GPUArchitecture:     "gb300",
				MlnxPerNode:         tc.mlnxPerNode,
				OCISecondaryNetwork: tc.network,
			})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "OCISecondaryNetwork is required") {
					t.Fatalf("Build error = %v, want the OCISecondaryNetwork error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
		})
	}
}

// TestSpecReferencesNetwork checks the sentinel lookup the controller gates
// detection on, including a name that only an unapplied override carries.
func TestSpecReferencesNetwork(t *testing.T) {
	entry := Lookup("communication", "nccl-all-reduce")
	if entry == nil {
		t.Fatal("communication/nccl-all-reduce is not registered")
	}
	spec, err := entry.Build(nvcrev1alpha1.TargetSpec{}, BuildConfig{
		NodesPerJob:         2,
		GpusPerNode:         4,
		GPUArchitecture:     "gb300",
		MlnxPerNode:         4,
		OCISecondaryNetwork: OCISecondaryNetworkUnresolved,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Before overlays apply, the OCI block is still an override.
	if got, err := SpecReferencesNetwork(&spec, OCISecondaryNetworkUnresolved); err != nil || !got {
		t.Fatalf("SpecReferencesNetwork(sentinel) = %v, %v; want true, nil", got, err)
	}
	if got, err := SpecReferencesNetwork(&spec, "other/net"); err != nil || got {
		t.Fatalf("SpecReferencesNetwork(other/net) = %v, %v; want false, nil", got, err)
	}
	// Detection keys on OCIMlnxResourceName, and the fragment writes the
	// resource as a literal: the two must name the same resource. Only the
	// OCI override that attaches the network counts, since Build also renders
	// the Azure and TogetherAI blocks, which request nvidia.com/mlnxnics too.
	checked := 0
	for _, o := range spec.Overrides {
		if o.When.Platform == nil || o.When.Platform.Equals != "oci" {
			continue
		}
		raw, err := json.Marshal(o.Dependencies)
		if err != nil {
			t.Fatalf("marshaling OCI override dependencies: %v", err)
		}
		if !strings.Contains(string(raw), OCISecondaryNetworkUnresolved) {
			continue
		}
		checked++
		if !strings.Contains(string(raw), `"`+OCIMlnxResourceName+`":"4"`) {
			t.Errorf("the OCI override attaching %s does not request %s: %s",
				OCISecondaryNetworkUnresolved, OCIMlnxResourceName, raw)
		}
	}
	if checked == 0 {
		t.Fatal("no OCI override attaches the OCI secondary network")
	}
}
