// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The workflows that can produce or verify a release artifact. Every policy
// test in this package scans this set, so it is defined once: a workflow added
// to the release path must be added here to come under the invariants.
const (
	wfRelease     = "release.yml"
	wfPublish     = "publish.yml"
	wfAttest      = "attest.yml"
	wfBuildImage  = "build-image.yml"
	wfAttestSmoke = "attest-selftest.yml"
)

var releasePathWorkflows = []string{wfRelease, wfPublish, wfAttest, wfBuildImage, wfAttestSmoke}

// onReleasePath reports whether a workflow file is part of the release path.
func onReleasePath(base string) bool {
	return slices.Contains(releasePathWorkflows, base)
}

// workflowFiles lists the workflow definitions, sorted for stable output.
func workflowFiles(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no workflows found under %s", workflowDir)
	}
	sort.Strings(paths)
	return paths
}

// step is one run step plus the environment it can actually see.
type runStep struct {
	workflow string
	job      string
	name     string
	run      string
	env      map[string]bool
}

// runSteps returns every `run:` step in the release-path workflows, with the
// step, job and workflow `env` blocks merged into one visible set.
func runSteps(t *testing.T) []runStep {
	t.Helper()

	var out []runStep
	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		if !onReleasePath(base) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		var doc struct {
			Env  map[string]string `json:"env"`
			Jobs map[string]struct {
				Env   map[string]string `json:"env"`
				Steps []struct {
					Name string            `json:"name"`
					Run  string            `json:"run"`
					Env  map[string]string `json:"env"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for jobName, job := range doc.Jobs {
			for _, s := range job.Steps {
				if strings.TrimSpace(s.Run) == "" {
					continue
				}
				env := map[string]bool{}
				for _, m := range []map[string]string{doc.Env, job.Env, s.Env} {
					for k := range m {
						env[k] = true
					}
				}
				out = append(out, runStep{
					workflow: base, job: jobName, name: s.Name, run: s.Run, env: env,
				})
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no run steps found in the release-path workflows")
	}
	return out
}

// commandPosition matches the places a bare word can start a command: the
// beginning of a line, after a list or pipeline operator, inside a substitution,
// after `!`, or after a keyword that introduces one. Anchoring on it is what
// keeps `curl --retry 5` and `--retry-delay` out of the match -- a flag is
// preceded by `-`, which is not a command position.
//
// The first version of this pattern accepted only line start, `|`, `&`, `;` and
// `$(`. Every call in `Verify every release asset and its bundle` reads
// `if retry cosign ...`, which matched none of them, so the one step this test
// was written about was skipped entirely: deleting its `retry()` definition
// still passed.
const commandPosition = "(?:^|[|&;(){\x60]|!|\\b(?:if|then|else|elif|do|while|until|time)\\b)"

// retryCall matches an invocation of the retry helper, not curl's --retry flag.
var retryCall = regexp.MustCompile(`(?m)` + commandPosition + `\s*retry\s+\S`)

// firstRealMatch returns the offset of the first match that is not inside a
// comment, or -1. A comment naming a command runs nothing.
func firstRealMatch(re *regexp.Regexp, body string) int {
	for _, m := range re.FindAllStringIndex(body, -1) {
		if !inComment(body, m[0]) {
			return m[0]
		}
	}
	return -1
}

// TestRetryHelperIsInScope catches a defect that shipped in this gate's first
// draft and would have failed every release.
//
// Each `run:` block executes in its own shell, so a function defined in one step
// does not exist in the next. `retry` was defined in the asset-verification step
// and called in the image-verification step, where it resolved to
// `retry: command not found`. Every call took the failure branch, the
// tag-to-digest comparison saw an empty value, and the step exited 1 on a
// perfectly good release — with error text blaming the signatures.
//
// The shell would not complain at lint time and actionlint does not model
// cross-step scope, so nothing else catches this.
func TestRetryHelperIsInScope(t *testing.T) {
	for _, s := range runSteps(t) {
		call := firstRealMatch(retryCall, s.run)
		if call < 0 {
			continue
		}
		// A definition inside a comment defines nothing, and one that appears
		// after the first call is not in scope at that call.
		def := firstRealMatch(retryDefine, s.run)
		if def == -1 || def > call {
			t.Errorf("%s: job %q step %q calls `retry` with no definition in scope before the call; "+
				"each run block is its own shell, so a helper from an earlier step does not carry over",
				s.workflow, s.job, s.name)
		}
	}
}

// retryDefine matches a real function definition, not a mention of one.
var retryDefine = regexp.MustCompile(`(?m)^\s*retry\s*\(\)\s*\{`)

// cosignCall matches a cosign invocation, capturing whether it went through the
// retry helper and which subcommand it runs.
var cosignCall = regexp.MustCompile(`(?m)` + commandPosition + `\s*(retry\s+)?cosign\s+(\S+)`)

// cosignLocal are the subcommands that reach no network, so a retry would buy
// nothing. Everything else -- including an indirect `cosign "$@"` -- talks to
// Rekor or the registry and must be retried.
var cosignLocal = map[string]bool{
	"version": true, "--version": true, "help": true, "--help": true,
	"generate": true, "public-key": true, "env": true,
}

// TestCosignCallsAreRetried pins the coverage half of the defect class
// TestRetryHelperIsInScope pins the scope half of. A helper that is in scope
// still protects nothing if a call is written without it.
//
// Every cosign call reaches Rekor and GHCR, so a bare one fails on a single
// 502. Inside the release gate that is worse than a wasted run: the calls after
// `gh release edit --draft=false` execute with the release already public, so a
// transient failure fires the retraction handler and pulls back a release that
// passed every signature, SBOM, digest and provenance check -- reporting a
// network blip as though the artifacts had been tampered with. That is how a
// gate earns a reputation for crying wolf and starts getting overridden.
//
// The gate shipped five unretried calls: the provenance predicate, both chart
// checks, and both public-channel checks.
func TestCosignCallsAreRetried(t *testing.T) {
	for _, s := range runSteps(t) {
		for _, m := range cosignCall.FindAllStringSubmatchIndex(s.run, -1) {
			// m[2] is the start of the optional `retry ` group; -1 means the
			// call is bare. m[4]:m[5] is the subcommand.
			if m[2] >= 0 || inComment(s.run, m[0]) || cosignLocal[s.run[m[4]:m[5]]] {
				continue
			}
			t.Errorf("%s: job %q step %q invokes cosign without the retry helper: %q. "+
				"cosign talks to Rekor and GHCR on every call, so an unretried one reports a "+
				"transient outage as a verification failure",
				s.workflow, s.job, s.name, snippet(s.run, m[0]))
		}
	}
}

// snippet returns a one-line excerpt starting at offset, for error messages.
func snippet(body string, offset int) string {
	end := strings.IndexByte(body[offset:], '\n')
	if end < 0 {
		end = len(body) - offset
	}
	return strings.TrimSpace(body[offset : offset+end])
}

// inComment reports whether the offset falls on a line whose first
// non-whitespace character is `#`.
func inComment(body string, offset int) bool {
	start := strings.LastIndexByte(body[:offset], '\n') + 1
	return strings.HasPrefix(strings.TrimSpace(body[start:offset]), "#")
}

// varRef matches ${NAME}; bareVarRef matches the unbraced $NAME form, which
// aborts under nounset exactly the same way.
var varRef = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)
var bareVarRef = regexp.MustCompile(`\$([A-Z][A-Z0-9_]*)`)

// nounset matches the spellings that turn an unset name into an abort:
// `set -u`, `set -eu`, `set -euo pipefail`, `set -o nounset`.
var nounset = regexp.MustCompile(`set\s+(-[a-zA-Z]*u|-o\s+nounset)`)

// assigned matches the forms that bring a name into scope within one script.
var assigned = regexp.MustCompile(`(?m)^\s*(?:export\s+|local\s+|declare\s+)?([A-Z][A-Z0-9_]*)=`)

// forLoopVar matches `for NAME in ...`, which also binds a name.
var forLoopVar = regexp.MustCompile(`(?m)\bfor\s+([A-Z][A-Z0-9_]*)\s+in\b`)

// runnerProvided are always in scope: shell built-ins the interpreter maintains,
// plus the variables the Actions runner exports into every step.
var runnerProvided = map[string]bool{
	// Shell built-ins.
	"PWD": true, "OLDPWD": true, "IFS": true, "UID": true, "EUID": true,
	"PPID": true, "SHLVL": true, "RANDOM": true, "SECONDS": true, "HOSTNAME": true,
	"BASH_VERSION": true, "LINENO": true, "FUNCNAME": true, "OSTYPE": true,
	// Runner-provided.
	"HOME": true, "PATH": true, "CI": true, "RUNNER_OS": true, "RUNNER_ARCH": true,
	"RUNNER_TEMP": true, "RUNNER_DEBUG": true, "TMPDIR": true, "LD_LIBRARY_PATH": true,
}

// TestNoUnboundVariablesInRunBlocks pins the other half of the same defect
// class. Every release-path script runs under `set -u`, so a name that reaches
// the shell without an `env:` entry is not an empty string — it aborts the step.
//
// The draft-state guard shipped exactly this way: it read `${TAG}` with only
// `GH_TOKEN` in its `env`, so it would have failed the job immediately after
// creating the release, skipping the gate entirely.
func TestNoUnboundVariablesInRunBlocks(t *testing.T) {
	for _, s := range runSteps(t) {
		if !nounset.MatchString(s.run) {
			continue
		}

		inScope := map[string]bool{}
		for _, m := range assigned.FindAllStringSubmatch(s.run, -1) {
			inScope[m[1]] = true
		}
		for _, m := range forLoopVar.FindAllStringSubmatch(s.run, -1) {
			inScope[m[1]] = true
		}

		refs := varRef.FindAllStringSubmatch(s.run, -1)
		refs = append(refs, bareVarRef.FindAllStringSubmatch(s.run, -1)...)
		for _, m := range refs {
			name := m[1]
			switch {
			case s.env[name], inScope[name], runnerProvided[name]:
				continue
			case strings.HasPrefix(name, "GITHUB_"):
				continue
			}
			t.Errorf("%s: job %q step %q reads variable %s under `set -u`, but nothing puts it in scope; "+
				"add it to the step's env: or assign it in the script, or the step aborts rather than "+
				"reading an empty value", s.workflow, s.job, s.name, name)
		}
	}
}

// TestFailureHandlersRunLast catches a cleanup step that cannot see the failure
// it exists to clean up after.
//
// Steps run in declaration order, and `if: failure()` is evaluated in that
// order too. A retract-on-failure step placed before the last verification step
// has already been evaluated -- and skipped, because nothing had failed yet --
// by the time that verification fails. It never fires, and the release stays
// published with the run red: the exact state the draft exists to prevent.
//
// A failure handler is therefore only meaningful if every step after it is
// itself conditional; an unconditional step below it is a failure it cannot
// catch.
func TestFailureHandlersRunLast(t *testing.T) {
	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		if !onReleasePath(base) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		var doc struct {
			Jobs map[string]struct {
				Steps []struct {
					Name string `json:"name"`
					If   string `json:"if"`
				} `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for jobName, job := range doc.Jobs {
			for i, s := range job.Steps {
				if !strings.Contains(s.If, "failure()") {
					continue
				}
				// Any later step at all is a problem, not only an unconditional
				// one: `if: always()` runs after a prior failure, so a later
				// always() step that fails is equally beyond this handler's
				// reach. The only safe position is last.
				for _, later := range job.Steps[i+1:] {
					t.Errorf("%s: job %q step %q handles failure() but step %q runs after it; "+
						"if:failure() is evaluated in declaration order, so a failure below this "+
						"step comes too late for it to fire. A failure handler must be last.",
						base, jobName, s.Name, later.Name)
				}
			}
		}
	}
}
