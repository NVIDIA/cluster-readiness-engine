// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import "testing"

// TestDefaultEnableMNNVL pins which architectures default to multi-node
// NVLink. HGX B200/B300 have NVSwitch inside the chassis only, so they must
// stay false even though B300 shares a name prefix with GB300 (ADR-092).
func TestDefaultEnableMNNVL(t *testing.T) {
	for arch, want := range map[string]bool{
		"gb200": true, "gb300": true,
		"b200": false, "b300": false, "": false,
	} {
		if got := DefaultEnableMNNVL(arch); got != want {
			t.Errorf("DefaultEnableMNNVL(%q) = %v, want %v", arch, got, want)
		}
	}
}
