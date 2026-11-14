// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestHarnessImageTag pins how a build version becomes the tag of a harness
// image this repository publishes for itself.
//
// The reason this is derived rather than pinned is that the pin had no check
// behind it: release.yml builds the binary and the harness image from the same
// tag, so forgetting to bump a literal shipped a catalog naming an image that
// was never built, and the first symptom was ImagePullBackOff on a cluster
// that had already installed the release. The cases below are the three shapes
// `git describe --tags --always --dirty` actually produces, plus the inputs
// that must NOT be mistaken for a version.
func TestHarnessImageTag(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "harness-image-tag",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// BuildVersion is what -ldflags set on the binary.
			BuildVersion string `json:"buildVersion"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		out := struct {
			Tag string `json:"tag"`
			// Released reports whether the tag names a release, which is the
			// only case where the image is guaranteed to exist.
			Released bool `json:"released"`
		}{Tag: HarnessImageTag(input.BuildVersion)}
		out.Released = releaseVersion.MatchString(out.Tag)

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
