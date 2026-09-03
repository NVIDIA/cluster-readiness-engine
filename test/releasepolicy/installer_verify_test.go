// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// verifyBlock returns the installer's signature-verification decision, from the
// `--skip-verify` branch to the end of the block.
//
// Extracted from the real script rather than copied. This covers the decision --
// when to verify, when to refuse, when to proceed -- and deliberately not
// `fetchReleaseAsset` or `resolveCosign`, which are stubbed below because they
// need the network.
//
// Those two have **no automated coverage**. They were exercised by hand against
// v0.2.0-rc.1 when this landed, which is not the same thing and will not catch a
// regression: a change to the asset-name escaping, to `gh release download
// --pattern`, or to the bootstrap digest check would ship without failing a
// test. Adding that coverage needs either a local asset server or a live
// release, and is worth doing.
//
// What is pinned here is narrower and still worth having: no path reaches an
// install without either a verified signature or someone having typed
// --skip-verify.
func verifyBlock(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, `if [[ "${SKIP_VERIFY}" == "true" ]]; then`) {
			start = i
			break
		}
	}
	if start == -1 {
		t.Fatal("could not find the signature-verification block in installer; " +
			"if it was renamed or removed, update this test rather than deleting it")
	}
	for i := start; i < len(lines); i++ {
		if lines[i] == "fi" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatal("could not find the end of the signature-verification block")
	return ""
}

// verifyHarness supplies the variables and commands the block reads. Every
// external dependency is a stub whose behaviour the cases control.
const verifyHarness = `
set -uo pipefail
err() { echo "REFUSED: $*"; exit 9; }
msg() { :; }
ok()  { echo "VERIFIED"; }
COLOR_RED='' ; COLOR_RESET=''

SKIP_VERIFY=%s
VERSION="v0.2.0-rc.1"
GH_REPO="NVIDIA/cluster-readiness-engine"
BINARY_NAME="nvcrectl-linux-amd64"
TEMP_DIR="$(mktemp -d)"
OS=linux ; ARCH=amd64
SIGSTORE_ISSUER="https://token.actions.githubusercontent.com"
SIGSTORE_IDENTITY_PREFIX="https://github.com/${GH_REPO}/.github/workflows/attest.yml@refs/tags"

fetchReleaseAsset() { return %d; }
resolveCosign()     { [ %d -eq 0 ] && echo "stub-cosign" ; return %d; }

# The block invokes cosign through "${COSIGN_BIN}", which resolveCosign sets to
# this function. It records its arguments so the cases can assert on the flags:
# a stub that ignores them lets --certificate-identity be swapped for
# --certificate-identity-regexp, or --type dropped, without any test noticing.
stub-cosign() { printf '%%s\n' "$*" > "${TEMP_DIR}/cosign-args"; return %d; }

%s

[ -f "${TEMP_DIR}/cosign-args" ] && echo "COSIGN_ARGS: $(cat "${TEMP_DIR}/cosign-args")"
echo "PROCEEDED"
`

type verifyCase struct {
	name       string
	skipVerify bool
	fetchRC    int // 0 = bundle downloaded
	cosignRC   int // 0 = cosign available
	verifyRC   int // 0 = signature verifies
	wantRefuse bool
	wantVerify bool // the ok "Signature verified." path ran
}

func runVerify(t *testing.T, block string, c verifyCase) (string, bool) {
	t.Helper()

	skip := "false"
	if c.skipVerify {
		skip = "true"
	}
	script := fmt.Sprintf(verifyHarness, skip,
		c.fetchRC, c.cosignRC, c.cosignRC, c.verifyRC, block)

	path := filepath.Join(t.TempDir(), "verify.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	// A malformed extraction exits non-zero, which is indistinguishable from a
	// refusal and would pass every refusal case for the wrong reason.
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("extracted block is not valid shell: %v\n%s", err, out)
	}

	out, err := exec.Command("bash", path).CombinedOutput()
	return strings.TrimSpace(string(out)), err != nil
}

