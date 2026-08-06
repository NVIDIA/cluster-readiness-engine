// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelmChartVersion(t *testing.T) {
	assert.Equal(t, "v1.20.0", helmChartVersion("v1.20.0"))
	assert.Equal(t, "1.20.0", helmChartVersion("1.20.0"))
}

func TestResolveHelmChartVersion(t *testing.T) {
	t.Run("override preserves v prefix", func(t *testing.T) {
		ver, err := resolveHelmChartVersion("dev", "v1.19.0")
		require.NoError(t, err)
		assert.Equal(t, "v1.19.0", ver)
	})

	t.Run("release build preserves version as-is", func(t *testing.T) {
		ver, err := resolveHelmChartVersion("1.20.0", "")
		require.NoError(t, err)
		assert.Equal(t, "1.20.0", ver)
	})

	t.Run("dev build requires override", func(t *testing.T) {
		_, err := resolveHelmChartVersion("1.20.0-4-gabcdef-dirty", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--version")
	})

	t.Run("pre-release tag needs no override", func(t *testing.T) {
		for _, v := range []string{"v0.1.0-rc.7", "0.1.0-rc.7", "v1.2.3-beta.1"} {
			ver, err := resolveHelmChartVersion(v, "")
			require.NoError(t, err, v)
			assert.Equal(t, v, ver)
		}
	})

	t.Run("git describe output requires override", func(t *testing.T) {
		for _, v := range []string{"1.20.0-4-gabcdef1", "v1.20.0-12-g0123456-dirty", "dev", ""} {
			_, err := resolveHelmChartVersion(v, "")
			require.Error(t, err, v)
		}
	})
}
