// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"regexp"
	"strings"
)

// productLabelDisallowed matches the characters GFD drops from a product name.
var productLabelDisallowed = regexp.MustCompile(`[^A-Za-z0-9-_. ]`)

// ProductLabelValue converts an NVML device name, e.g. "NVIDIA GB300", to the
// nvidia.com/gpu.product label value GFD writes for it, "NVIDIA-GB300". It
// mirrors sanitise in NVIDIA/k8s-device-plugin internal/lm/resource.go, which
// owns the format: drop characters outside [A-Za-z0-9-_. ], then join the
// whitespace-separated fields with hyphens. GFD's resourceLabeler.getProductName
// in the same file also appends "-SHARED" under time-slicing; that suffix is
// not modeled here.
func ProductLabelValue(name string) string {
	return strings.Join(strings.Fields(productLabelDisallowed.ReplaceAllString(name, "")), "-")
}

// ParseProduct extracts the GPU architecture from a GPU product name. It
// accepts both the NVIDIA GPU Feature Discovery label format, which is
// hyphen-separated (e.g. "NVIDIA-H100-80GB-HBM3"), and the ResourceSlice
// productName device attribute format used on DRA-only platforms, which is
// space-separated (e.g. "NVIDIA GB300"). It strips the "NVIDIA" prefix,
// takes the first segment before any hyphen or space, and lowercases it.
// RTX PRO 6000 keeps its full model name.
// Examples: "NVIDIA-H100-80GB-HBM3" → "h100", "NVIDIA GB300" → "gb300",
// and "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition" → "rtxpro6000".
// Returns "" if the input is empty.
func ParseProduct(product string) string {
	if product == "" {
		return ""
	}
	product = strings.TrimPrefix(product, "NVIDIA-")
	product = strings.TrimPrefix(product, "NVIDIA ")
	// RTX PRO product names contain separators within the model name. Preserve the
	// model before the generic first-segment fallback (which would return rtx).
	if strings.HasPrefix(strings.ToUpper(strings.ReplaceAll(product, " ", "-")), "RTX-PRO-6000") {
		return "rtxpro6000"
	}
	if idx := strings.IndexAny(product, "- "); idx > 0 {
		product = product[:idx]
	}
	return strings.ToLower(product)
}
