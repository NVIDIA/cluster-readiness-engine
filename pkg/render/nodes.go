// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package render

// SyntheticProviderID returns a providerID prefix that detectPlatform
// maps back to the given platform string. Used for offline rendering.
func SyntheticProviderID(platform string) string {
	switch platform {
	case "aws":
		return "aws://synthetic/render"
	case "gcp":
		return "gce://synthetic/render"
	case "azure":
		return "azure://synthetic/render"
	case "oci":
		return "ocid1.synthetic.render"
	case "togetherai":
		return "kubevirt://synthetic/render"
	case "mistral":
		return "metal3://synthetic/render"
	default:
		return ""
	}
}
