// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The pin guard is shell embedded in YAML, so nothing type-checks it and a
// weakened guard would not break a build: it would just stop rejecting things.
// So the step is extracted from release.yml and executed here, the same way
// attest.yml's input validation is, and the test cannot drift from the guard
// it covers.
//
// What it protects is a step with no other check behind it. RELEASE.md says to
// bump the pinned versions before tagging and nothing in CI rewrote or read
// them back, so forgetting shipped artifacts naming a release other than the
// one being cut, and the first symptom was on a user's cluster.
const (
	verifyPinsJob  = "verify-pins"
	verifyPinsStep = "Check pinned versions"

	kyvernoPin          = "config/samples/policy/kyverno-verify-images.yaml"
	policyControllerPin = "config/samples/policy/policy-controller-verify-images.yaml"
	catalogEntriesDir   = "pkg/catalog/entries/compute"

	// stateAbsent marks a fixture file that should not be written at all, as
	// opposed to one written empty. Shared with the installer guard's cases,
	// which use the same word for the same idea.
	stateAbsent = "absent"
	// jobBuildCLI is one of the release jobs that starts work from the tag.
	jobBuildCLI = "build-cli"
	// prevRelease stands in for "the release before the one being cut", which
	// is the value a forgotten bump leaves behind.
	prevRelease = "v0.11.0"
)

// pinTree lays out the files the guard reads, under dir.
type pinTree struct {
	// kyverno and policyController are the versions each sample pins. Empty
	// writes a sample with no version in it; stateAbsent omits the file.
	kyverno          string
	policyController string
	// catalogImage is the image line written into a catalog entry.
	catalogImage string
}

func (p pinTree) write(t *testing.T, dir string) {
	t.Helper()

	sample := func(rel, version string) {
		if version == stateAbsent {
			return
		}
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		body := "spec:\n  subject: ghcr.io/nvidia/cluster-readiness-engine/manager\n"
		if version != "" {
			body += "  tag: " + version + "\n"
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	sample(kyvernoPin, p.kyverno)
	sample(policyControllerPin, p.policyController)

	entry := filepath.Join(dir, catalogEntriesDir, "cutlass-gemm.yaml")
	if err := os.MkdirAll(filepath.Dir(entry), 0o750); err != nil {
		t.Fatalf("mkdir for catalog entry: %v", err)
	}
	body := "          image: " + p.catalogImage + "\n"
	if err := os.WriteFile(entry, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalog entry: %v", err)
	}
}

const (
	derivedImage = "ghcr.io/nvidia/cluster-readiness-engine/cutlass-gemm:{{ .HarnessImageTag }}"
	literalImage = "ghcr.io/nvidia/cluster-readiness-engine/cutlass-gemm:" + prevRelease
)

func TestVerifyPinsAcceptsAndRejects(t *testing.T) {
	script := gateStep(t, verifyPinsJob, verifyPinsStep)

	const tag = "v0.12.0"

	cases := map[string]struct {
		tree pinTree
		// wantErr is a substring of the refusal. Empty means the guard must
		// accept.
		wantErr string
	}{
		// The state a correctly prepared release is in.
		"every pin matches the tag": {
			pinTree{kyverno: tag, policyController: tag, catalogImage: derivedImage},
			"",
		},
		// The exact mistake the guard exists for: the bump was forgotten, so
		// the samples tell a user to verify the previous release's signature
		// against what they just installed.
		"a sample still pins the previous release": {
			pinTree{kyverno: prevRelease, policyController: tag, catalogImage: derivedImage},
			kyvernoPin + " pins '" + prevRelease + "' but this release is " + tag,
		},
		// Both stale is the likelier shape of that mistake, since the two are
		// bumped together, and must name both rather than stopping at the
		// first.
		"both samples pin the previous release": {
			pinTree{kyverno: prevRelease, policyController: prevRelease, catalogImage: derivedImage},
			policyControllerPin + " pins '" + prevRelease + "'",
		},
		// A sample with the pin deleted rather than stale. Treated as a
		// failure, not as "nothing to compare": an unpinned sample verifies
		// against whatever is newest, which is what pinning prevents.
		"a sample pins no version at all": {
			pinTree{kyverno: "", policyController: tag, catalogImage: derivedImage},
			kyvernoPin + " pins no version",
		},
		// If a file is renamed, the guard must fail rather than silently
		// checking nothing. A guard that passes because its subject vanished
		// is worse than no guard.
		"a sample file is missing": {
			pinTree{kyverno: stateAbsent, policyController: tag, catalogImage: derivedImage},
			kyvernoPin + " is missing",
		},
		// The catalog tag is derived from the build version precisely so no
		// one has to bump it. A literal put back is a regression to a pin
		// nothing keeps in step with the release.
		"a catalog entry reintroduces a literal version": {
			pinTree{kyverno: tag, policyController: tag, catalogImage: literalImage},
			"pins a literal image version",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tc.tree.write(t, dir)

			out, failed := runShell(t, dir, script, "TAG="+tag)

			if tc.wantErr == "" {
				if failed {
					t.Fatalf("guard refused a correctly pinned release; output:\n%s", out)
				}
				if !strings.Contains(out, "every pinned version matches "+tag) {
					t.Errorf("guard accepted without confirming the tag it checked; output:\n%s", out)
				}
				return
			}

			if !failed {
				t.Fatalf("guard accepted a release it must refuse (%s); output:\n%s", name, out)
			}
			if !strings.Contains(out, tc.wantErr) {
				t.Errorf("guard refused for the wrong reason.\nwant substring: %s\ngot:\n%s",
					tc.wantErr, out)
			}
		})
	}
}

// TestVerifyPinsGatesEveryPublisher is the other half. A guard that runs but
// that nothing waits on would let the builds publish alongside it and finish
// first, so the release would still ship with a stale pin while a red check
// sat beside it.
func TestVerifyPinsGatesEveryPublisher(t *testing.T) {
	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflow, err)
	}

	var doc struct {
		Jobs map[string]struct {
			Needs []string `json:"needs"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflow, err)
	}

	if _, ok := doc.Jobs[verifyPinsJob]; !ok {
		t.Fatalf("%s has no %q job; it is the only thing that reads a pinned "+
			"version back, so removing it restores a silent failure",
			releaseWorkflow, verifyPinsJob)
	}

	// Every job that starts work straight from the tag has to wait for the
	// guard. Anything downstream of these inherits the gate transitively.
	for _, job := range []string{"build-image", "gemm-build-image", jobBuildCLI} {
		spec, ok := doc.Jobs[job]
		if !ok {
			t.Errorf("%s has no %q job; if it was renamed, update this list rather "+
				"than dropping it", releaseWorkflow, job)
			continue
		}
		if !slices.Contains(spec.Needs, verifyPinsJob) {
			t.Errorf("job %q does not need %q (needs: %v), so it can publish before "+
				"the pins are checked", job, verifyPinsJob, spec.Needs)
		}
	}
}
