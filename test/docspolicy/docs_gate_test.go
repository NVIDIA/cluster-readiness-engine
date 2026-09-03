// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package docspolicy tests the gate that decides whether the documentation
// checks run at all.
//
// It is separate from releasepolicy because these are not release-path
// workflows and none of that package's invariants (pinned signing identity,
// retried network calls, failure handlers last) apply to them. What they share
// is the failure mode: a gate that answers "nothing to check" is indistinguish-
// able, on the pull request page, from a check that ran and passed.
package docspolicy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const workflowDir = "../../.github/workflows"

// The two answers the gate can give, as it writes them to GITHUB_OUTPUT.
const (
	runChecks  = "docs=true"
	skipChecks = "docs=false"
)

// docsGateWorkflows are the workflows whose `changed-files` job decides whether
// documentation validation runs. Both must answer the question the same way.
var docsGateWorkflows = []string{"fern-docs-ci.yml", "fern-docs-preview-build.yml"}

// gateStep returns the body of the `changes` step from a workflow's
// changed-files job. Extracted from the workflow rather than copied, so the
// test cannot drift from the shell it covers.
func gateStep(t *testing.T, workflow string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowDir, workflow))
	if err != nil {
		t.Fatalf("read %s: %v", workflow, err)
	}

	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				ID  string `json:"id"`
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", workflow, err)
	}

	for _, step := range doc.Jobs["changed-files"].Steps {
		if step.ID == "changes" {
			return step.Run
		}
	}
	t.Fatalf("%s: no step with id 'changes' in the changed-files job; "+
		"if the gate moved, update this test rather than deleting it", workflow)
	return ""
}

// harness runs the extracted step with git stubbed, so the cases below control
// exactly what the gate sees.
const harness = `
git() {
  case "$1 $2" in
    "fetch --no-tags") return %d ;;
    "merge-base FETCH_HEAD") echo "deadbeef"; return 0 ;;
    "diff --name-only") %s ;;
  esac
}

%s
`

// runGate executes the gate and returns what it wrote to GITHUB_OUTPUT.
func runGate(t *testing.T, step, diff string, fetchRC int) string {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "gate.sh")
	output := filepath.Join(dir, "output")

	if err := os.WriteFile(script, []byte(fmt.Sprintf(harness, fetchRC, diff, step)), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatalf("create output: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+output, "EVENT_NAME=push")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gate exited non-zero: %v\n%s", err, out)
	}

	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	return strings.TrimSpace(string(got))
}

// TestDocsGateDoesNotSkipOnLargeDiffs pins the defect that shipped in this gate
// and would have silently disabled the documentation checks.
//
// `grep -q` exits at its first match, which sends SIGPIPE to a `git diff` still
// writing paths. Under `pipefail` the pipeline then reports 141 even though the
// match succeeded, so a branch that really did change docs/ is reported as
// having changed nothing and the checks are skipped. A skipped job reads as a
// passing run, which is how the break this workflow now guards against reached
// main in the first place.
//
// The diff has to outrun the pipe buffer for the race to fire, so a realistic
// small diff will not reproduce it -- which is exactly why it needs a test
// rather than a look.
func TestDocsGateDoesNotSkipOnLargeDiffs(t *testing.T) {
	// One matching path first, then far more than a pipe buffer of others.
	const bigDiff = `echo "docs/first.md"; for i in $(seq 1 200000); do echo "pkg/file_$i.go"; done; return 0`

	for _, wf := range docsGateWorkflows {
		t.Run(wf, func(t *testing.T) {
			if got := runGate(t, gateStep(t, wf), bigDiff, 0); got != runChecks {
				t.Errorf("gate skipped the docs checks on a large diff that changed docs/\n"+
					" got: %s\nwant: %s\n"+
					"pipe `git diff` into `grep -q` under pipefail and an early match "+
					"looks like a failed pipeline", got, runChecks)
			}
		})
	}
}

// TestDocsGateAnswersHonestly covers the rest of the decision. Every case the
// gate cannot answer must run the checks: skipping is the outcome that hides a
// problem, so it is only correct when the gate positively established that
// nothing relevant changed.
func TestDocsGateAnswersHonestly(t *testing.T) {
	cases := []struct {
		name    string
		diff    string
		fetchRC int
		want    string
	}{
		{
			name: "docs changed",
			diff: `echo "docs/getting-started/install.md"; return 0`,
			want: runChecks,
		},
		{
			name: "fern config changed",
			diff: `echo "fern/docs.yml"; return 0`,
			want: runChecks,
		},
		{
			name: "nothing relevant changed",
			diff: `echo "pkg/controller/job.go"; echo "README.md"; return 0`,
			want: skipChecks,
		},
		{
			// `docs` and `fern` are anchored to the start of the path. A file
			// that merely contains them elsewhere is not a docs change.
			name: "path only mentions docs elsewhere",
			diff: `echo "pkg/docs/thing.go"; echo "internal/fern/x.go"; return 0`,
			want: skipChecks,
		},
		{
			name: "the diff itself fails",
			diff: `return 1`,
			want: runChecks,
		},
		{
			name:    "main cannot be fetched",
			diff:    `echo "pkg/controller/job.go"; return 0`,
			fetchRC: 1,
			want:    runChecks,
		},
	}

	for _, wf := range docsGateWorkflows {
		for _, c := range cases {
			t.Run(wf+"/"+c.name, func(t *testing.T) {
				if got := runGate(t, gateStep(t, wf), c.diff, c.fetchRC); got != c.want {
					t.Errorf("gate answered wrongly\n got: %s\nwant: %s", got, c.want)
				}
			})
		}
	}
}
