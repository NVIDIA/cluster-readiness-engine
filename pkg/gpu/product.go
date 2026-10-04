// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import "strings"

// ParseProduct extracts the GPU architecture from an nvidia.com/gpu.product label value.
// It strips the "NVIDIA-" prefix and lowercases the product name. RTX PRO 6000
// keeps its full model name; other products use the first hyphen-separated segment.
// Examples: "NVIDIA-H100-80GB-HBM3" → "h100", "NVIDIA-GB200-NVL72" → "gb200",
// and "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition" → "rtxpro6000".
// Returns "" if the input is empty.
func ParseProduct(product string) string {
	if product == "" {
		return ""
	}
	product = strings.TrimPrefix(product, "NVIDIA-")
	// RTX PRO product labels contain hyphens within the model name. Preserve the
	// model before the generic first-segment fallback (which would return rtx).
	if strings.HasPrefix(strings.ToUpper(product), "RTX-PRO-6000") {
		return "rtxpro6000"
	}
	if idx := strings.Index(product, "-"); idx > 0 {
		product = product[:idx]
	}
	return strings.ToLower(product)
}
