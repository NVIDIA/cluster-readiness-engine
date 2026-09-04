// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The weekly image scan decides what gets scanned in one shell step, and every
// completeness guarantee downstream is anchored to what that step emits. The
// report job checks that every resolved target produced a report -- so if this
// step is allowed to quietly resolve fewer targets than it set out to, the
// check downstream validates the survivors and the run reports clean.
//
// That is not hypothetical: the first version of this step warned and
// `continue`d when an image did not resolve, which dropped the release target
// -- the image operators actually run -- from a green weekly run that sent no
// notification, because the Slack step only fires when findings exist.
//
// These tests execute the step's own shell against stubs, extracted from the
// workflow rather than copied, so they cannot drift from what they cover.

const resolveStepName = "Resolve per-platform digests"

// resolveStep returns the body of the resolve step, by job and step name.
func resolveStep(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `json:"name"`
				Run  string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}
	for _, s := range doc.Jobs["resolve"].Steps {
		if s.Name == resolveStepName {
			return s.Run
		}
	}
	t.Fatalf("%s has no step %q in job \"resolve\"; if it was renamed, update this "+
		"test rather than deleting it -- the branch it covers is the one that runs "+
		"when the registry does not answer", vulnScanWorkflow, resolveStepName)
	return ""
}

// resolveHarness stubs the three commands the step shells out to.
//
// The stubs record their arguments rather than only returning a status. A stub
// that returns only an exit code cannot tell "the step asked the registry the
// right question" from "the step asked nothing at all" -- which is how a
// version of this step that resolved per-platform digests from a mutable tag
// instead of the pinned index would pass a status-only harness unchanged.
func resolveHarness(t *testing.T, dir, failTag string) {
	t.Helper()

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	// `timeout` is GNU coreutils and is absent on darwin, where these tests also
	// run. The step wraps every crane call in it, so it is stubbed to drop the
	// flag and duration and exec the rest. Nothing here is testing timeout.
	write(t, filepath.Join(bin, "timeout"), `#!/usr/bin/env bash
[[ "$1" == "--foreground" ]] && shift
shift
exec "$@"
`)

	// Distinct digests per platform, and neither equal to the index, so the
	// step's own multi-platform assertions pass on the happy path.
	write(t, filepath.Join(bin, "crane"), fmt.Sprintf(`#!/usr/bin/env bash
echo "$*" >> %q
ref="${!#}"
if [[ -n %q && "${ref}" == *%q* ]]; then
  echo "MANIFEST_UNKNOWN: manifest unknown" >&2
  exit 1
fi
if [[ "$*" == *"--platform linux/amd64"* ]]; then
  echo "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
elif [[ "$*" == *"--platform linux/arm64"* ]]; then
  echo "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
else
  echo "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
fi
`, filepath.Join(dir, "crane.log"), failTag, failTag))

	write(t, filepath.Join(bin, "gh"), fmt.Sprintf(`#!/usr/bin/env bash
echo "$*" >> %q
if [[ "$1" == "release" ]]; then
  echo "v1.2.3"
elif [[ "$1" == "api" ]]; then
  printf '%%s\n' aaaaaaa bbbbbbb ccccccc
fi
`, filepath.Join(dir, "gh.log")))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // stub must be executable
		t.Fatalf("write %s: %v", path, err)
	}
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func runResolve(t *testing.T, failTag string) (out string, failed bool, dir string) {
	t.Helper()

	dir = t.TempDir()
	resolveHarness(t, dir, failTag)
	out, failed = runShell(t, dir, resolveStep(t),
		"PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"),
		"IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager",
		"GITHUB_REPOSITORY=NVIDIA/cluster-readiness-engine",
		"GH_TOKEN=stub",
	)
	return out, failed, dir
}

// TestResolveRefusesAPartialTargetSet is the regression test for the defect
// that made every downstream completeness check unable to see a missing target.
func TestResolveRefusesAPartialTargetSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failTag string
		want    string
	}{
		{
			// The image operators are actually running.
			name:    "release image does not resolve",
			failTag: "v1.2.3",
			want:    "refusing to scan a subset",
		},
		{
			// The other half of the claimed coverage.
			name:    "main image does not resolve",
			failTag: "main-",
			want:    "no main-<sha> image found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, failed, dir := runResolve(t, tc.failTag)
			if !failed {
				t.Fatalf("resolve accepted a partial target set and exited 0:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("failed for the wrong reason\n got: %s\nwant substring: %s", out, tc.want)
			}
			// Exiting non-zero is not enough: a step that emitted a short target
			// list and then failed for an unrelated reason would still hand the
			// report job a set it would happily call complete.
			if got := readLog(t, filepath.Join(dir, "github_output")); strings.Contains(got, "value=") {
				t.Errorf("resolve published a target set on a failed run: %s", got)
			}
		})
	}
}

// TestResolveScansTheStableRelease pins the target selection itself.
//
// `gh release list --exclude-drafts --limit 1` returns the newest release
// including pre-releases, so during any RC window the scan would substitute the
// RC for the GA image and never scan what operators are running -- green, and
// silent, because Slack only fires on findings.
func TestResolveScansTheStableRelease(t *testing.T) {
	_, failed, dir := runResolve(t, "")
	if failed {
		t.Fatalf("resolve failed on the happy path")
	}

	gh := readLog(t, filepath.Join(dir, "gh.log"))
	if !strings.Contains(gh, "--exclude-pre-releases") {
		t.Errorf("resolve asked for the newest release without --exclude-pre-releases, "+
			"so an RC displaces the GA image operators run; gh calls were:\n%s", gh)
	}
}

// TestResolvePinsPlatformsToTheIndex holds the digest handoff.
//
// Re-resolving the tag for each platform lets a republish between calls pair an
// amd64 manifest from one index with an arm64 from another, under one target
// name -- and makes the step's own "equal to the index" assertion compare
// against a value that is no longer current. attest.yml resolves the index once
// and descends from it for exactly this reason.
func TestResolvePinsPlatformsToTheIndex(t *testing.T) {
	_, failed, dir := runResolve(t, "")
	if failed {
		t.Fatalf("resolve failed on the happy path")
	}

	for line := range strings.SplitSeq(readLog(t, filepath.Join(dir, "crane.log")), "\n") {
		if !strings.Contains(line, "--platform") {
			continue
		}
		if !strings.Contains(line, "@sha256:") {
			t.Errorf("per-platform digest resolved from a mutable tag rather than the "+
				"pinned index: %q", line)
		}
	}
}

// TestResolveEmitsEveryTarget is the accept side. Without it, a step that
// failed unconditionally would satisfy every rejection case above.
func TestResolveEmitsEveryTarget(t *testing.T) {
	out, failed, dir := runResolve(t, "")
	if failed {
		t.Fatalf("resolve rejected a fully resolvable set:\n%s", out)
	}

	got := readLog(t, filepath.Join(dir, "github_output"))
	if !strings.Contains(got, "value=") {
		t.Fatalf("resolve published no target set:\n%s", got)
	}
	// Two targets, two platforms each. The step asserts this itself; this
	// confirms the assertion is reachable and passes on a good run.
	for _, want := range []string{"release-amd64", "release-arm64", "main-amd64", "main-arm64"} {
		if !strings.Contains(got, want) {
			t.Errorf("target %q missing from the published set: %s", want, got)
		}
	}
}
