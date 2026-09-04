// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// cosignSignCmds are the signing invocations that may exist only inside
// attest.yml. A second home for any of them widens the published identity
// contract: consumers pin one workflow path, so a signer elsewhere is accepted
// under a different SAN with no change to the pin.
//
// Also covers the "not inlined as a job" half of ADR-074 D2: moving these
// invocations into release.yml / publish.yml as ordinary job steps would still
// compile and still green every other test, but Fulcio would stop naming
// attest.yml and identity-pinned verification would quietly fail for consumers.
var cosignSignCmds = regexp.MustCompile(`(?m)(?:^|[\s;|&])(?:retry\s+)?cosign\s+(sign|attest|attest-blob)\b`)

var attestProvenanceAction = regexp.MustCompile(`(^|/)actions/attest-build-provenance(@|$)`)

// Local attest.yml call shape used by publish.yml / release.yml / selftest.
var localAttestUses = regexp.MustCompile(`^\./\.github/workflows/attest\.yml(@.+)?$`)

const attestWorkflowName = "attest.yml"

// TestAttestIsSoleSigner keeps the published certificate identity contract true.
//
// Consumers pin
//
//	.../attest.yml@refs/tags/<TAG>
//
// so any second workflow that invokes cosign sign/attest/attest-blob (or
// actions/attest-build-provenance) silently widens what that pin accepts.
// attest.yml itself must stay workflow_call-only: any other trigger makes the
// signing identity reachable from a branch push or a dispatch, and inlining
// its steps into a caller would demote the reusable-workflow boundary while
// every other test stayed green.
func TestAttestIsSoleSigner(t *testing.T) {
	assertAttestIsWorkflowCallOnly(t)
	assertAttestIsInvokedAsReusableWorkflow(t)

	paths := append([]string{}, workflowFiles(t)...)
	paths = append(paths, compositeActionFiles(t)...)

	for _, path := range paths {
		base := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if isCompositeActionPath(path) {
			var doc struct {
				Runs struct {
					Using string       `json:"using"`
					Steps []policyStep `json:"steps"`
				} `json:"runs"`
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			if doc.Runs.Using != "" && doc.Runs.Using != "composite" {
				continue
			}
			assertStepsForbidForeignSigners(t, relGithub(path), false /* allowCosign */, doc.Runs.Steps)
			continue
		}

		var doc struct {
			Jobs map[string]struct {
				Steps []policyStep `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for jobName, job := range doc.Jobs {
			where := fmt.Sprintf("%s: job %q", base, jobName)
			assertStepsForbidForeignSigners(t, where, base == attestWorkflowName, job.Steps)
		}
	}
}

// policyStep is the subset of a workflow/composite step the signer checks
// reason about.
type policyStep struct {
	Name string `json:"name"`
	Run  string `json:"run"`
	Uses string `json:"uses"`
}

func assertStepsForbidForeignSigners(t *testing.T, where string, allowCosign bool, steps []policyStep) {
	t.Helper()

	for _, step := range steps {
		if attestProvenanceAction.MatchString(step.Uses) {
			t.Errorf("%s step %q uses %s; provenance must be emitted by attest.yml "+
				"via cosign, not actions/attest-build-provenance",
				where, step.Name, step.Uses)
		}
		if allowCosign {
			continue
		}
		if m := cosignSignCmds.FindStringSubmatch(step.Run); m != nil {
			t.Errorf("%s step %q invokes `cosign %s`; attest.yml must be the sole signer",
				where, step.Name, m[1])
		}
	}
}

// compositeActionFiles returns every local composite action.yml.
func compositeActionFiles(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob("../../.github/actions/*/action.yml")
	if err != nil {
		t.Fatalf("glob composite actions: %v", err)
	}
	return paths
}

func isCompositeActionPath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/.github/actions/")
}

func relGithub(path string) string {
	slash := filepath.ToSlash(path)
	if i := strings.Index(slash, ".github/"); i >= 0 {
		return slash[i:]
	}
	return filepath.Base(path)
}

func assertAttestIsWorkflowCallOnly(t *testing.T) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowDir, attestWorkflowName))
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflowName, err)
	}

	triggers := workflowTriggers(raw, t)
	if len(triggers) == 0 {
		t.Fatalf("%s declares no triggers; it must be workflow_call-only", attestWorkflowName)
	}
	for name := range triggers {
		if name != "workflow_call" {
			t.Errorf("%s is triggered by %q; only workflow_call is allowed so the signing "+
				"identity cannot be reached from a branch or dispatch",
				attestWorkflowName, name)
		}
	}
	if _, ok := triggers["workflow_call"]; !ok {
		t.Errorf("%s is missing workflow_call", attestWorkflowName)
	}
}

// assertAttestIsInvokedAsReusableWorkflow checks that every release-path caller
// reaches attest.yml through `uses: ./…`, not by inlining its jobs. Without
// this, a refactor could copy the attest steps into release.yml, keep
// workflow_call on an unused attest.yml, and demote the Fulcio identity while
// TestAttestIsSoleSigner still saw cosign only inside attest.yml — until the
// copy started signing too.
func assertAttestIsInvokedAsReusableWorkflow(t *testing.T) {
	t.Helper()

	callers := []string{wfRelease, wfPublish, wfAttestSmoke}
	found := 0
	for _, base := range callers {
		raw, err := os.ReadFile(filepath.Join(workflowDir, base))
		if err != nil {
			t.Fatalf("read %s: %v", base, err)
		}
		var doc struct {
			Jobs map[string]struct {
				Uses string `json:"uses"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", base, err)
		}
		for jobName, job := range doc.Jobs {
			if !localAttestUses.MatchString(strings.TrimSpace(job.Uses)) {
				continue
			}
			found++
			if !strings.HasPrefix(job.Uses, "./") {
				t.Errorf("%s: job %q calls attest.yml as %q; same-repo reusable "+
					"calls must use the ./ form so the call is a workflow_call boundary",
					base, jobName, job.Uses)
			}
		}
	}
	if found == 0 {
		t.Fatalf("no release-path workflow calls ./.github/workflows/attest.yml; " +
			"attestation must stay behind a reusable-workflow boundary")
	}
}

// workflowTriggers returns the `on:` block, tolerating YAML 1.1 turning a bare
// `on:` key into the boolean true (the same hazard workflowCallOutputs faces).
func workflowTriggers(raw []byte, t *testing.T) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse workflow triggers: %v", err)
	}
	for _, key := range []string{"on", boolTrue} {
		switch v := doc[key].(type) {
		case map[string]any:
			return v
		case string:
			return map[string]any{v: nil}
		case []any:
			out := map[string]any{}
			for _, item := range v {
				if s, ok := item.(string); ok {
					out[s] = nil
				}
			}
			return out
		}
	}
	return nil
}

