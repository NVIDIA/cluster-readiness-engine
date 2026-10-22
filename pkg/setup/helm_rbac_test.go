// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRBACScope(t *testing.T) {
	for scope, ok := range map[string]bool{
		"":              true,
		"cluster":       true,
		"namespaced":    true,
		"per-namespace": false,
		"Cluster":       false,
	} {
		err := validateRBACScope(scope)
		require.Equal(t, ok, err == nil, "scope %q: %v", scope, err)
	}
}
