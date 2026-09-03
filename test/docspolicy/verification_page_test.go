// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package docspolicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// verificationPage is the page that tells users how to check what we shipped.
// A wrong command here is worse than no page: it converts an unverified install
// into one the user believes was verified.
const verificationPage = "../../docs/operations/verifying-artifacts.md"

// bashFence matches a ```bash fenced block and captures its body.
var bashFence = regexp.MustCompile("(?s)```bash\\n(.*?)```")

func pageBlocks(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(verificationPage)
	if err != nil {
		t.Fatalf("read %s: %v", verificationPage, err)
	}
	matches := bashFence.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatalf("%s has no ```bash blocks; the page is meant to be copy-pasteable", verificationPage)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// TestVerificationPageBlocksAreValidShell parses every published command.
//
// These are copy-pasted by people verifying a supply chain, often onto a
// machine they are being careful about. A block that does not parse wastes
// their time; worse, a block that parses but was never run can fail halfway and
// leave them believing the part that did run proved something. The executable
// half of this lives in .github/workflows/docs-verify.yml, which runs the same
// blocks against a real release.
func TestVerificationPageBlocksAreValidShell(t *testing.T) {
	for i, block := range pageBlocks(t) {
		path := filepath.Join(t.TempDir(), "block.sh")
		if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
			t.Fatalf("write block: %v", err)
		}
		if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("block %d in %s is not valid shell: %v\n%s\n--- block ---\n%s",
				i+1, verificationPage, err, out, block)
		}
	}
}

// TestVerificationPagePinsAnExactIdentity is the specific mistake this page
// exists to stop people making.
//
// SECURITY.md previously published `--certificate-identity-regexp` with a
// pattern naming no workflow and no ref. This repository also publishes
// `main-<sha>` development images signed under the same repository, so that
// pattern accepts an unreleased branch build as a release. Anyone following the
// documentation exactly would have accepted one.
func TestVerificationPagePinsAnExactIdentity(t *testing.T) {
	blocks := pageBlocks(t)
	joined := strings.Join(blocks, "\n")

	if strings.Contains(joined, "--certificate-identity-regexp") {
		t.Error("the verification page publishes --certificate-identity-regexp; " +
			"an identity naming no workflow and no ref also accepts a branch build")
	}

	// The whole identity, repository included. Checking only the
	// `/.github/workflows/attest.yml@refs/tags/` suffix would accept a page
	// that pinned some other repository's workflow -- an identity under which
	// cosign would happily verify an artifact NVIDIA never built. The suffix is
	// the part that looks security-relevant; the origin is the part that is.
	wantIdentity := "https://github.com/NVIDIA/cluster-readiness-engine" +
		"/.github/workflows/attest.yml@refs/tags/"
	if !strings.Contains(joined, wantIdentity) {
		t.Errorf("no published command pins the identity %q; "+
			"it must name this repository, the signing workflow and the tag", wantIdentity)
	}

	if !strings.Contains(joined, "https://token.actions.githubusercontent.com") {
		t.Error("no published command pins --certificate-oidc-issuer; " +
			"without it any issuer that can mint a matching SAN is accepted")
	}
}

// TestVerificationPageIsInTheNav keeps the page reachable.
//
// A verification page nobody can find is the same as not having one, and the
// Fern site is built from docs/index.yml rather than by directory scan, so a
// new page is invisible until it is listed.
func TestVerificationPageIsInTheNav(t *testing.T) {
	raw, err := os.ReadFile("../../docs/index.yml")
	if err != nil {
		t.Fatalf("read docs/index.yml: %v", err)
	}

	// Parsed, not string-matched. A commented-out entry still contains the
	// path, so containment would report a page as navigable while the site
	// renders without it -- which is the exact state this test exists to catch.
	var nav any
	if err := yaml.Unmarshal(raw, &nav); err != nil {
		t.Fatalf("parse docs/index.yml: %v", err)
	}
	if !navHasPath(nav, "operations/verifying-artifacts.md") {
		t.Error("docs/index.yml has no navigation entry with " +
			"path: operations/verifying-artifacts.md, so the page does not appear " +
			"in the published navigation")
	}
}

// navHasPath reports whether the parsed navigation contains an entry whose
// `path` is want, at any depth.
func navHasPath(node any, want string) bool {
	switch v := node.(type) {
	case map[string]any:
		if p, ok := v["path"].(string); ok && p == want {
			return true
		}
		for _, child := range v {
			if navHasPath(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if navHasPath(child, want) {
				return true
			}
		}
	}
	return false
}
