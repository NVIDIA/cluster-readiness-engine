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

// gate names the one `changed-files` job in a workflow and the output key it
// writes.
//
// Parameterised because the pattern now has a second user. container-build.yml
// gates an expensive CUTLASS compile the same way these workflows gate the
// documentation checks, and the failure mode is identical: a skipped job reads
// as a passing one, so a gate that wrongly answers "nothing changed" hides
// whatever it was guarding. The key varies, the invariants do not.
// runSentinel and skipSentinel stand in for the answer in a shared case
// table, so one table can be run against gates with different output keys.
const (
	runSentinel  = "RUN"
	skipSentinel = "SKIP"
)

type gate struct {
	workflow string
	// job is the gate job's name. The fern workflows and container-build.yml
	// call it changed-files; publish.yml calls it gemm-changed.
	job string
	// key is the output name, so a gate writing "gemm=true" is checked the
	// same way as one writing "docs=true".
	key string
}

// jobChangedFiles is the gate job name every workflow but publish.yml uses.
const jobChangedFiles = "changed-files"

// The fail-open states every gate shares, named once so the two tables below
// cannot drift into describing the same case differently.
const (
	caseDiffFails  = "the diff itself fails"
	caseFetchFails = "main cannot be fetched"
	stubDiffFails  = `return 1`
	stubUnrelated  = `echo "docs/typo.md"; return 0`
)

// run and skip are the two answers a gate can give, as it writes them to
// GITHUB_OUTPUT. The whole trimmed file is compared against these, so a gate
// that writes a second line fails: an extra output is an extra claim nothing
// here has checked.
func (g gate) run() string  { return g.key + "=true" }
func (g gate) skip() string { return g.key + "=false" }

func (g gate) String() string { return g.workflow + "[" + g.key + "]" }

// docsGates are the workflows whose `changed-files` job decides whether
// documentation validation runs. Both must answer the question the same way.
var docsGates = []gate{
	{workflow: "fern-docs-ci.yml", job: jobChangedFiles, key: "docs"},
	{workflow: "fern-docs-preview-build.yml", job: jobChangedFiles, key: "docs"},
}

// imageGate decides whether the CUTLASS GEMM harness image is rebuilt on a
// pull request (ADR-091). `paths:` is a trigger-level filter and the trigger
// already fires on every Go change, which it must for the manager image, so
// this is the only thing standing between a Go-only pull request and a
// multi-hour kernel compile.
var imageGate = gate{workflow: "container-build.yml", job: jobChangedFiles, key: "gemm"}

// publishImageGate decides whether a merge to main rebuilds the harness image.
//
// Separate from imageGate because it answers a different question against a
// different baseline. container-build.yml asks "does this pull request touch
// the harness"; this one asks "has the harness changed since we last published
// one", and it has to, because a run that answered "build" and then did not
// finish leaves a change no later diff against HEAD~1 would ever see again.
var publishImageGate = gate{workflow: "publish.yml", job: "gemm-changed", key: "gemm"}

// gateStep returns the body of the `changes` step from a workflow's
// changed-files job. Extracted from the workflow rather than copied, so the
// test cannot drift from the shell it covers.
func gateStep(t *testing.T, g gate) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowDir, g.workflow))
	if err != nil {
		t.Fatalf("read %s: %v", g.workflow, err)
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
		t.Fatalf("parse %s: %v", g.workflow, err)
	}

	for _, step := range doc.Jobs[g.job].Steps {
		if step.ID == "changes" {
			return step.Run
		}
	}
	t.Fatalf("%s: no step with id 'changes' in the %s job; "+
		"if the gate moved, update this test rather than deleting it", g.workflow, g.job)
	return ""
}

