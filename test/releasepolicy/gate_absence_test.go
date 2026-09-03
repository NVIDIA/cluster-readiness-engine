// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release gate has been shown to reject a tampered artifact: cutting
// v0.2.0-rc.2 from a commit with a modified binary produced two signature
// failures (run 33766293155; the draft release was deleted afterwards, so the
// run is the surviving evidence). It has never been shown to reject an artifact
// that is simply not there, and absence takes different branches than tampering
// does.
//
// The distinction matters because the absence branches are the ones that run
// when a signing step silently no-ops -- the failure this gate exists for. A
// skipped job does not fail its caller, and a missing asset makes a
// verification loop verify the survivors. Both read correctly. So did every
// other check in this epic that turned out not to work.
//
// These tests execute the gate's own shell against stubs, so the branches run
// without cutting a tag.

// Mutations these tests are verified against. Each was applied to
// release.yml, the suite was run, and each failed. Recorded so the claim is
// checkable later rather than resting on a number in a commit message.
//
// Asset step:
//   1. missing-asset branch made fail-open (failed=1 dropped)
//   2. missing-bundle branch made fail-open
//   3. missing SBOM-binding branch made fail-open
//   4. checksums.txt absence made fail-open
//   5. `[[ ! -s ]]` weakened to `[[ ! -e ]]`, at each of its four sites
//   6. final `[[ failed -eq 0 ]] || exit 1` replaced with `true`
//   7. the `::error::` annotation removed from a failure branch
//   8. the ASSETS-loop cosign call replaced with `if true`
//   9. the SBOM-binding cosign call replaced with `if true`
//  10. `--certificate-identity`/`--certificate-oidc-issuer` dropped
//  11. `--type https://slsa.dev/provenance/v1` widened to `--type custom`
//  12. `--type cyclonedx` widened to `--type custom`
//  13. the failure branch made to name a fixed, wrong artifact
//
// Job guards, for each of the three:
//  14-16. the `skipped)` arm made benign
//  17-19. the catch-all `*)` arm made benign
//  20-21. the build and publish result checks removed
//
// 8 through 13 escaped the first version of these tests: the stub keyed on the
// trailing artifact, which both verification loops share, and ignored the
// arguments entirely. The gate could stop verifying signatures and stop pinning
// the identity with every case still green. That is what the recording stub
// and the assertions in `a complete release passes` exist for. 13 escaped for a
// second reason: the failure assertion matched the artifact name alone, and
// that name is a prefix of the next asset in the list, so a gate that always
// blamed the last artifact it looked at satisfied it. The assertion is the
// whole error line now, plus a count.

// gateStep returns the body of one release.yml step, found by job and step
// name. Extracted from the workflow rather than copied: a copy would drift, and
// a drifted copy would keep passing while the gate rotted.
func gateStep(t *testing.T, job, name string) string {
	t.Helper()

	for _, s := range runSteps(t) {
		if s.workflow == wfRelease && s.job == job && s.name == name {
			return s.run
		}
	}
	t.Fatalf("%s has no step %q in job %q; if it was renamed, update this test "+
		"rather than deleting it -- the branch it covers is the one that runs "+
		"when a signing step no-ops", wfRelease, name, job)
	return ""
}

// runShell writes a script and runs it, returning combined output and whether
// it failed. A malformed harness exits non-zero, which is indistinguishable
// from the gate refusing, so the script is syntax-checked first -- without
// that, every rejection case would pass even on a garbage extraction.
func runShell(t *testing.T, dir, script string, env ...string) (string, bool) {
	t.Helper()

	path := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("extracted step is not valid shell: %v\n%s", err, out)
	}

	cmd := exec.Command("bash", path)
	cmd.Dir = dir
	// Point the workflow-command files at the temp dir before the caller's
	// env, the way the sibling runner in this package does. Neither extracted
	// step writes to them today, but the asset step carries `id: assets` --
	// which is what you give a step you intend to read outputs from -- and the
	// step above it in the same job already appends to GITHUB_OUTPUT. The first
	// time this one does, an unguarded run under `make test-ci` would append to
	// the real CI job's file.
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+filepath.Join(dir, "github_output"),
		"GITHUB_ENV="+filepath.Join(dir, "github_env"),
		"GITHUB_PATH="+filepath.Join(dir, "github_path"),
		"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "github_step_summary"),
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err != nil
}

