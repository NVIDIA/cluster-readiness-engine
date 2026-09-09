// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Local attest.yml call shape used by publish.yml / release.yml / selftest.
// Complements isAttestWorkflowCall in workflow_policy_test.go: that helper
// accepts any path whose base is attest.yml; this one insists on the ./ form
// that keeps the call a same-repo workflow_call boundary.
var localAttestUses = regexp.MustCompile(`^\./\.github/workflows/attest\.yml(@.+)?$`)

// TestAttestIsInvokedAsReusableWorkflow pins the half of ADR-074 D2 that
// TestAttestIsSoleSigner (workflow_policy_test.go) does not cover: every
// release-path caller must reach attest.yml through `uses: ./…`, not by
// inlining its jobs. Without this, a refactor could copy the attest steps into
// release.yml, keep workflow_call on an unused attest.yml, and demote the
// Fulcio identity while TestAttestIsSoleSigner still saw cosign only inside
// attest.yml — until the copy started signing too.
func TestAttestIsInvokedAsReusableWorkflow(t *testing.T) {
	assertAttestIsInvokedAsReusableWorkflow(t)
}

func assertAttestIsInvokedAsReusableWorkflow(t *testing.T) {
	t.Helper()

	callers := []string{wfRelease, wfPublish, wfAttestSmoke}
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
		foundInWorkflow := 0
		for jobName, job := range doc.Jobs {
			if !localAttestUses.MatchString(strings.TrimSpace(job.Uses)) {
				continue
			}
			foundInWorkflow++
			if !strings.HasPrefix(job.Uses, "./") {
				t.Errorf("%s: job %q calls attest.yml as %q; same-repo reusable "+
					"calls must use the ./ form so the call is a workflow_call boundary",
					base, jobName, job.Uses)
			}
		}
		if foundInWorkflow == 0 {
			t.Errorf("%s does not call ./.github/workflows/attest.yml; "+
				"attestation must stay behind a reusable-workflow boundary", base)
		}
	}
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
