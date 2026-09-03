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

// installerPath is the script under test. It is shell, so nothing type-checks
// it, and a weakened guard would not break a build -- it would just stop
// refusing things. That is the same reason attest.yml's input validation is
// tested here rather than trusted.
const installerPath = "../../installer"

// draftGuard returns the installer's draft-refusal block, from the line that
// seeds the state to the end of the case statement that acts on it.
//
// Extracted from the real script rather than copied, so the test cannot drift
// from the code it covers. If the markers move, the test fails loudly instead
// of silently covering nothing.
func draftGuard(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatalf("read installer: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, `IS_DRAFT="unknown"`) {
			start = i
			break
		}
	}
	if start == -1 {
		t.Fatal(`could not find the start of the draft guard (IS_DRAFT="unknown") in installer; ` +
			"if the guard was renamed or removed, update this test rather than deleting it")
	}

	// The closing `esac` of the outer case, matched unindented. The guard
	// contains an inner `case` inside a retry loop, and stopping at the first
	// `esac` truncated the block into a syntax error -- which exits non-zero and
	// so read as "the guard refused", passing the refusal cases for the wrong
	// reason. runGuard syntax-checks the result to keep that from recurring.
	for i := start; i < len(lines); i++ {
		if lines[i] == "esac" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatal("could not find the end of the draft guard (unindented esac) in installer")
	return ""
}

// guardCase is one run of the guard against a stubbed GitHub.
type guardCase struct {
	name string
	// ghAvailable and token select which of the three branches runs.
	ghAvailable string
	token       string
	// ghStdout, ghStderr and ghExit stub the `gh release view` call.
	ghStdout, ghStderr string
	ghExit             int
	// httpCode and body stub the curl call.
	httpCode, body string
	// refused is whether the guard must stop the install.
	refused bool
	// state is the value the guard must settle on when it does not refuse.
	state string
}

// harness wraps the extracted guard with the variables and commands it reads,
// so the block runs exactly as it does in the script but against a stub.
const harness = `
set -uo pipefail
err() { echo "REFUSED: $*"; exit 9; }
VERSION="v1.2.3"
GH_REPO="NVIDIA/cluster-readiness-engine"
GH_API_URL="https://api.github.com/repos/NVIDIA/cluster-readiness-engine"
AUTH_HEADERS=()
RELEASE_JSON=""
GH_AVAILABLE="%s"
GH_TOKEN="%s"

# Never actually wait: the retry backoff is not what these cases exercise.
sleep() { :; }

gh() {
  printf '%%s' "${STUB_GH_STDOUT}"
  printf '%%s' "${STUB_GH_STDERR}" >&2
  return "${STUB_GH_EXIT}"
}

curl() {
  # Mirror the real call's -o <file> handling so the guard reads a body.
  local out=""
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "-o" ]; then out="$2"; shift 2; continue; fi
    shift
  done
  [ -n "${out}" ] && printf '%%s' "${STUB_BODY}" > "${out}"
  printf '%%s' "${STUB_HTTP_CODE}"
  return 0
}

%s

echo "PROCEEDED:${IS_DRAFT}"
`

func runGuard(t *testing.T, guard string, c guardCase) (string, bool) {
	t.Helper()

	script := fmt.Sprintf(harness, c.ghAvailable, c.token, guard)
	path := filepath.Join(t.TempDir(), "guard.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	// A malformed harness exits non-zero, which is indistinguishable from the
	// guard refusing an install. Without this, every refusal case would pass
	// even if the extracted block were garbage.
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("extracted guard is not valid shell: %v\n%s", err, out)
	}

	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(),
		"STUB_GH_STDOUT="+c.ghStdout,
		"STUB_GH_STDERR="+c.ghStderr,
		"STUB_GH_EXIT="+fmt.Sprint(c.ghExit),
		"STUB_HTTP_CODE="+c.httpCode,
		"STUB_BODY="+c.body,
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err != nil
}

