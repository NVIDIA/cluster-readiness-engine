// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import "testing"

func TestValidateAkamaiFlag(t *testing.T) {
	if err := ValidateFlag(Akamai); err != nil {
		t.Fatalf("Akamai must be accepted by --platform: %v", err)
	}
}
