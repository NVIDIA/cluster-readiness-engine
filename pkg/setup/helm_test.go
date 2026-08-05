// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelmChartVersion(t *testing.T) {
	assert.Equal(t, "1.20.0", helmChartVersion("v1.20.0"))
	assert.Equal(t, "1.20.0", helmChartVersion("1.20.0"))
}

func TestResolveHelmChartVersion(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		ver, err := resolveHelmChartVersion("dev", "v1.19.0")
		require.NoError(t, err)
		assert.Equal(t, "1.19.0", ver)
	})

	t.Run("release build", func(t *testing.T) {
		ver, err := resolveHelmChartVersion("1.20.0", "")
		require.NoError(t, err)
		assert.Equal(t, "1.20.0", ver)
	})

	t.Run("dev build requires override", func(t *testing.T) {
		_, err := resolveHelmChartVersion("1.20.0-4-gabcdef-dirty", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--version")
	})
}
