// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
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

// needsOutputRef matches a GitHub Actions expression that reads a job output.
// Those outputs are typically pass-throughs of workflow_call inputs (see
// validate.outputs.subject_kind), so folding one into an origin field is the
// same laundering as referencing inputs.* directly.
var needsOutputRef = regexp.MustCompile(`needs\.[A-Za-z0-9_-]+\.outputs\.[A-Za-z0-9_-]+`)

// TestAttestIsInvokedAsReusableWorkflow pins the half of ADR-074 D2 that
// TestAttestIsSoleSigner (workflow_policy_test.go) does not cover: every
// release-path caller must reach attest.yml through `uses: ./…`. The unique
// gap this test closes is a caller silently dropping its attest.yml call so
// nothing gets signed at all (TestAttestIsSoleSigner already rejects copying
// cosign sign/attest into a non-attest workflow).
func TestAttestIsInvokedAsReusableWorkflow(t *testing.T) {
	assertAttestIsInvokedAsReusableWorkflow(t)
}

func assertAttestIsInvokedAsReusableWorkflow(t *testing.T) {
	t.Helper()

	// Must-call list: release-path workflows that are required to invoke
	// attest.yml. Form-checking sweeps every workflow (including future
	// callers outside this list) via isAttestWorkflowCall below.
	mustCall := []string{wfRelease, wfPublish, wfAttestSmoke}
	called := map[string]bool{}

	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		raw, err := os.ReadFile(path)
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
			uses := strings.TrimSpace(job.Uses)
			if !isAttestWorkflowCall(uses) {
				continue
			}
			called[base] = true
			// Assert the ./ form on EVERY attest.yml call, not only those that
			// already match localAttestUses. An org-qualified sha-pinned call
			// would otherwise keep the must-call count green while attesting
			// under a stale pre-hardening attest.yml.
			if !localAttestUses.MatchString(uses) || !strings.HasPrefix(uses, "./") {
				t.Errorf("%s: job %q calls attest.yml as %q; same-repo reusable "+
					"calls must use the ./ form so the call is a workflow_call boundary",
					base, jobName, uses)
			}
		}
	}

	for _, base := range mustCall {
		if !called[base] {
			t.Errorf("%s does not call ./.github/workflows/attest.yml; "+
				"attestation must stay behind a reusable-workflow boundary", base)
		}
	}
}