// TestAttestPredicateUsesOnlyTrustedContext pins the half of ADR-074 D2 that
// makes provenance unforgeable by the build process: the predicate's origin
// fields are minted from GITHUB_* / github.workflow_ref inside attest.yml, never
// from a caller-supplied inputs.* value.
//
// A refactor that wired --arg repo ${{ inputs.repository }} (or passed the same
// through env) would still produce a green release whose predicate said whatever
// the caller asked. This test fails that change before it ships.
func TestAttestPredicateUsesOnlyTrustedContext(t *testing.T) {
	step := provenanceStep(t)

	// The step must not pull workflow_call inputs into the provenance surface.
	for envName, envVal := range step.Env {
		if strings.Contains(envVal, "inputs.") {
			t.Errorf("provenance step env %q expands %q; origin fields must not "+
				"be sourced from workflow_call inputs", envName, envVal)
		}
	}
	if strings.Contains(step.Run, "inputs.") {
		t.Errorf("provenance step run block references inputs.*; the predicate " +
			"must be built from trusted context only")
	}

	// CALLER_WORKFLOW_REF (builder.id) must come from github.workflow_ref, which
	// GitHub sets to the workflow that started the run — the caller — not from
	// an input a caller could forge.
	callerRef, ok := step.Env["CALLER_WORKFLOW_REF"]
	if !ok {
		t.Fatal("provenance step is missing CALLER_WORKFLOW_REF; builder.id must " +
			"be derived from github.workflow_ref")
	}
	if !strings.Contains(callerRef, "github.workflow_ref") {
		t.Errorf("CALLER_WORKFLOW_REF is %q; it must expand github.workflow_ref "+
			"so a caller cannot name an arbitrary builder", callerRef)
	}

	// arg -> required substring in the --arg value. builder is special: it is
	// derived from CALLER_WORKFLOW_REF / GITHUB_SERVER_URL via builder_id.
	wantByArg := map[string]string{
		"repo":    "GITHUB_REPOSITORY",
		"ref":     "GITHUB_REF",
		"sha":     "GITHUB_SHA",
		"server":  "GITHUB_SERVER_URL",
		"run_id":  "GITHUB_RUN_ID",
		"builder": "builder_id",
	}
	for arg, want := range wantByArg {
		flag := "--arg " + arg + " "
		idx := strings.Index(step.Run, flag)
		if idx < 0 {
			t.Errorf("provenance jq is missing --arg %s; the predicate must still "+
				"emit that origin field", arg)
			continue
		}
		rest := step.Run[idx+len(flag):]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[:nl]
		}
		rest = strings.TrimSpace(rest)
		if !strings.Contains(rest, want) {
			t.Errorf("--arg %s value %q must expand %s (trusted context), "+
				"not a caller-supplied input", arg, rest, want)
		}
	}
}