// TestInstallerDraftGuard pins the state machine that decides whether the
// installer will install a release.
//
// A draft is a build the release gate has not cleared, or has refused. The
// guard must refuse one, must install a published release, and must refuse
// rather than guess when it cannot read the state -- an unreadable state is not
// "published". The one benign unknown is the anonymous path, where no draft is
// reachable at all, so refusing there would break every unauthenticated install
// to guard against something that cannot happen.
//
// Three of these cases are regressions for defects that shipped in this guard:
// stderr merged into the captured value, a 200 with no readable draft field
// read as "published", and a 404 retried as though it were an outage.
func TestInstallerDraftGuard(t *testing.T) {
	guard := draftGuard(t)

	cases := []guardCase{
		{
			name:        "gh reports a draft",
			ghAvailable: boolTrue, ghStdout: boolTrue + "\n",
			refused: true,
		},
		{
			name:        "gh reports a published release",
			ghAvailable: boolTrue, ghStdout: boolFalse + "\n",
			state: boolFalse,
		},
		{
			// gh prints an upgrade notice to stderr once every 24h. Merging it
			// into the captured value put it inside IS_DRAFT on a successful
			// call, which matched no case arm and refused a published release.
			name:        "gh succeeds while writing an upgrade notice to stderr",
			ghAvailable: boolTrue, ghStdout: boolFalse + "\n",
			ghStderr: "A new release of gh is available: 2.97.0 -> 2.98.0\n",
			state:    boolFalse,
		},
		{
			name:        "gh reports no such release",
			ghAvailable: boolTrue, ghExit: 1, ghStderr: "release not found\n",
			state: "absent",
		},
		{
			name:        "gh fails with an outage on every attempt",
			ghAvailable: boolTrue, ghExit: 1, ghStderr: "HTTP 503 Service Unavailable\n",
			refused: true,
		},
		{
			// Not an answer. Treating anything that is not "true" as published
			// is the fail-open this guard exists to prevent.
			name:        "gh succeeds but prints something unexpected",
			ghAvailable: boolTrue, ghStdout: "null\n",
			refused: true,
		},
		{
			name:  "token path reports a draft",
			token: "t", httpCode: "200", body: `{"tag_name":"v1.2.3","draft": true}`,
			refused: true,
		},
		{
			name:  "token path reports a published release",
			token: "t", httpCode: "200", body: `{"tag_name":"v1.2.3","draft": false}`,
			state: boolFalse,
		},
		{
			name:  "token path reports no such release",
			token: "t", httpCode: "404", body: `{"message":"Not Found"}`,
			state: "absent",
		},
		{
			// A TLS-interception page, a WAF block or a CDN glitch can answer
			// 200 with a body that has no draft field. That is not evidence the
			// release is published.
			name:  "token path gets 200 with a body that has no draft field",
			token: "t", httpCode: "200", body: "<html><body>Blocked by policy</body></html>",
			refused: true,
		},
		{
			name:  "token path gets a server error on every attempt",
			token: "t", httpCode: "500", body: "",
			refused: true,
		},
		{
			// No gh and no token: drafts are not reachable, so there is nothing
			// to determine and refusing would break every anonymous install.
			name:  "anonymous caller",
			state: "skipped",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, failed := runGuard(t, guard, c)

			if c.refused {
				if !failed {
					t.Errorf("guard allowed the install but must refuse it; output: %s", out)
				}
				return
			}
			if failed {
				t.Fatalf("guard refused the install but must allow it; output: %s", out)
			}
			want := "PROCEEDED:" + c.state
			if !strings.Contains(out, want) {
				t.Errorf("guard settled on the wrong state\n got: %s\nwant: %s", out, want)
			}
		})
	}
}

// TestInstallerDraftGuardRefusesUnknownStates is the negative half: every value
// the guard does not positively recognise must refuse.
//
// The exhaustive list matters because the guard reads values produced by two
// external commands. `true` is the only value that means "draft", but anything
// that is not a recognised answer must also stop the install rather than fall
// through to it.
func TestInstallerDraftGuardRefusesUnknownStates(t *testing.T) {
	guard := draftGuard(t)

	// Values a broken or changed `gh` could plausibly emit on exit 0.
	for _, out := range []string{"", "null", "TRUE", "true false", "{\"isDraft\":false}"} {
		t.Run("gh emits "+strings.ReplaceAll(out, " ", "_"), func(t *testing.T) {
			_, failed := runGuard(t, guard, guardCase{
				ghAvailable: boolTrue, ghStdout: out + "\n",
			})
			if !failed {
				t.Errorf("guard accepted %q as an answer; only a literal true or false is one", out)
			}
		})
	}
}
