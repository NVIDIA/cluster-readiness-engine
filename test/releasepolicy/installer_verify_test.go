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

// verifyBlock returns the installer's signature-verification decision, from the
// `--skip-verify` branch to the end of the block.
//
// Extracted from the real script rather than copied. This covers the decision --
// when to verify, when to refuse, when to proceed -- and deliberately not
// `fetchReleaseAsset` or `resolveCosign`, which are stubbed below because they
// need the network. Those two are exercised end-to-end against a real release
// instead; what is pinned here is that no path reaches an install without
// either a verified signature or someone having typed --skip-verify.
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
stub-cosign()       { return %d; }

# The block calls cosign through "${COSIGN_BIN}", so route that to the stub.
stub_cosign_wrapper() { return %d; }

%s

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
		c.fetchRC, c.cosignRC, c.cosignRC, c.verifyRC, c.verifyRC, block)

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
		})
	}
}

// TestInstallerSkipVerifyIsExplicit pins that skipping is spelled out rather
// than inferred. The flag is long on purpose: a single letter is the kind of
// thing that gets copied out of an unrelated command line.
func TestInstallerSkipVerifyIsExplicit(t *testing.T) {
	raw, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "--skip-verify") {
		t.Error("installer has no --skip-verify flag; verification must be skippable only on request")
	}
	if strings.Contains(body, `SKIP_VERIFY=true`+"\n") && !strings.Contains(body, `--skip-verify) SKIP_VERIFY=true`) {
		t.Error("SKIP_VERIFY is set somewhere other than the --skip-verify flag; " +
			"it must never be inferred from a missing tool or a failed download")
	}
	// getopts handles short flags only. A single-letter form would mean the
	// long flag is decorative.
	if strings.Contains(body, `getopts "d:pv:s"`) || strings.Contains(body, `getopts "d:psv:"`) {
		t.Error("skip-verify has a short-flag form; it must be long-only")
	}
}
