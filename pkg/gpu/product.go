// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import "strings"

// ParseProduct extracts the GPU architecture from an nvidia.com/gpu.product label value.
// It strips the "NVIDIA-" prefix, takes the first segment before any hyphen, and lowercases it.
// Examples: "NVIDIA-H100-80GB-HBM3" → "h100", "NVIDIA-GB200-NVL72" → "gb200".
// Returns "" if the input is empty.
func ParseProduct(product string) string {
	if product == "" {
		return ""
	}
	product = strings.TrimPrefix(product, "NVIDIA-")
	if idx := strings.Index(product, "-"); idx > 0 {
		product = product[:idx]
	}
	return strings.ToLower(product)
}