// The assets the gate requires, spelled out as the gate spells them.
//
// This is deliberately a second copy of the workflow's list, and the guarantee
// it buys runs in **one direction only**: if the gate starts requiring an asset
// this test does not create, the "complete release" case fails and names the
// drift. The reverse is invisible here. Adding a target to NVCRECTL_PLATFORMS
// produces a binary that is built, staged, checksummed and uploaded by glob but
// is in neither the attest-binaries matrix nor the gate's list nor this one --
// so it ships unsigned with every test in this file green. Four hardcoded lists
// against one glob-driven producer; closing that is #274's fourth invariant and
// needs a three-way set comparison against the Makefile, not another copy.
// darwinARM64 is named because the fixture and two verification cases share it.
const darwinARM64 = "nvcrectl-darwin-arm64"

var (
	gateBinaries = []string{
		"nvcrectl-linux-amd64",
		"nvcrectl-linux-arm64",
		"nvcrectl-darwin-amd64",
		darwinARM64,
	}
	gateLooseAssets = append(append([]string{}, gateBinaries...),
		"installer", "THIRD_PARTY_NOTICES.md")
)

// releaseFiles is every file a complete release publishes, which is what the
// gate downloads before it runs. 25 files, matching v0.2.0-rc.1 exactly.
func releaseFiles() []string {
	files := append([]string{}, gateLooseAssets...)
	for _, b := range gateBinaries {
		files = append(files, b+".cyclonedx.json")
	}
	// Every loose asset and every SBOM carries its own provenance bundle.
	for _, a := range append(append([]string{}, gateLooseAssets...), sbomNames()...) {
		files = append(files, a+".sigstore.json")
	}
	// Plus the bundle binding each SBOM to its binary.
	for _, b := range gateBinaries {
		files = append(files, b+".cyclonedx.sigstore.json")
	}
	return append(files, "checksums.txt")
}

func sbomNames() []string {
	out := make([]string, 0, len(gateBinaries))
	for _, b := range gateBinaries {
		out = append(out, b+".cyclonedx.json")
	}
	return out
}

// assetHarness stubs the two commands the step shells out to.
//
// cosign is stubbed as a function because the step invokes it through its own
// `retry` helper as `"$@"`, which resolves functions.
//
// The stub records every invocation rather than only returning a status, and
// it keys on the --bundle value rather than on the trailing artifact. Both
// choices are load-bearing. The step has two verification loops -- one over
// asset provenance, one binding each SBOM to its binary -- and they pass the
// same binary name as the final argument. Keying on that name tripped both
// loops at once, so replacing either loop's cosign call with `if true` left
// every case green: the gate could stop verifying signatures entirely and this
// file reported ok. The bundles are distinct, so keying on them is not.
//
// Recording the arguments is what lets the test assert the gate actually asked
// for the pinned identity. That the published identity is exact rather than a
// regexp is the hole this whole epic was filed to close.
const assetHarness = `
set -uo pipefail
export IDENTITY="` + wantIdentity + `"
export OIDC_ISSUER="` + wantIssuer + `"

sleep() { :; }

cosign() {
  local args=("$@")
  local bundle="" ptype="" identity="" issuer=""
  local i
  for ((i = 0; i < ${#args[@]}; i++)); do
    case "${args[i]}" in
      --bundle)                  bundle="${args[i+1]:-}" ;;
      --type)                    ptype="${args[i+1]:-}" ;;
      --certificate-identity)    identity="${args[i+1]:-}" ;;
      --certificate-oidc-issuer) issuer="${args[i+1]:-}" ;;
    esac
  done
  printf '%%s\t%%s\t%%s\t%%s\n' "${bundle}" "${ptype}" "${identity}" "${issuer}" \
    >> "${STUB_COSIGN_LOG}"
  case " ${STUB_COSIGN_FAIL} " in
    *" ${bundle} "*) return 1 ;;
  esac
  return 0
}

sha256sum() { [ "${STUB_CHECKSUMS_OK}" = "true" ]; }

%s
`

