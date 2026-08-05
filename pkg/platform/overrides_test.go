// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// TestEveryLibFragmentParsesThroughPlatform is the regression test for a
// divergence that used to exist between the two renderers of entries/_lib/.
//
// pkg/catalog registered indent, trimSuffix, repeat, int, mul, toYaml and
// toMpiArgs; pkg/platform registered only lib and indent. A fragment using any
// of the others parsed fine through the catalog and failed to parse through
// pkg/platform — and because BuildOverrides panics on render error, that
// surfaced as a process crash at startup rather than a handled error.
//
// Only deps/oci-mlnxnics-comm.yaml happened to use the missing functions, and
// no platform override referenced it, so the trap was live but unsprung. This
// asserts the property directly rather than relying on that coincidence: every
// fragment must parse with the function map pkg/platform renders through.
func TestEveryLibFragmentParsesThroughPlatform(t *testing.T) {
	entriesFS := catalog.EntriesFS()

	var fragments []string
	require.NoError(t, fs.WalkDir(entriesFS, "entries/_lib", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".yaml" {
			fragments = append(fragments, path)
		}
		return nil
	}))
	require.NotEmpty(t, fragments, "expected to find _lib fragments to check")

	for _, path := range fragments {
		t.Run(strings.TrimPrefix(path, "entries/_lib/"), func(t *testing.T) {
			content, err := entriesFS.ReadFile(path)
			require.NoError(t, err)

			_, err = template.New(filepath.Base(path)).
				Funcs(catalog.TemplateFuncsWithLib()).
				Parse(string(content))
			require.NoError(t, err,
				"fragment must parse with the function map pkg/platform renders through")
		})
	}
}

// TestBuildOverridesRendersForEveryPlatform exercises the real entry point.
// BuildOverrides panics rather than returning an error, so a template
// regression here takes down the controller at startup; this keeps that
// failure in CI instead.
func TestBuildOverridesRendersForEveryPlatform(t *testing.T) {
	tests := []struct {
		name string
		cfg  OverrideConfig
	}{
		{
			name: "mpi with MNNVL",
			cfg: OverrideConfig{
				EntryName: "test", NodesPerJob: 2, GpusPerNode: 4,
				MlnxPerNode: 2, EnableMNNVL: true, FrameworkType: "mpi",
			},
		},
		{
			name: "torch without MNNVL",
			cfg: OverrideConfig{
				EntryName: "test", NodesPerJob: 8, GpusPerNode: 8,
				MlnxPerNode: 8, EnableMNNVL: false, FrameworkType: "torch",
			},
		},
		{
			name: "exec framework, single node",
			cfg: OverrideConfig{
				EntryName: "test", NodesPerJob: 1, GpusPerNode: 1,
				FrameworkType: "exec",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				require.NotEmpty(t, BuildOverrides(tt.cfg),
					"expected at least one platform override")
			})
		})
	}
}
