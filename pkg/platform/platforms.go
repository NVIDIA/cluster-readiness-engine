// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"slices"
	"strings"
)

// Platform name constants. These are the values the controller's platform
// detection (pkg/controller/workflow_detect.go) can return and the only
// values the CLI --platform flags accept. Detection returns these constants
// and the CLIs derive validation, help text, and error messages from Names(),
// so the two can never drift apart.
const (
	AWS        = "aws"
	GCP        = "gcp"
	Azure      = "azure"
	OCI        = "oci"
	OnPrem     = "onprem"
	TogetherAI = "togetherai"
	Mistral    = "mistral"
	Forge      = "forge"
	NScale     = "nscale"
)

// Names returns every valid platform name, in the order used in flag help
// and error messages.
func Names() []string {
	return []string{AWS, GCP, Azure, OCI, OnPrem, TogetherAI, Mistral, Forge, NScale}
}

// NamesList returns the valid platform names joined with ", " for flag help
// and error messages.
func NamesList() string {
	return strings.Join(Names(), ", ")
}

// ValidateFlag validates a --platform flag value. An empty value is allowed
// (platform not specified). The error text derives the valid names from
// Names() so the message always matches the actual set.
func ValidateFlag(name string) error {
	if name == "" || slices.Contains(Names(), name) {
		return nil
	}
	return fmt.Errorf("invalid --platform %q: must be one of %s", name, NamesList())
}
