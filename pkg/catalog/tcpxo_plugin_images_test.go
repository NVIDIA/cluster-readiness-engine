// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

type tcpxoPluginImagesInput struct {
	Category           string                   `json:"category"`
	Subcategory        string                   `json:"subcategory"`
	Target             nvcrev1alpha1.TargetSpec `json:"target"`
	NodesPerJob        int32                    `json:"nodesPerJob"`
	GpusPerNode        int32                    `json:"gpusPerNode"`
	TCPXOPluginVersion string                   `json:"tcpxoPluginVersion"`
}

// TestTCPXOPluginImages verifies the images the GCP H100 override renders for
// a detected TCPXO plugin version (issues #438, #439): the workload image
// whose CUDA major matches the plugin, on every container that runs it and on
// trainer.image, and the tcpxo-daemon Google pairs with the plugin. An empty
// version renders the MinimumTCPXOPluginVersion profile, and an unmapped one
// the nearest safe mapped profile (TCPXOPluginProfileFor).
func TestTCPXOPluginImages(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "tcpxo-plugin-images",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input tcpxoPluginImagesInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		entry := Lookup(input.Category, input.Subcategory)
		if entry == nil {
			return fmt.Errorf("category %s/%s not registered", input.Category, input.Subcategory)
		}
		spec, err := entry.Build(input.Target, BuildConfig{
			NodesPerJob:        input.NodesPerJob,
			GpusPerNode:        input.GpusPerNode,
			GPUArchitecture:    GPUArchFromNodeSelector(input.Target.NodeSelector),
			TCPXOPluginVersion: input.TCPXOPluginVersion,
		})
		if err != nil {
			return err
		}
		images, err := gcpH100OverrideImages(spec)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(images, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// gcpH100OverrideImages returns every image the GCP H100 override sets, as
// "container=image" pairs ("trainer=image" for trainer.image), sorted. The
// override is still unapplied at Build time, so only its own fragments are
// read: base images that the override leaves alone do not appear.
func gcpH100OverrideImages(spec nvcrev1alpha1.WorkflowSpec) ([]string, error) {
	var images []string
	found := false
	for _, o := range spec.Overrides {
		if o.When.Platform == nil || o.When.Platform.Equals != "gcp" ||
			o.When.GPUArchitecture == nil || o.When.GPUArchitecture.Equals != "h100" {
			continue
		}
		found = true
		raw, err := json.Marshal(o)
		if err != nil {
			return nil, err
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		var walk func(key string, v any)
		walk = func(key string, v any) {
			switch n := v.(type) {
			case map[string]any:
				if img, ok := n["image"].(string); ok {
					name, _ := n["name"].(string)
					if key == "trainer" {
						name = "trainer"
					}
					images = append(images, name+"="+img)
					if name == "tcpxo-daemon" {
						images = append(images, "tcpxo-daemon-entrypoint="+daemonEntrypoint(n["args"]))
					}
				}
				for k, child := range n {
					walk(k, child)
				}
			case []any:
				for _, child := range n {
					walk(key, child)
				}
			}
		}
		walk("", doc)
	}
	if !found {
		return nil, fmt.Errorf("no GCP H100 override in the built spec")
	}
	slices.Sort(images)
	return images, nil
}

// daemonEntrypoint returns the entrypoint_rxdm_container.sh line from the
// tcpxo-daemon's shell script args, whose flags follow the daemon release.
func daemonEntrypoint(args any) string {
	list, _ := args.([]any)
	for _, a := range list {
		script, _ := a.(string)
		for line := range strings.SplitSeq(script, "\n") {
			if strings.Contains(line, "entrypoint_rxdm_container.sh --") {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// TestTCPXOPluginProfileFor pins which mapped release each detected tag
// renders: its own when mapped, with or without a build suffix, the latest
// when newer, the minimum otherwise.
func TestTCPXOPluginProfileFor(t *testing.T) {
	const latest = "v1.0.17"
	for _, tt := range []struct {
		version string
		release string
		exact   bool
	}{
		{MinimumTCPXOPluginVersion, MinimumTCPXOPluginVersion, true},
		{latest, latest, true},
		{"v1.0.15-1", MinimumTCPXOPluginVersion, true},
		{"v1.0.16-2", "v1.0.16", true},
		{"v1.0.17-1", latest, true},
		{"v1.0.18-1", latest, false},
		{"v1.0.18", latest, false},
		{"v1.1.0", latest, false},
		{"v2.0.0", latest, false},
		{"v1.0.14", MinimumTCPXOPluginVersion, false},
		{"v1.0.9-1", MinimumTCPXOPluginVersion, false},
		{"", MinimumTCPXOPluginVersion, false},
		{"latest", MinimumTCPXOPluginVersion, false},
		{"v1.0.18rc1", MinimumTCPXOPluginVersion, false},
	} {
		_, release, exact := TCPXOPluginProfileFor(tt.version)
		if release != tt.release || exact != tt.exact {
			t.Errorf("TCPXOPluginProfileFor(%q) = (%q, %v), want (%q, %v)",
				tt.version, release, exact, tt.release, tt.exact)
		}
	}
}

// TestTCPXOPluginProfileForUnmappedRelease pins a release inside the mapped
// range that has no row of its own: it renders the newest mapped release
// before it, not the minimum, so a gap in the table cannot drop a CUDA 13
// plugin to the CUDA 12 images.
func TestTCPXOPluginProfileForUnmappedRelease(t *testing.T) {
	const minimum, middle = MinimumTCPXOPluginVersion, "v1.0.17"
	saved := tcpxoPluginProfiles
	t.Cleanup(func() { tcpxoPluginProfiles = saved })
	tcpxoPluginProfiles = map[string]TCPXOPluginProfile{
		minimum:   saved[minimum],
		middle:    saved[middle],
		"v1.0.19": saved[middle],
	}
	for _, tt := range []struct {
		version string
		release string
	}{
		{"v1.0.16", minimum},
		{"v1.0.16-1", minimum},
		{"v1.0.18", middle},
		{"v1.0.18-1", middle},
	} {
		if _, release, exact := TCPXOPluginProfileFor(tt.version); release != tt.release || exact {
			t.Errorf("TCPXOPluginProfileFor(%q) = (%q, %v), want (%q, false)", tt.version, release, exact, tt.release)
		}
	}
}
