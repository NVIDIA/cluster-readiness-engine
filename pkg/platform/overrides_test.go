// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
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
	p := &testutil.TestCaseParser{
		Subdir:         "build-overrides",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg OverrideConfig
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		overrides, err := buildOverridesRecovered(cfg)
		if err != nil {
			return err
		}
		if len(overrides) == 0 {
			return fmt.Errorf("expected at least one platform override")
		}

		b, err := json.MarshalIndent(struct {
			Config             OverrideConfig `json:"config"`
			AtLeastOneOverride bool           `json:"atLeastOneOverride"`
		}{cfg, len(overrides) > 0}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestBuildOverridesMPIArgs records every rendered override that carries
// mpiArgs, together with its when clause. mpiArgs never reach the Workflow CR
// — the WorkloadRun controller bakes them into trainer.args before the
// Workflow is created — so the Workflow-level goldens cannot guard the
// platform table's mpirun blocks by themselves. This records them at the
// source, including the AWS GB300 RoCE transport pins composed from the
// catalog's _lib fragments (ADR-070).
func TestBuildOverridesMPIArgs(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "override-mpi-args",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg OverrideConfig
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		overrides, err := buildOverridesRecovered(cfg)
		if err != nil {
			return err
		}

		type mpiOverride struct {
			When    nvcrev1alpha1.WhenSpec `json:"when"`
			MPIArgs []string               `json:"mpiArgs"`
		}
		out := []mpiOverride{}
		for _, o := range overrides {
			if len(o.MPIArgs) > 0 {
				out = append(out, mpiOverride{When: o.When, MPIArgs: o.MPIArgs})
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// buildOverridesRecovered wraps BuildOverrides, converting its panic-on-error
// behaviour into a returned error so a template regression fails the
// offending test case cleanly instead of crashing the whole test binary —
// the same effect require.NotPanics had in the table-driven version.
func buildOverridesRecovered(cfg OverrideConfig) (overrides []WorkloadRunOverride, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("BuildOverrides panicked: %v", r)
		}
	}()
	overrides = BuildOverrides(cfg)
	return overrides, nil
}