// TestAttestPredicateUsesOnlyTrustedContext pins the half of ADR-074 D2 that
// makes provenance origin fields unforgeable by the build process: repository,
// ref, sha, server, run id, and builder.id are minted from GITHUB_* /
// github.workflow_ref inside attest.yml, never from a caller-supplied
// inputs.* value (including one laundered through needs.*.outputs.*).
//
// subjectKind is intentionally out of scope here: it is a caller-chosen,
// enum-validated inputs.subject_kind pass-through and shapes the predicate
// without claiming origin. Origin fields are executed below so a second jq
// writer or a reformatted --arg line cannot slip past a first-occurrence scan.
func TestAttestPredicateUsesOnlyTrustedContext(t *testing.T) {
	step := provenanceStep(t)

	// Reject inputs.* / needs.*.outputs.* on every env level the step can see,
	// and inside the run block, for anything that could reach an origin field.
	// SUBJECT_KIND may legitimately expand needs.validate.outputs.subject_kind.
	for envName, envVal := range step.MergedEnv {
		if envName == "SUBJECT_KIND" {
			continue
		}
		if strings.Contains(envVal, "inputs.") {
			t.Errorf("provenance step env %q expands %q; origin fields must not "+
				"be sourced from workflow_call inputs", envName, envVal)
		}
		if needsOutputRef.MatchString(envVal) {
			t.Errorf("provenance step env %q expands %q; origin fields must not "+
				"be laundered through needs.*.outputs.* (caller inputs)", envName, envVal)
		}
	}
	if strings.Contains(step.Run, "inputs.") {
		t.Errorf("provenance step run block references inputs.*; origin fields " +
			"must be built from trusted context only")
	}

	callerRef, ok := step.MergedEnv["CALLER_WORKFLOW_REF"]
	if !ok {
		t.Fatal("provenance step is missing CALLER_WORKFLOW_REF; builder.id must " +
			"be derived from github.workflow_ref")
	}
	if !strings.Contains(callerRef, "github.workflow_ref") {
		t.Errorf("CALLER_WORKFLOW_REF is %q; it must expand github.workflow_ref "+
			"so a caller cannot name an arbitrary builder", callerRef)
	}

	// Execute the step and assert the resulting provenance.json origin fields.
	// Grepping --arg lines misses a later writer and false-fails on formatting.
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "provenance.sh")
	if err := os.WriteFile(scriptPath, []byte(step.Run), 0o600); err != nil {
		t.Fatalf("write provenance script: %v", err)
	}

	const (
		wantRepo   = "NVIDIA/cluster-readiness-engine"
		wantRef    = "refs/tags/v9.9.9"
		wantSHA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		wantServer = "https://github.com"
		wantRunID  = "424242"
		wantKind   = "image"
	)
	callerWorkflow := wantRepo + "/.github/workflows/release.yml@" + wantRef
	env := append(os.Environ(),
		"CALLER_WORKFLOW_REF="+callerWorkflow,
		"GITHUB_REPOSITORY="+wantRepo,
		"GITHUB_REF="+wantRef,
		"GITHUB_SHA="+wantSHA,
		"GITHUB_SERVER_URL="+wantServer,
		"GITHUB_RUN_ID="+wantRunID,
		"SUBJECT_KIND="+wantKind,
	)
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("provenance step rejected a legitimate caller:\n%s", out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "provenance.json"))
	if err != nil {
		t.Fatalf("read provenance.json: %v", err)
	}
	var pred struct {
		BuildDefinition struct {
			ExternalParameters struct {
				Repository  string `json:"repository"`
				Ref         string `json:"ref"`
				SubjectKind string `json:"subjectKind"`
			} `json:"externalParameters"`
			ResolvedDependencies []struct {
				Digest struct {
					GitCommit string `json:"gitCommit"`
				} `json:"digest"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
			Metadata struct {
				InvocationID string `json:"invocationId"`
			} `json:"metadata"`
		} `json:"runDetails"`
	}
	if err := json.Unmarshal(raw, &pred); err != nil {
		t.Fatalf("parse provenance.json: %v\n%s", err, raw)
	}
	ep := pred.BuildDefinition.ExternalParameters
	if ep.Repository != wantRepo {
		t.Errorf("externalParameters.repository = %q, want %q (trusted GITHUB_REPOSITORY)",
			ep.Repository, wantRepo)
	}
	if ep.Ref != wantRef {
		t.Errorf("externalParameters.ref = %q, want %q (trusted GITHUB_REF)", ep.Ref, wantRef)
	}
	if ep.SubjectKind != wantKind {
		t.Errorf("externalParameters.subjectKind = %q, want %q", ep.SubjectKind, wantKind)
	}
	if len(pred.BuildDefinition.ResolvedDependencies) == 0 ||
		pred.BuildDefinition.ResolvedDependencies[0].Digest.GitCommit != wantSHA {
		t.Errorf("resolvedDependencies gitCommit missing or wrong; want %q", wantSHA)
	}
	wantBuilder := wantServer + "/" + wantRepo + "/.github/workflows/release.yml"
	if pred.RunDetails.Builder.ID != wantBuilder {
		t.Errorf("runDetails.builder.id = %q, want %q", pred.RunDetails.Builder.ID, wantBuilder)
	}
	wantInvocation := wantServer + "/" + wantRepo + "/actions/runs/" + wantRunID
	if pred.RunDetails.Metadata.InvocationID != wantInvocation {
		t.Errorf("runDetails.metadata.invocationId = %q, want %q",
			pred.RunDetails.Metadata.InvocationID, wantInvocation)
	}
}

// TestAttestBuilderIdGuardRejectsAttestorAsBuilder pins the guard that keeps
// runDetails.builder.id honest by extracting and executing it — the same
// convention attest_guards_test.go uses for the validate step. Grepping for
// the needle would stay green if exit 1 were deleted or the comparison flipped.
func TestAttestBuilderIdGuardRejectsAttestorAsBuilder(t *testing.T) {
	step := provenanceStep(t)
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "provenance.sh")
	if err := os.WriteFile(scriptPath, []byte(step.Run), 0o600); err != nil {
		t.Fatalf("write provenance script: %v", err)
	}

	baseEnv := func(callerRef string) []string {
		return append(os.Environ(),
			"CALLER_WORKFLOW_REF="+callerRef,
			"GITHUB_REPOSITORY=NVIDIA/cluster-readiness-engine",
			"GITHUB_REF=refs/tags/v1.2.3",
			"GITHUB_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"GITHUB_SERVER_URL=https://github.com",
			"GITHUB_RUN_ID=1",
			"SUBJECT_KIND=image",
		)
	}
	run := func(t *testing.T, callerRef string) (bool, string) {
		t.Helper()
		cmd := exec.Command("bash", scriptPath)
		cmd.Dir = t.TempDir()
		cmd.Env = baseEnv(callerRef)
		out, err := cmd.CombinedOutput()
		return err == nil, string(out)
	}

	t.Run("accepts caller workflow as builder", func(t *testing.T) {
		ok, out := run(t, "NVIDIA/cluster-readiness-engine/.github/workflows/release.yml@refs/tags/v1.2.3")
		if !ok {
			t.Fatalf("guard rejected a legitimate caller:\n%s", out)
		}
	})

	t.Run("rejects attest.yml as builder", func(t *testing.T) {
		ok, out := run(t, "NVIDIA/cluster-readiness-engine/.github/workflows/attest.yml@refs/tags/v1.2.3")
		if ok {
			t.Fatal("guard accepted attest.yml as builder_id; attestor cannot be the builder")
		}
		if !strings.Contains(out, "attestor cannot be the builder") &&
			!strings.Contains(out, "builder resolved to attest.yml") {
			t.Errorf("rejected, but not by the attestor-as-builder guard:\n%s", out)
		}
	})

	t.Run("fail-closed on malformed workflow_ref", func(t *testing.T) {
		ok, out := run(t, "NVIDIA/cluster-readiness-engine/.github/workflows/release.yml")
		if ok {
			t.Fatal("guard accepted a workflow_ref without @ref; must fail closed")
		}
		if !strings.Contains(out, "unexpected GITHUB_WORKFLOW_REF") {
			t.Errorf("rejected, but not by the fail-closed case guard:\n%s", out)
		}
	})
}

type namedEnvStep struct {
	Name      string
	Run       string
	MergedEnv map[string]string // workflow + job + step, later wins
}

func provenanceStep(t *testing.T) namedEnvStep {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(workflowDir, attestWorkflowName))
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflowName, err)
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
		t.Fatalf("parse %s: %v", attestWorkflowName, err)
	}

	const wantName = "Generate SLSA provenance predicate"
	for jobName, job := range doc.Jobs {
		for _, step := range job.Steps {
			if step.Name != wantName {
				continue
			}
			if strings.TrimSpace(step.Run) == "" {
				t.Fatalf("%s job %q step %q has an empty run block",
					attestWorkflowName, jobName, wantName)
			}
			merged := map[string]string{}
			for _, m := range []map[string]string{doc.Env, job.Env, step.Env} {
				maps.Copy(merged, m)
			}
			return namedEnvStep{
				Name:      step.Name,
				Run:       step.Run,
				MergedEnv: merged,
			}
		}
	}
	t.Fatalf("%s has no step named %q; the provenance predicate must be minted "+
		"inside the reusable workflow", attestWorkflowName, wantName)
	return namedEnvStep{}
}
