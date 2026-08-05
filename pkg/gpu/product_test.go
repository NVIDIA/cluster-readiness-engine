// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import "testing"

func TestParseProduct(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"NVIDIA-H100-80GB-HBM3", "h100"},
		{"NVIDIA-H100-NVL", "h100"},
		{"NVIDIA-GB200-NVL72", "gb200"},
		{"NVIDIA-GB200", "gb200"},
		{"NVIDIA-GB300-NVL72", "gb300"},
		{"NVIDIA-A100-SXM4-40GB", "a100"},
		{"NVIDIA-L40S", "l40s"},
		{"NVIDIA-L40", "l40"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ParseProduct(tt.input); got != tt.want {
				t.Errorf("ParseProduct(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