// The identity and issuer the gate must pin on every call.
const (
	wantIdentity = "https://github.com/NVIDIA/cluster-readiness-engine/" +
		".github/workflows/attest.yml@refs/tags/v1.2.3"
	wantIssuer = "https://token.actions.githubusercontent.com"
)

// cosignInvocation is one recorded invocation of the stub.
type cosignInvocation struct{ bundle, ptype, identity, issuer string }

// readCosignLog returns every cosign invocation the step made.
func readCosignLog(t *testing.T, path string) []cosignInvocation {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read cosign log: %v", err)
	}
	var calls []cosignInvocation
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			t.Fatalf("malformed cosign log line: %q", line)
		}
		calls = append(calls, cosignInvocation{f[0], f[1], f[2], f[3]})
	}
	return calls
}

// stageRelease writes a complete release into <dir>/verify, then applies the
// case's mutation. Returns the directory the step runs from.
func stageRelease(t *testing.T, omit string, truncate string) string {
	t.Helper()

	dir := t.TempDir()
	verify := filepath.Join(dir, "verify")
	if err := os.MkdirAll(verify, 0o755); err != nil {
		t.Fatalf("mkdir verify: %v", err)
	}
	for _, f := range releaseFiles() {
		if f == omit {
			continue
		}
		body := []byte("content of " + f + "\n")
		if f == truncate {
			// Present but empty. The gate tests with `-s`, not `-e`: a
			// zero-length upload is a failed upload, not an asset.
			body = nil
		}
		if err := os.WriteFile(filepath.Join(verify, f), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	return dir
}

// runAssetGate runs the step and returns its output, whether it failed, and
// every cosign call it made. The log path is absolute because the step does
// `cd verify` before anything else.
func runAssetGate(t *testing.T, step, dir, cosignFail string, checksumsOK bool) (string, bool, []cosignInvocation) {
	t.Helper()

	logPath := filepath.Join(dir, "cosign-calls.tsv")
	out, failed := runShell(t, dir, fmt.Sprintf(assetHarness, step),
		"STUB_COSIGN_FAIL="+cosignFail,
		"STUB_CHECKSUMS_OK="+fmt.Sprint(checksumsOK),
		"STUB_COSIGN_LOG="+logPath,
	)
	return out, failed, readCosignLog(t, logPath)
}

// TestGateRejectsMissingAssets covers #270's "deleting one asset before the
// gate fails the release" criterion.
//
// It exercises the verification step itself, not the `gh release download` that
// precedes it, so it proves the gate refuses to pass a release with a missing
// asset rather than tracing every route a deletion could take to get there.
//
// The gate derives what it expects from a static list rather than from the
// release's own inventory, precisely so that deleting an asset cannot make the
// check verify the survivors and report clean. That property has never been
// executed. Here it is, once per asset.
func TestGateRejectsMissingAssets(t *testing.T) {
	step := gateStep(t, "verify-release", "Verify every release asset and its bundle")

	t.Run("a complete release passes", func(t *testing.T) {
		out, failed, calls := runAssetGate(t, step, stageRelease(t, "", ""), "", true)
		if failed {
			t.Fatalf("the gate rejected a complete release; if an asset was added to "+
				"the gate's list, add it to releaseFiles too\n%s", out)
		}

		// Passing is not evidence the gate verified anything. Deleting either
		// verification loop outright also produces a clean run, so assert the
		// gate actually asked cosign about every bundle it is supposed to.
		want := map[string]string{}
		for _, a := range append(append([]string{}, gateLooseAssets...), sbomNames()...) {
			want[a+".sigstore.json"] = "https://slsa.dev/provenance/v1"
		}
		for _, b := range gateBinaries {
			want[b+".cyclonedx.sigstore.json"] = "cyclonedx"
		}

		got := map[string]bool{}
		for _, c := range calls {
			got[c.bundle] = true
			if wantType, ok := want[c.bundle]; ok && c.ptype != wantType {
				t.Errorf("%s was verified as %q, want %q", c.bundle, c.ptype, wantType)
			}
			// The exact identity is the property this epic exists to establish.
			// A gate that verifies without pinning it accepts a main-branch
			// build as a release, which is the defect that already shipped once.
			if c.identity != wantIdentity {
				t.Errorf("%s was verified against identity %q, want the pinned %q",
					c.bundle, c.identity, wantIdentity)
			}
			if c.issuer != wantIssuer {
				t.Errorf("%s was verified against issuer %q, want %q",
					c.bundle, c.issuer, wantIssuer)
			}
		}
		for bundle := range want {
			if !got[bundle] {
				t.Errorf("the gate never verified %s; a release can be published "+
					"with that signature unchecked", bundle)
			}
		}
	})

	// Every file a release publishes, removed one at a time.
	for _, missing := range releaseFiles() {
		t.Run("missing "+missing, func(t *testing.T) {
			out, failed, _ := runAssetGate(t, step, stageRelease(t, missing, ""), "", true)
			if !failed {
				t.Errorf("the gate published a release with %s deleted\n%s", missing, out)
			}
			if !strings.Contains(out, "::error::") {
				t.Errorf("the gate failed without saying why; a release engineer reading "+
					"this log learns nothing\n%s", out)
			}
		})
	}

	// A zero-length file is the shape a failed upload leaves behind, and it is
	// the case an existence test rather than a size test would let through.
	// One per `-s` test in the step. Without the SBOM-binding bundle, relaxing
	// that line alone to `-e` is silent: a zero-byte bundle passes presence,
	// then passes verification, and the release ships.
	for _, empty := range []string{
		"installer",
		"installer.sigstore.json",
		"nvcrectl-linux-amd64.cyclonedx.sigstore.json",
		"checksums.txt",
	} {
		t.Run("zero-length "+empty, func(t *testing.T) {
			out, failed, _ := runAssetGate(t, step, stageRelease(t, "", empty), "", true)
			if !failed {
				t.Errorf("the gate accepted a zero-length %s\n%s", empty, out)
			}
			if !strings.Contains(out, "::error::") {
				t.Errorf("the gate failed without saying why\n%s", out)
			}
		})
	}

	// Absence is not the only way a loop can be wrong. Each verification loop
	// gets its own case, keyed on a bundle only that loop passes, so a loop
	// removed outright cannot be masked by the other one still failing.
	//
	// The assertion is the whole error line, not the artifact name. A substring
	// check on the name alone is satisfied by the defect it is meant to exclude:
	// the name is a prefix of the next asset in the list, so a gate that always
	// reported the last artifact it looked at would still contain it.
	for _, c := range []struct{ name, bundle, wantError string }{
		{"an asset's provenance does not verify",
			"installer.sigstore.json",
			"::error::installer does not verify against "},
		{"an SBOM's binding to its binary does not verify",
			darwinARM64 + ".cyclonedx.sigstore.json",
			"::error::" + darwinARM64 + "'s SBOM binding does not verify"},
		{"an SBOM's own provenance does not verify",
			"nvcrectl-linux-amd64.cyclonedx.json.sigstore.json",
			"::error::nvcrectl-linux-amd64.cyclonedx.json does not verify against "},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, failed, _ := runAssetGate(t, step, stageRelease(t, "", ""), c.bundle, true)
			if !failed {
				t.Fatalf("the gate published a release although %s does not verify\n%s", c.bundle, out)
			}
			if !strings.Contains(out, c.wantError) {
				t.Errorf("the gate did not report the failure it should have\n want: %s\n got: %s",
					c.wantError, out)
			}
			// Exactly one, so a gate that fails everything indiscriminately --
			// or one loop leaking into the other -- is not mistaken for a gate
			// that identified the right artifact.
			if n := strings.Count(out, "::error::"); n != 1 {
				t.Errorf("want exactly 1 failure, got %d\n%s", n, out)
			}
		})
	}

	// Named for what it proves. sha256sum is stubbed and ignores its arguments,
	// so this covers the gate reacting to a failed check -- not sha256sum's own
	// judgement of whether the file describes the assets.
	t.Run("a failing checksum check stops the gate", func(t *testing.T) {
		out, failed, _ := runAssetGate(t, step, stageRelease(t, "", ""), "", false)
		if !failed {
			t.Errorf("the gate accepted a failing checksum check\n%s", out)
		}
		if !strings.Contains(out, "::error::") {
			t.Errorf("the gate failed without saying why\n%s", out)
		}
	})
}

