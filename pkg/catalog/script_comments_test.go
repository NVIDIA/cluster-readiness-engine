// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// shellAssignment matches an environment-style shell assignment anywhere in a
// line. A whitespace trim that eats the newline after a comment leaves the
// next line's assignment at the end of the comment, as in
// "# ... at render time.TOTAL_GPUS=8".
var shellAssignment = regexp.MustCompile(`[A-Z_][A-Z0-9_]*=`)

type scriptCommentsInput struct {
	GPUProducts      []string `json:"gpuProducts"`
	NodesPerJob      int32    `json:"nodesPerJob"`
	EnableMNNVL      bool     `json:"enableMNNVL"`
	EnableCheckpoint bool     `json:"enableCheckpoint"`
	MlnxPerNode      int32    `json:"mlnxPerNode"`
	NicResourceName  string   `json:"nicResourceName"`
	RDMADeviceAccess string   `json:"rdmaDeviceAccess"`
}

type scriptCommentsProjection struct {
	Entries    []scriptCommentsEntry `json:"entries"`
	Violations []string              `json:"violations"`
}

type scriptCommentsEntry struct {
	Entry string `json:"entry"`
	// Scripts are the paths, without array indices, of the rendered strings
	// that carry at least one # comment line.
	Scripts []string `json:"scripts"`
}

// TestCatalogScriptComments verifies that no rendered comment line of any
// registered entry carries a shell assignment. Go template trim markers
// ({{- and -}}) remove every newline next to them, so a block of
// output-less actions trimmed on both sides joins the comment above it to the
// line below it, and the shell then ignores that line as part of the comment.
// The nemotron5 train.sh templates shipped this way from the first commit:
// TOTAL_GPUS=<n> rendered at the end of the "Dynamic computation" comment and
// was never set, and the integration goldens recorded the joined line, so no
// test noticed. Every string of the built WorkflowSpec is checked, base and
// platform overrides alike, because entry YAML is a template too. The golden
// lists where comment-bearing scripts were found, so an entry that stops
// rendering one shows up as a diff. Coverage is total: an entry that fails to
// build at the case's product and node count is a violation, not a skip.
func TestCatalogScriptComments(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "script-comments",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in scriptCommentsInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		proj := projectScriptComments(tc.T, in)
		require.Empty(tc.T, proj.Violations,
			"comment line carries a shell assignment; a template whitespace trim likely joined it to the next line")
		out, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out)
		return nil
	})
}

func projectScriptComments(t testing.TB, in scriptCommentsInput) scriptCommentsProjection {
	proj := scriptCommentsProjection{Entries: []scriptCommentsEntry{}, Violations: []string{}}
	violations := map[string]bool{}
	for _, category := range List() {
		entry := Lookup(category.Domain, category.Variant)
		require.NotNil(t, entry, "catalog.List returned %s/%s but Lookup does not know it",
			category.Domain, category.Variant)
		for _, product := range in.GPUProducts {
			source := fmt.Sprintf("%s/%s@%s", category.Domain, category.Variant, product)
			target := nvcrev1alpha1.TargetSpec{
				NodeSelector: map[string]string{gpu.ProductLabel: product},
			}
			arch := GPUArchFromNodeSelector(target.NodeSelector)
			// GpusPerNode resolves the way the controller resolves it, so the
			// scripts render real GPU counts rather than zero.
			spec, err := entry.Build(target, BuildConfig{
				NodesPerJob:      in.NodesPerJob,
				GpusPerNode:      GPUDefaults(arch, "").GpusPerNode,
				GPUArchitecture:  arch,
				EnableMNNVL:      in.EnableMNNVL,
				EnableCheckpoint: in.EnableCheckpoint,
				MlnxPerNode:      in.MlnxPerNode,
				NicResourceName:  in.NicResourceName,
				RDMADeviceAccess: in.RDMADeviceAccess,
			})
			if err != nil {
				violations[source+": build failed: "+err.Error()] = true
				continue
			}
			raw, err := json.Marshal(spec)
			if err != nil {
				violations[source+": marshal failed: "+err.Error()] = true
				continue
			}
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				violations[source+": unmarshal failed: "+err.Error()] = true
				continue
			}

			scripts := map[string]bool{}
			walkRenderedStrings(doc, "", func(path, s string) {
				for line := range strings.SplitSeq(s, "\n") {
					comment := strings.TrimSpace(line)
					if !strings.HasPrefix(comment, "#") {
						continue
					}
					scripts[path] = true
					if shellAssignment.MatchString(comment) {
						violations[fmt.Sprintf("%s: %s: %q", source, path, comment)] = true
					}
				}
			})
			proj.Entries = append(proj.Entries, scriptCommentsEntry{
				Entry:   source,
				Scripts: slices.Sorted(maps.Keys(scripts)),
			})
		}
	}
	for i := range proj.Entries {
		if proj.Entries[i].Scripts == nil {
			proj.Entries[i].Scripts = []string{}
		}
	}
	sort.Slice(proj.Entries, func(i, j int) bool { return proj.Entries[i].Entry < proj.Entries[j].Entry })
	proj.Violations = append(proj.Violations, slices.Sorted(maps.Keys(violations))...)
	return proj
}

// walkRenderedStrings calls fn with every string under v and its path. The
// path drops array indices, so it names a kind of location (a ConfigMap key,
// a container's args) rather than a position that shifts when an override is
// added.
func walkRenderedStrings(v any, path string, fn func(path, s string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			walkRenderedStrings(child, path+"."+k, fn)
		}
	case []any:
		for _, child := range x {
			walkRenderedStrings(child, path+"[]", fn)
		}
	case string:
		fn(path, x)
	}
}
