// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const pruneWorkflow = workflowDir + "/prune-images.yml"

// TestPruneNamesNoPackageDirectly is the regression guard for a mistake that
// was made while adding the second image, and that deletes data when made.
//
// prune-images.yml names its target package in four places and three
// encodings: the registry path (env.IMAGE), the packages API path with the
// slash percent-encoded (env.VERSIONS_API), the registry token scope with it
// un-encoded, and a bare /v2/<path>/referrers/ URL. Converting the job to a
// matrix over packages caught the first three and left the referrers URL
// pinned to "manager".
//
// That is not a cosmetic miss. The referrers lookup is how signature and
// attestation manifests enter the keep set, and the delete step removes every
// untagged version OUTSIDE that set. On the second package's matrix leg the
// lookup would have asked the first package for referrers of a digest it has
// never heard of, contributed nothing, and the prune would then have deleted
// the second image's own attestations as unreferenced. A workflow whose
// purpose is deleting things cannot have a copy of the package name that one
// leg of a matrix does not reach.
//
// So: no literal package path anywhere in the file. Every occurrence must come
// from the matrix.
func TestPruneNamesNoPackageDirectly(t *testing.T) {
	raw, err := os.ReadFile(pruneWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", pruneWorkflow, err)
	}

	var doc struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix map[string][]string `json:"matrix"`
			} `json:"strategy"`
			Env map[string]string `json:"env"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", pruneWorkflow, err)
	}

	prune, ok := doc.Jobs["prune"]
	if !ok {
		t.Fatalf("%s has no 'prune' job; if it was renamed, update this test "+
			"rather than deleting it", pruneWorkflow)
	}

	packages := prune.Strategy.Matrix["package"]
	if len(packages) < 2 {
		t.Fatalf("the prune job runs over %d package(s); this guard exists because a "+
			"single-package job grew a second one, so it should not pass with one",
			len(packages))
	}

	// Every package the matrix names must be reachable only through
	// ${{ matrix.package }}, never spelled out.
	for _, pkg := range packages {
		if strings.Contains(string(raw), "cluster-readiness-engine/"+pkg) ||
			strings.Contains(string(raw), "cluster-readiness-engine%2F"+pkg) {
			t.Errorf("%s names the package %q literally. Every package path must come "+
				"from ${{ matrix.package }}, or one matrix leg reads another package's "+
				"data: the referrers lookup feeds the keep set and the delete step "+
				"removes everything outside it.", pruneWorkflow, pkg)
		}
	}

	// And the three encodings must all be present and matrix-derived, so a
	// future edit cannot quietly drop one back to a literal.
	for _, key := range []string{"IMAGE", "VERSIONS_API", "TOKEN_SCOPE"} {
		v, found := prune.Env[key]
		if !found {
			t.Errorf("the prune job declares no env.%s; the package path has three "+
				"encodings and each needs its own matrix-derived variable", key)
			continue
		}
		if !strings.Contains(v, "matrix.package") {
			t.Errorf("env.%s is %q, which does not come from the matrix", key, v)
		}
	}

	// The bare /v2/ referrers URL is the one that was missed, so it is named
	// explicitly rather than left to the literal scan above.
	referrers := regexp.MustCompile(`ghcr\.io/v2/([^"\s]*)/referrers/`)
	for _, m := range referrers.FindAllStringSubmatch(string(raw), -1) {
		if !strings.Contains(m[1], "TOKEN_SCOPE") && !strings.Contains(m[1], "matrix.package") {
			t.Errorf("the referrers URL path is %q, which is not matrix-derived. This is "+
				"the exact site that was missed: it feeds the keep set, and a keep set "+
				"built from the wrong package deletes this one's attestations.", m[1])
		}
	}
}