// TestInstallerVerifiesOrRefuses pins the property the installer exists to
// provide: it does not install a binary whose signature it could not check.
//
// checksums.txt is served from the same origin as the binary and is itself
// unsigned, so it detects corruption rather than tampering. The signature is
// the check that means anything, which is why every failure below refuses
// rather than falling back to the checksum -- a silent downgrade to a check
// that proves nothing is worse than no check, because it reads as verified.
func TestInstallerVerifiesOrRefuses(t *testing.T) {
	block := verifyBlock(t)

	cases := []verifyCase{
		{
			name:       "signature verifies",
			wantVerify: true,
		},
		{
			// A release older than the attestation work carries no bundle. That
			// is not a reason to install it unverified.
			name:       "bundle cannot be downloaded",
			fetchRC:    1,
			wantRefuse: true,
		},
		{
			// The bare-machine case. Missing cosign must never be read as
			// permission to skip -- that is the inference --skip-verify exists
			// to make explicit.
			name:       "cosign unavailable and cannot be bootstrapped",
			cosignRC:   1,
			wantRefuse: true,
		},
		{
			name:       "signature does not verify",
			verifyRC:   1,
			wantRefuse: true,
		},
		{
			name:       "operator asked to skip",
			skipVerify: true,
		},
		{
			// Skipping is a decision about this run, not a repair of a broken
			// release: it must not depend on anything being fetchable.
			name:       "operator asked to skip and nothing is fetchable",
			skipVerify: true,
			fetchRC:    1,
			cosignRC:   1,
			verifyRC:   1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, failed := runVerify(t, block, c)

			if c.wantRefuse {
				if !failed {
					t.Errorf("installer continued to install; it must refuse\noutput: %s", out)
				}
				if strings.Contains(out, "VERIFIED") {
					t.Errorf("installer reported the signature verified on a failure path\noutput: %s", out)
				}
				return
			}
			if failed {
				t.Fatalf("installer refused but should have continued\noutput: %s", out)
			}
			if !strings.Contains(out, "PROCEEDED") {
				t.Errorf("installer did not reach the install step\noutput: %s", out)
			}
			if got := strings.Contains(out, "VERIFIED"); got != c.wantVerify {
				t.Errorf("verified-path ran = %v, want %v\noutput: %s", got, c.wantVerify, out)
			}
			if !c.wantVerify {
				return
			}
			// The flags are the verification. An exact identity that becomes a
			// regexp, or a missing --type, still "passes" a stub that ignores
			// its arguments -- and both are real weakenings.
			identity := "--certificate-identity https://github.com/NVIDIA/cluster-readiness-engine" +
				"/.github/workflows/attest.yml@refs/tags/v0.2.0-rc.1"
			for _, want := range []string{
				identity,
				"--type https://slsa.dev/provenance/v1",
				"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("cosign was not called with %q\noutput: %s", want, out)
				}
			}
			if strings.Contains(out, "--certificate-identity-regexp") {
				t.Error("cosign was called with --certificate-identity-regexp; " +
					"an identity naming no workflow and no ref also accepts a branch build")
			}
		})
	}
}