// jobGuard is one of the three steps that refuse to proceed unless the
// attestation jobs they depend on actually succeeded.
type jobGuard struct {
	job, step string
	// results are the job-result variables the guard reads, in the order the
	// case tables below supply them.
	vars []string
}

var jobGuards = []jobGuard{
	{"image-attested", "Require every image attestation to have succeeded",
		[]string{"BUILD", "INDEX", "AMD64", "ARM64"}},
	{"chart-attested", "Require the chart attestation to have succeeded",
		[]string{"PUBLISH", "ATTEST"}},
	{"binaries-attested", "Require every binary attestation to have succeeded",
		[]string{"BUILD", "ATTEST"}},
}

func runJobGuard(t *testing.T, g jobGuard, step string, results []string) (string, bool) {
	t.Helper()

	env := make([]string, 0, len(g.vars))
	for i, v := range g.vars {
		env = append(env, v+"="+results[i])
	}
	return runShell(t, t.TempDir(), "set -uo pipefail\n"+step, env...)
}

// TestGateRejectsSkippedSigningJobs covers the job-level half of #270's
// "skipping any signing step fails the gate" criterion.
//
// Only the job-level half. These three guards read `needs.<job>.result` and
// nothing else, so disabling the signing *step* inside attest.yml while its job
// still succeeds passes all three. That case is caught later, by verify-release
// re-checking the signatures against the registry, which is a path this test
// does not cover and the criterion's "test by dispatch" prescribes.
//
// A skipped job does not fail its caller. That is the whole hazard: disable a
// signing job and the release does not go red, it goes green with an unsigned
// artifact. Each of the three guards exists to convert that silence into a
// failure, and none had ever run.
//
// `cancelled` is in the table because it is the result a timeout or a
// cancelled run produces, and it is neither `success` nor `skipped` -- a guard
// written as `if result == skipped` would let it through.
func TestGateRejectsSkippedSigningJobs(t *testing.T) {
	for _, g := range jobGuards {
		t.Run(g.job, func(t *testing.T) {
			// Hoisted: gateStep re-parses every release-path workflow, and the
			// result is the same for all of this guard's cases.
			step := gateStep(t, g.job, g.step)

			allGood := make([]string, len(g.vars))
			for i := range allGood {
				allGood[i] = "success"
			}

			t.Run("everything succeeded", func(t *testing.T) {
				out, failed := runJobGuard(t, g, step, allGood)
				if failed {
					t.Fatalf("the guard blocked a release in which every job succeeded\n%s", out)
				}
			})

			// Each dependency, one at a time, in each result a real run can
			// produce. Every one of them means the artifact is published and
			// unsigned, so every one must stop the release.
			for i, v := range g.vars {
				for _, result := range []string{"skipped", "failure", "cancelled"} {
					t.Run(v+"="+result, func(t *testing.T) {
						results := append([]string{}, allGood...)
						results[i] = result
						out, failed := runJobGuard(t, g, step, results)
						if !failed {
							t.Errorf("%s=%s did not stop the release; the artifact is "+
								"published and unsigned\n%s", v, result, out)
						}
						if !strings.Contains(out, "::error::") {
							t.Errorf("the guard failed without an error annotation\n%s", out)
						}
					})
				}
			}
		})
	}
}