// TestAttestBuilderIdGuardRejectsAttestorAsBuilder pins the guard that keeps
// runDetails.builder.id honest. Naming attest.yml as the builder would make the
// predicate false on its face and is exactly the trade ADR-074 D2 refuses when
// it keeps L2 rather than inflating to L3.
func TestAttestBuilderIdGuardRejectsAttestorAsBuilder(t *testing.T) {
	step := provenanceStep(t)

	const needle = `"/.github/workflows/attest.yml"`
	if !strings.Contains(step.Run, needle) && !strings.Contains(step.Run, "'/.github/workflows/attest.yml'") {
		// Accept either quoting style used by the shell guard.
		if !strings.Contains(step.Run, "/.github/workflows/attest.yml") {
			t.Fatal("provenance step is missing the builder_id == attest.yml guard")
		}
	}
	if !strings.Contains(step.Run, "builder_id") {
		t.Fatal("provenance step does not compute builder_id")
	}
	if !strings.Contains(step.Run, "attestor cannot be the builder") &&
		!strings.Contains(step.Run, "builder resolved to attest.yml") {
		t.Error("provenance step must refuse when builder_id resolves to attest.yml " +
			"(message should name the attestor-as-builder failure)")
	}
}

type namedEnvStep struct {
	Name string
	Run  string
	Env  map[string]string
}

func provenanceStep(t *testing.T) namedEnvStep {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowDir, attestWorkflowName))
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflowName, err)
	}

	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `json:"name"`
				Run  string            `json:"run"`
				Env  map[string]string `json:"env"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", attestWorkflowName, err)
	}

	const wantName = "Generate SLSA provenance predicate"
	for jobName, job := range doc.Jobs {
		for _, step := range job.Steps {
			if step.Name == wantName {
				if strings.TrimSpace(step.Run) == "" {
					t.Fatalf("%s job %q step %q has an empty run block",
						attestWorkflowName, jobName, wantName)
				}
				return namedEnvStep{Name: step.Name, Run: step.Run, Env: step.Env}
			}
		}
	}
	t.Fatalf("%s has no step named %q; the provenance predicate must be minted "+
		"inside the reusable workflow", attestWorkflowName, wantName)
	return namedEnvStep{}
}