// argParseBlock returns the installer's argument pre-parsing plus the getopts
// loop -- the code that connects `--skip-verify` on the command line to the
// SKIP_VERIFY variable the verification block reads.
func argParseBlock(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	start, getopts := -1, -1
	for i, l := range lines {
		if start == -1 && strings.HasPrefix(l, "ARGS=()") {
			start = i
		}
		if start != -1 && strings.HasPrefix(l, "while getopts") {
			getopts = i
			break
		}
	}
	if start == -1 || getopts == -1 {
		t.Fatal("could not find the argument parsing in installer")
	}
	for i := getopts; i < len(lines); i++ {
		if lines[i] == "done" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatal("could not find the end of the getopts loop")
	return ""
}

// TestInstallerSkipVerifyFlagIsWired runs the real argument parsing.
//
// Without this, deleting the `--skip-verify)` case arm passes every other test
// in this file: the verification block is exercised by setting SKIP_VERIFY
// directly, so the flag can quietly become a no-op while remaining documented
// in usage text and error messages. A flag that is advertised and does nothing
// is worse than an absent one.
func TestInstallerSkipVerifyFlagIsWired(t *testing.T) {
	block := argParseBlock(t)

	cases := []struct {
		name string
		args string
		want string // "SKIP_VERIFY=<v> VERSION=<v> PRERELEASE=<v> DIR=<v>"
	}{
		{
			name: "no arguments", args: ``,
			want: "SKIP_VERIFY=false VERSION= PRERELEASE=false DIR=/usr/local/bin",
		},
		{
			name: "skip-verify alone", args: `--skip-verify`,
			want: "SKIP_VERIFY=true VERSION= PRERELEASE=false DIR=/usr/local/bin",
		},
		{
			name: "short flags still parse", args: `-v v1.2.3 -d /opt/bin -p`,
			want: "SKIP_VERIFY=false VERSION=v1.2.3 PRERELEASE=true DIR=/opt/bin",
		},
		{
			name: "skip-verify alongside short flags", args: `--skip-verify -v v1.2.3`,
			want: "SKIP_VERIFY=true VERSION=v1.2.3 PRERELEASE=false DIR=/usr/local/bin",
		},
		{
			name: "skip-verify after short flags", args: `-v v1.2.3 --skip-verify`,
			want: "SKIP_VERIFY=true VERSION=v1.2.3 PRERELEASE=false DIR=/usr/local/bin",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			script := fmt.Sprintf(`
set -uo pipefail
usage() { echo "USAGE"; exit 2; }
INSTALL_DIR="/usr/local/bin"
PRERELEASE=false
VERSION=""
SKIP_VERIFY=false
set -- %s

%s

echo "SKIP_VERIFY=${SKIP_VERIFY} VERSION=${VERSION} PRERELEASE=${PRERELEASE} DIR=${INSTALL_DIR}"
`, c.args, block)

			path := filepath.Join(t.TempDir(), "args.sh")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatalf("write harness: %v", err)
			}
			out, err := exec.Command("bash", path).CombinedOutput()
			if err != nil {
				t.Fatalf("argument parsing failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Errorf("argument parsing produced the wrong state\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}

// TestInstallerSkipVerifyIsNeverInferred pins that SKIP_VERIFY is only ever set
// by the flag.
//
// The whole point of the long flag is that verification is skipped when someone
// asks, and never because a tool was missing or a download failed. An earlier
// version of this test tried to express that with substring matching and could
// not fail: it looked for `SKIP_VERIFY=true` followed by a newline, but the real
// line ends `SKIP_VERIFY=true ;;`, so the check was dead. Assert on every
// assignment instead, which is a property rather than a spelling.
func TestInstallerSkipVerifyIsNeverInferred(t *testing.T) {
	raw, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}

	assign := regexp.MustCompile(`SKIP_VERIFY=`)
	for i, line := range strings.Split(string(raw), "\n") {
		if !assign.MatchString(line) {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "#"):
		case trimmed == "SKIP_VERIFY=false":
		case strings.HasPrefix(trimmed, `--skip-verify) SKIP_VERIFY=true`):
		case strings.Contains(trimmed, `"${SKIP_VERIFY}"`):
		default:
			t.Errorf("installer:%d assigns SKIP_VERIFY outside the --skip-verify flag: %q\n"+
				"skipping verification must never be inferred from a missing tool or a failed download",
				i+1, trimmed)
		}
	}

	// getopts takes short flags only, so a short spelling would mean the long
	// flag is decorative. Parse the real optstring rather than matching a few
	// guessed spellings of it.
	m := regexp.MustCompile(`getopts\s+"([^"]*)"`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find the getopts optstring in installer")
	}
	if strings.Contains(string(m[1]), "s") {
		t.Errorf("getopts optstring %q accepts a short -s flag; skip-verify must be long-only", m[1])
	}
}
