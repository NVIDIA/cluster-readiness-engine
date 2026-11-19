// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkflowsHaveNoDuplicateKeys is the regression guard for a mistake that
// was made in this repository and that cannot be seen from a green build.
//
// prune-images.yml ended up with `permissions: {}` declared twice while it was
// being converted to a matrix over packages. GitHub Actions refuses to parse a
// workflow with a duplicate mapping key, so the workflow does not run at all:
// the run appears as a failure with no jobs and no logs, which reads like
// infrastructure flaking rather than a syntax error in the file. A scheduled
// workflow would simply have stopped firing.
//
// Nothing else in the tree catches it. Every other test in this package parses
// workflows with sigs.k8s.io/yaml, which converts YAML to JSON and silently
// keeps the LAST of a set of duplicate keys, so a duplicated job or a
// duplicated `on:` block parses cleanly here while GitHub rejects the file.
//
// The check is written against indentation rather than a YAML parser on
// purpose: adding a parser that reports duplicates would mean a new dependency
// for one assertion, and the shapes that matter are the ones a human edit
// produces, which is a repeated key at the top level or a repeated job name.
func TestWorkflowsHaveNoDuplicateKeys(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob %s: %v", workflowDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows found under %s; this guard must not pass by "+
			"finding nothing to check", workflowDir)
	}

	// A key at the given indentation: "name:", "on:", "jobs:" at column 0, and
	// job names at two spaces. Quoted and bracketed forms are not used for
	// these in this repository.
	key := regexp.MustCompile(`^(\s*)([A-Za-z_][A-Za-z0-9_.-]*):`)

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}

			// Track the keys seen at each indentation, resetting a level
			// whenever a shallower key closes the block it belonged to.
			seen := map[int]map[string]int{}
			inBlockScalar, blockIndent := false, 0

			for i, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)

				// Skip the body of a `run: |` block and friends, where the
				// content is shell and a "word:" line is not a YAML key.
				if inBlockScalar {
					if trimmed == "" {
						continue
					}
					if indentOf(line) > blockIndent {
						continue
					}
					inBlockScalar = false
				}
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}

				m := key.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				indent, name := len(m[1]), m[2]

				// Only the top level and job names. Deeper levels repeat keys
				// legitimately across sibling list items, and tracking those
				// needs the real parser this test is written to avoid.
				if indent > 2 {
					if strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, ">") ||
						strings.HasSuffix(trimmed, "|-") || strings.HasSuffix(trimmed, ">-") {
						inBlockScalar, blockIndent = true, indent
					}
					continue
				}
				// A list item resets the sibling scope, so `- name:` repeated
				// across steps is fine.
				if strings.HasPrefix(trimmed, "- ") {
					continue
				}
				if indent == 0 {
					// A new top-level key closes every nested scope.
					delete(seen, 2)
				}
				if seen[indent] == nil {
					seen[indent] = map[string]int{}
				}
				if prev, dup := seen[indent][name]; dup {
					t.Errorf("%s declares %q twice, at lines %d and %d. GitHub Actions "+
						"refuses to parse a workflow with a duplicate key, so this file "+
						"does not run at all: the run shows up as a failure with no jobs "+
						"and no logs.", filepath.Base(f), name, prev, i+1)
				}
				seen[indent][name] = i + 1

				if strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, ">") ||
					strings.HasSuffix(trimmed, "|-") || strings.HasSuffix(trimmed, ">-") {
					inBlockScalar, blockIndent = true, indent
				}
			}
		})
	}
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