// harness runs the extracted step with git stubbed, so the cases below control
// exactly what the gate sees.
const harness = `
git() {
  case "$1 $2" in
    "fetch --no-tags") return %d ;;
    # Echo back the revision the gate chose, so the diff stub can tell one
    # baseline from another. Which revision a gate compares against is the
    # thing under test, and a fixed sha here cannot express it. The existing
    # gates pass HEAD, which is non-empty exactly as a fixed sha was.
    "merge-base FETCH_HEAD") echo "$3"; return 0 ;;
    "diff --name-only") %s ;;
  esac
}

# The run-history lookup publish.yml's baseline uses. Returns a sha unlike any
# revision a gate could name lexically, so a diff stub asserting on the base
# cannot pass by accident.
gh() { echo "${STUB_LAST_SHA-lastpublishedsha}"; }

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
	// RUNS_API and GH_TOKEN are publish.yml's; the other gates do not read
	// them. Supplied unconditionally because the step runs under `set -u`, so
	// an absent one aborts the lookup and the gate fails open, which would make
	// every fail-open case here pass for the wrong reason.
	cmd.Env = append(os.Environ(),
		"GITHUB_OUTPUT="+output,
		"EVENT_NAME=push",
		"RUNS_API=/repos/o/r/actions/workflows/publish.yml/runs",
		"GH_TOKEN=stub",
	)
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

	// One matching path first, then far more than a pipe buffer of others,
	// for the image gate's own prefix.
	const bigImageDiff = `echo "images/cutlass-gemm/entrypoint.sh"; ` +
		`for i in $(seq 1 200000); do echo "pkg/file_$i.go"; done; return 0`

	for _, g := range docsGates {
		t.Run(g.String(), func(t *testing.T) {
			if got := runGate(t, gateStep(t, g), bigDiff, 0); got != g.run() {
				t.Errorf("gate skipped the checks on a large diff that changed a watched path\n"+
					" got: %s\nwant: %s\n"+
					"pipe `git diff` into `grep -q` under pipefail and an early match "+
					"looks like a failed pipeline", got, g.run())
			}
		})
	}
	t.Run(imageGate.String(), func(t *testing.T) {
		if got := runGate(t, gateStep(t, imageGate), bigImageDiff, 0); got != imageGate.run() {
			t.Errorf("gate skipped the image build on a large diff that changed the harness\n"+
				" got: %s\nwant: %s", got, imageGate.run())
		}
	})
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
			want: runSentinel,
		},
		{
			name: "fern config changed",
			diff: `echo "fern/docs.yml"; return 0`,
			want: runSentinel,
		},
		{
			name: "nothing relevant changed",
			diff: `echo "pkg/controller/job.go"; echo "README.md"; return 0`,
			want: skipSentinel,
		},
		{
			// `docs` and `fern` are anchored to the start of the path. A file
			// that merely contains them elsewhere is not a docs change.
			name: "path only mentions docs elsewhere",
			diff: `echo "pkg/docs/thing.go"; echo "internal/fern/x.go"; return 0`,
			want: skipSentinel,
		},
		{
			name: caseDiffFails,
			diff: stubDiffFails,
			want: runSentinel,
		},
		{
			name:    caseFetchFails,
			diff:    `echo "pkg/controller/job.go"; return 0`,
			fetchRC: 1,
			want:    runSentinel,
		},
	}

	for _, g := range docsGates {
		for _, c := range cases {
			t.Run(g.String()+"/"+c.name, func(t *testing.T) {
				want := g.run()
				if c.want == skipSentinel {
					want = g.skip()
				}
				if got := runGate(t, gateStep(t, g), c.diff, c.fetchRC); got != want {
					t.Errorf("gate answered wrongly\n got: %s\nwant: %s", got, want)
				}
			})
		}
	}
}

// TestImageGateAnswersHonestly is the docs cases' counterpart for the CUTLASS
// harness gate. Same invariants, different watched paths, and one extra
// consequence: skipping here does not hide a failing check, it ships an image
// that was never rebuilt from the sources that changed.
func TestImageGateAnswersHonestly(t *testing.T) {
	cases := []struct {
		name    string
		diff    string
		fetchRC int
		want    string
	}{
		{
			name: "harness script changed",
			diff: `echo "images/cutlass-gemm/entrypoint.sh"; return 0`,
			want: runSentinel,
		},
		{
			name: "harness Dockerfile changed",
			diff: `echo "images/cutlass-gemm/Dockerfile"; return 0`,
			want: runSentinel,
		},
		{
			name: "the workflow itself changed",
			diff: `echo ".github/workflows/container-build.yml"; return 0`,
			want: runSentinel,
		},
		{
			// The expensive case the gate exists for: a Go-only change fires
			// the trigger, because the manager image needs it, and must not
			// drag a kernel compile along.
			name: "go-only change does not rebuild the harness",
			diff: `echo "pkg/controller/job.go"; echo "go.mod"; return 0`,
			want: skipSentinel,
		},
		{
			// Anchored at the repository root. A path that merely contains the
			// directory name elsewhere is not a harness change.
			name: "path only mentions the directory elsewhere",
			diff: `echo "docs/images/cutlass-gemm/diagram.png"; echo "pkg/images/cutlass-gemm/x.go"; return 0`,
			want: skipSentinel,
		},
		{
			// The controller's own Dockerfile is a different image.
			name: "controller Dockerfile alone does not rebuild the harness",
			diff: `echo "Dockerfile"; return 0`,
			want: skipSentinel,
		},
		{
			name: caseDiffFails,
			diff: stubDiffFails,
			want: runSentinel,
		},
		{
			name:    caseFetchFails,
			diff:    `echo "pkg/controller/job.go"; return 0`,
			fetchRC: 1,
			want:    runSentinel,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := imageGate.run()
			if c.want == skipSentinel {
				want = imageGate.skip()
			}
			if got := runGate(t, gateStep(t, imageGate), c.diff, c.fetchRC); got != want {
				t.Errorf("gate answered wrongly\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// TestPublishGateComparesAgainstTheLastPublishedCommit pins the baseline
// publish.yml's harness gate measures from.
//
// The first version diffed against HEAD~1, which asks "did the single newest
// commit touch the harness". That question forgets. A run that answered "build"
// and then did not finish, cancelled to clear the queue, a lost runner, or a
// 5xx from GHCR on push, leaves its harness change unpublished; every later
// push then diffs against its own parent, so the change is never in a diff
// again, no run rebuilds it and no run goes red about it. The gate answers
// false forever and the image stays older than the branch it is named after,
// which is the one outcome the gate's own comment says it must not produce.
//
// Asserted by making the diff report a harness change only for the right
// baseline, so the two candidate revisions give opposite answers. Nothing
// covered this gate before: docsGates lists only the fern workflows, imageGate
// only container-build.yml, and releasepolicy's gate table is hardcoded to
// release.yml. That gap is how the HEAD~1 baseline got in.
func TestPublishGateComparesAgainstTheLastPublishedCommit(t *testing.T) {
	// The harness echoes back whichever revision the gate hands merge-base, so
	// $3 here is the base the gate chose.
	onlyForLastPublished := `case "$3" in
    lastpublishedsha) echo "images/cutlass-gemm/Dockerfile" ;;
  esac
  return 0`

	got := runGate(t, gateStep(t, publishImageGate), onlyForLastPublished, 0)
	if got != publishImageGate.run() {
		t.Errorf("the gate did not diff against the last published commit;"+
			" a harness change that no run published would be forgotten\n got: %s\nwant: %s",
			got, publishImageGate.run())
	}
}

// TestPublishGateFailsOpen pins that every path which cannot answer the
// question answers "build it".
//
// The gate's own comment commits to that, and it is the property that makes a
// path filter safe on a publishing workflow: a wrong skip leaves a dev image
// silently lagging the branch it is named after, while a wrong build costs
// minutes.
func TestPublishGateFailsOpen(t *testing.T) {
	cases := []struct {
		name    string
		diff    string
		fetchRC int
		noLast  bool
	}{
		{
			name: caseDiffFails,
			diff: stubDiffFails,
		},
		{
			name:    caseFetchFails,
			diff:    stubUnrelated,
			fetchRC: 1,
		},
		{
			// No successful publish run to measure from: a fresh fork, a
			// renamed workflow, or a run-history lookup that failed. There is
			// no baseline, so there is no basis for a skip.
			name:   "no successful publish run to compare against",
			diff:   stubUnrelated,
			noLast: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.noLast {
				t.Setenv("STUB_LAST_SHA", "")
			}
			got := runGate(t, gateStep(t, publishImageGate), c.diff, c.fetchRC)
			if got != publishImageGate.run() {
				t.Errorf("the gate skipped the build on a path that cannot answer"+
					" the question\n got: %s\nwant: %s", got, publishImageGate.run())
			}
		})
	}
}

// TestPublishGateSkipsWhenTheHarnessIsUntouched pins the other direction, so
// the fail-open cases above cannot be satisfied by a gate that always builds.
func TestPublishGateSkipsWhenTheHarnessIsUntouched(t *testing.T) {
	got := runGate(t, gateStep(t, publishImageGate),
		`echo "docs/typo.md"; echo "pkg/controller/job.go"; return 0`, 0)
	if got != publishImageGate.skip() {
		t.Errorf("the gate rebuilt the harness for a diff that does not touch it\n got: %s\nwant: %s",
			got, publishImageGate.skip())
	}
}
