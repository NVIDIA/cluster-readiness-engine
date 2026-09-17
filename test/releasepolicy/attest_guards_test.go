// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package releasepolicy tests invariants of the release-path workflows.
//
// attest.yml holds the release signing identity, and its input validation is
// the gate that stands between a caller and that identity. The validation is
// shell embedded in YAML, which nothing else in the repository type-checks or
// exercises: a weakened regex or a dropped guard would not break any build, it
// would just stop rejecting things. These tests execute that shell directly so
// that a regression fails `make test` rather than shipping.
package releasepolicy

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const attestWorkflow = "../../.github/workflows/attest.yml"

// validDigest is a well-formed sha256 digest: the shape every guard below is
// measured against, so a case fails for the reason it names and not because
// the digest happened to be malformed too.
const validDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

const managerImage = "ghcr.io/nvidia/cluster-readiness-engine/manager"

// The environment names the validate step reads. They are the workflow_call
// input surface, so the whole set is named here rather than spelled out in
// every case table.
const (
	inSubjectKind    = "IN_SUBJECT_KIND"
	inSubjectName    = "IN_SUBJECT_NAME"
	inSubjectTag     = "IN_SUBJECT_TAG"
	inPlatform       = "IN_PLATFORM"
	inExpectedDigest = "IN_EXPECTED_DIGEST"
	inArtifactName   = "IN_ARTIFACT_NAME"
	inPredicateName  = "IN_PREDICATE_NAME"
	inPredicateType  = "IN_PREDICATE_TYPE"
	inCosignVersion  = "IN_COSIGN_VERSION"
	inCraneVersion   = "IN_CRANE_VERSION"
	inAllowUntagged  = "IN_ALLOW_UNTAGGED"
	inEmitProvenance = "IN_EMIT_PROVENANCE"
	callerRef        = "CALLER_REF"
)

// Fixture values for the blob subject, which most of the artifact and
// predicate cases build on.
const (
	kindBlob               = "blob"
	blobSubject            = "nvcrectl-linux-amd64"
	blobArtifact           = "cli-binaries"
	predicateTypeCycloneDX = "cyclonedx"
)

// The workflow's boolean inputs reach the validate step as strings, and
// platformAMD64 is the os/arch pair the per-platform cases use.
const (
	boolTrue      = "true"
	boolFalse     = "false"
	platformAMD64 = "linux/amd64"
)

// Rejection messages asserted by more than one case.
const (
	errDigestMustMatch = "expected_digest must match"
	errInvalidTag      = "subject_tag is not a valid OCI tag"
)

// workflow is the subset of the workflow schema these tests read.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			ID  string `json:"id"`
			Run string `json:"run"`
		} `json:"steps"`
	} `json:"jobs"`
}

// validateScript returns the body of attest.yml's input-validation step.
//
// Extracting it from the workflow rather than keeping a copy here is the point:
// a copy would drift, and a drifted copy would keep passing while the real
// validation rotted.
func validateScript(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(attestWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflow, err)
	}

	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", attestWorkflow, err)
	}

	job, ok := wf.Jobs["validate"]
	if !ok {
		t.Fatalf("%s has no `validate` job; input validation must not be removed", attestWorkflow)
	}
	for _, step := range job.Steps {
		if step.ID == "check" {
			return step.Run
		}
	}
	t.Fatalf("%s `validate` job has no step with id `check`", attestWorkflow)
	return ""
}

// inputs are the workflow_call inputs as the validate step sees them, plus the
// caller ref. Defaults mirror a well-formed tagged image call so each test case
// changes only the field it is about.
type inputs map[string]string

func defaultInputs() inputs {
	return inputs{
		inSubjectKind:    "image",
		inSubjectName:    managerImage,
		inSubjectTag:     "v1.2.3",
		inPlatform:       "",
		inExpectedDigest: validDigest,
		inArtifactName:   "",
		inPredicateName:  "",
		inPredicateType:  "",
		inCosignVersion:  "v3.1.3",
		inCraneVersion:   "v0.20.6",
		inAllowUntagged:  boolFalse,
		inEmitProvenance: boolTrue,
		callerRef:        "refs/tags/v1.2.3",
	}
}

func (in inputs) with(overrides inputs) inputs {
	out := inputs{}
	maps.Copy(out, in)
	maps.Copy(out, overrides)
	return out
}

// runValidate executes the validation body and reports its exit status and
// combined output.
//
// `bash` is resolved through PATH rather than pinned, so this runs against
// whatever the developer or runner provides. The extracted script sticks to
// constructs available since bash 2.0 -- indirect expansion, ANSI-C quoting,
// and unquoted `[[ =~ ]]` patterns -- and the whole table has been confirmed to
// pass under both macOS's bash 3.2 and bash 5.3, so a machine with either
// exercises the same guards.
func runValidate(t *testing.T, script string, in inputs) (bool, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "validate.sh")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+filepath.Join(t.TempDir(), "out"))
	for k, v := range in {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// TestAttestValidationAccepts covers the shapes real callers will use. A guard
// that rejects everything is not a working guard.
func TestAttestValidationAccepts(t *testing.T) {
	script := validateScript(t)

	cases := map[string]inputs{
		"tagged image": {},
		"per-platform image": {
			inPlatform: platformAMD64,
		},
		"oci artifact (helm chart)": {
			inSubjectKind: "oci-artifact",
			inSubjectName: "ghcr.io/nvidia/cluster-readiness-engine",
		},
		"blob": {
			inSubjectKind:  kindBlob,
			inSubjectName:  blobSubject,
			inSubjectTag:   "",
			inArtifactName: blobArtifact,
		},
		"blob with sbom predicate": {
			inSubjectKind:   kindBlob,
			inSubjectName:   blobSubject,
			inSubjectTag:    "",
			inArtifactName:  blobArtifact,
			inPredicateName: "sbom.cyclonedx.json",
			inPredicateType: predicateTypeCycloneDX,
		},
		// Per-platform SBOM calls suppress provenance: ADR-074 puts provenance
		// on the index digest only, so a child manifest must not carry a
		// second, competing provenance statement.
		"image predicate with provenance suppressed": {
			inPlatform:       platformAMD64,
			inArtifactName:   "sboms",
			inPredicateName:  "sbom-linux-amd64.cyclonedx.json",
			inPredicateType:  predicateTypeCycloneDX,
			inEmitProvenance: boolFalse,
		},
		// The non-production escape hatch, which exists so the guards can be
		// exercised by dispatch at all. It must still work.
		"untagged ref with allow_untagged": {
			callerRef:       "refs/heads/main",
			inSubjectTag:    "main-abc1234",
			inAllowUntagged: boolTrue,
		},
	}

	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			ok, out := runValidate(t, script, defaultInputs().with(overrides))
			if !ok {
				t.Errorf("validation rejected a legitimate call:\n%s", out)
			}
		})
	}
}

// TestAttestValidationRejects is the substance. Each case is a way a caller
// could reach the signing identity with something it should not, and the
// wantErr string pins the rejection to the guard that is supposed to catch it
// — so a case cannot start passing for an unrelated reason.
func TestAttestValidationRejects(t *testing.T) {
	script := validateScript(t)

	cases := map[string]struct {
		overrides inputs
		wantErr   string
	}{
		// Release attestations come from tags. Without this, a branch build
		// signs under an identity users are told to trust.
		"non-tag ref without allow_untagged": {
			inputs{callerRef: "refs/heads/main"},
			"refuses to run on refs/heads/main",
		},
		// allow_untagged on a v* tag is contradictory: the caller claimed a
		// non-production run while standing on the release ref that mints the
		// identity SECURITY.md pins. Refuse rather than silently mint it.
		"allow_untagged on a release tag": {
			inputs{inAllowUntagged: boolTrue},
			"allow_untagged: true is contradictory on release ref",
		},
		"digest with non-hex characters": {
			inputs{inExpectedDigest: "sha256:zzzz"},
			errDigestMustMatch,
		},
		// Uppercase hex is a different string to cosign and to the registry, so
		// accepting it would let two spellings of one digest diverge.
		"digest with uppercase hex": {
			inputs{inExpectedDigest: "sha256:AAAA111111111111111111111111111111111111111111111111111111111111"},
			errDigestMustMatch,
		},
		"digest missing the sha256 prefix": {
			inputs{inExpectedDigest: strings.TrimPrefix(validDigest, "sha256:")},
			errDigestMustMatch,
		},
		// A newline lets a value forge extra lines in GITHUB_OUTPUT.
		"newline in subject_name": {
			inputs{inSubjectName: managerImage + "\nevil=1"},
			"newline or carriage return",
		},
		"carriage return in subject_tag": {
			inputs{inSubjectTag: "v1.2.3\rx"},
			"newline or carriage return",
		},
		"unknown subject_kind": {
			inputs{inSubjectKind: "sbom"},
			"subject_kind must be",
		},
		// Without a tag there is nothing to resolve the digest from except the
		// digest itself, which is the tautology this guard exists to prevent.
		"image without subject_tag": {
			inputs{inSubjectTag: ""},
			"subject_tag is required",
		},
		"subject_name carrying a digest": {
			inputs{inSubjectName: managerImage + "@" + validDigest},
			"without a tag or digest",
		},
		"subject_name carrying a tag": {
			inputs{inSubjectName: managerImage + ":v1.2.3"},
			"without a tag or digest",
		},
		// artifact_name and predicate_name each had a traversal case; this one
		// did not, and subject_name reaches `path="subject/${SUBJECT_NAME}"`
		// in the job that holds the signing token.
		// Suppressing provenance on a blob with no predicate would sign and
		// attest nothing at all, producing a green run with no artifact.
		"blob with provenance suppressed and no predicate": {
			inputs{
				inSubjectKind:    kindBlob,
				inSubjectName:    blobSubject,
				inSubjectTag:     "",
				inArtifactName:   blobArtifact,
				inEmitProvenance: boolFalse,
			},
			"leaves a blob call with nothing to attest",
		},
		"path traversal in blob subject_name": {
			inputs{
				inSubjectKind:  kindBlob,
				inSubjectName:  "../../etc/passwd",
				inSubjectTag:   "",
				inArtifactName: blobArtifact,
			},
			"must be a plain file name for a blob subject",
		},
		"blob subject_name with a path separator": {
			inputs{
				inSubjectKind:  kindBlob,
				inSubjectName:  "nested/nvcrectl-linux-amd64",
				inSubjectTag:   "",
				inArtifactName: blobArtifact,
			},
			"must be a plain file name for a blob subject",
		},
		// Distinct from "subject_tag is required": an empty tag trips the
		// emptiness guard and never reaches the format guard.
		"malformed subject_tag": {
			inputs{inSubjectTag: "v1/2.3"},
			errInvalidTag,
		},
		"subject_tag starting with a dash": {
			inputs{inSubjectTag: "-v1.2.3"},
			errInvalidTag,
		},
		"over-long subject_tag": {
			inputs{inSubjectTag: strings.Repeat("v", 129)},
			errInvalidTag,
		},
		// artifact_name is required whenever predicate_name is set, including
		// for image subjects where it is otherwise optional. Only the blob
		// requirement was covered.
		"image predicate without artifact_name": {
			inputs{
				inPredicateName: "sbom.cyclonedx.json",
				inPredicateType: predicateTypeCycloneDX,
			},
			"artifact_name is required when predicate_name is set",
		},
		"blob without artifact_name": {
			inputs{
				inSubjectKind: kindBlob,
				inSubjectName: blobSubject,
				inSubjectTag:  "",
			},
			"artifact_name is required when subject_kind is blob",
		},
		// An untyped predicate would be signed as cosign's `custom` default,
		// which no documented verification command asks for.
		"predicate without predicate_type": {
			inputs{
				inSubjectKind:   kindBlob,
				inSubjectName:   blobSubject,
				inSubjectTag:    "",
				inArtifactName:  blobArtifact,
				inPredicateName: "sbom.json",
			},
			"predicate_type is required",
		},
		"predicate_type outside the allowed set": {
			inputs{
				inSubjectKind:   kindBlob,
				inSubjectName:   blobSubject,
				inSubjectTag:    "",
				inArtifactName:  blobArtifact,
				inPredicateName: "sbom.json",
				inPredicateType: "custom",
			},
			"predicate_type must be one of",
		},
		"path traversal in predicate_name": {
			inputs{
				inSubjectKind:   kindBlob,
				inSubjectName:   blobSubject,
				inSubjectTag:    "",
				inArtifactName:  blobArtifact,
				inPredicateName: "../../etc/passwd",
				inPredicateType: predicateTypeCycloneDX,
			},
			"predicate_name must match",
		},
		"path traversal in artifact_name": {
			inputs{
				inSubjectKind:  kindBlob,
				inSubjectName:  blobSubject,
				inSubjectTag:   "",
				inArtifactName: "../secrets",
			},
			"artifact_name must match",
		},
		"platform on a blob subject": {
			inputs{
				inSubjectKind:  kindBlob,
				inSubjectName:  blobSubject,
				inSubjectTag:   "",
				inArtifactName: blobArtifact,
				inPlatform:     platformAMD64,
			},
			"platform is meaningless",
		},
		"platform without an os/arch pair": {
			inputs{inPlatform: "amd64"},
			"platform must be os/arch",
		},
		// The version pin is what fixes the emitted bundle format, so a
		// floating value would let the on-registry layout drift.
		"floating cosign version": {
			inputs{inCosignVersion: "latest"},
			"cosign_version must be",
		},
		"floating crane version": {
			inputs{inCraneVersion: "main"},
			"crane_version must be",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ok, out := runValidate(t, script, defaultInputs().with(tc.overrides))
			if ok {
				t.Fatalf("validation accepted a call it must reject; expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(out, tc.wantErr) {
				t.Errorf("rejected, but not by the expected guard.\nwant error containing: %q\ngot:\n%s", tc.wantErr, out)
			}
		})
	}
}

// TestAttestWorkflowIsGatedToThisRepository pins the guard that keeps outside
// repositories away from the release signing identity.
//
// `workflow_call` on a public repository is callable by anyone, and in a called
// reusable workflow `github.repository` is the *caller's* repository. Without
// this condition on every job, an outside repository could invoke attest.yml at
// one of our release tags and obtain a Fulcio certificate whose SAN is the
// identity SECURITY.md tells users to pin.
func TestAttestWorkflowIsGatedToThisRepository(t *testing.T) {
	raw, err := os.ReadFile(attestWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflow, err)
	}

	var wf struct {
		Jobs map[string]struct {
			If string `json:"if"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", attestWorkflow, err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("attest.yml declares no jobs")
	}

	const want = "github.repository == 'NVIDIA/cluster-readiness-engine'"
	for name, job := range wf.Jobs {
		if !strings.Contains(job.If, want) {
			t.Errorf("job %q is missing the caller gate %s; a public reusable workflow without it "+
				"lets any repository sign under this repository's release identity", name, want)
		}
	}
}

// Exact job-level `if:` expressions that keep workflow_dispatch off a v* tag
// from minting the release signing identity (#340). Shared by the pin test and
// the fail-closed recognizer so the allowlist cannot drift from what we assert.
const (
	exactRepoAndMainIf = "github.repository == 'NVIDIA/cluster-readiness-engine'" +
		" && github.ref == 'refs/heads/main'"
	exactAlwaysRepoAndMainIf = "always() && github.repository == 'NVIDIA/cluster-readiness-engine'" +
		" && github.ref == 'refs/heads/main'"
	// exactRepoOnlyIf is the job-level repository gate release.yml uses on
	// intermediates (build-cli, helm-publish, release-tag, ...). It is not a
	// ref guard, but it is the only non-empty if: we allowlist for needs-walk
	// traversal so attest-binaries can still see release-tag's shell check.
	exactRepoOnlyIf = "github.repository == 'NVIDIA/cluster-readiness-engine'"
)

// Exact shell comparison release.yml's release-tag job uses to refuse a
// workflow_dispatch whose GITHUB_REF is not the release tag. Mentioning
// GITHUB_REF is not enough; this is the tested restriction.
const exactReleaseTagRefCheck = `[[ "${GITHUB_REF}" != "refs/tags/${INPUT_TAG}" ]]`

// Fixture job names / uses for fail-closed recognition cases.
const (
	jobCaller       = "caller"
	jobGuarded      = "guarded"
	jobReleaseTag   = "release-tag"
	localAttestUses = "./.github/workflows/attest.yml"
)

// TestMainBranchAttestCallersPinExactRefGuards pins the full job-level `if:`
// expressions that keep workflow_dispatch off a v* tag from minting the release
// signing identity (#340).
//
// Substring needles are not enough: appending `|| github.event_name ==
// 'workflow_dispatch'` keeps both needles present while making the guard
// vacuous (`&&` binds tighter than `||`, and these workflows are
// workflow_dispatch-capable). Pinning the whitespace-normalized whole
// expression is the difference between asserting the guard is mentioned and
// asserting the guard is the condition.
//
// publish.yml carries the same load-bearing shape on `tag` / `attested`. Both
// files are tabled here so deleting either guard fails the same test.
func TestMainBranchAttestCallersPinExactRefGuards(t *testing.T) {
	cases := []struct {
		workflow string
		job      string
		wantIf   string
	}{
		{wfAttestSmoke, "smoke", exactRepoAndMainIf},
		{wfAttestSmoke, "report", exactAlwaysRepoAndMainIf},
		{wfPublish, "tag", exactRepoAndMainIf},
		{wfPublish, "attested", exactAlwaysRepoAndMainIf},
	}
	for _, tc := range cases {
		t.Run(tc.workflow+"/"+tc.job, func(t *testing.T) {
			got := normalizeWorkflowIf(jobIfCondition(t, tc.workflow, tc.job))
			if got != tc.wantIf {
				t.Errorf("%s job %q if: = %q, want exact %q; a widened expression that still "+
					"mentions the needles would mint the release signing identity on a v* "+
					"workflow_dispatch", tc.workflow, tc.job, got, tc.wantIf)
			}
		})
	}
}

// TestAttestDispatchCallersRequireRefGuards closes the class for future
// workflow_dispatch callers of attest.yml that forget their own ref guard.
//
// attest.yml's non-tag refusal only fires when allow_untagged is false; a
// caller that forgets a ref guard and does not pass the flag takes the release
// branch, hits no check, and mints the identity. Enumerating every
// workflow_dispatch caller and requiring an *effective* ref constraint on the
// path to each attest.yml call is what actually closes that class, whatever
// inputs the caller passes.
//
// Recognition is fail-closed: only the exact `if:` expressions pinned above
// and release.yml's exact GITHUB_REF comparison count. Substring mentions of
// github.ref / GITHUB_REF, non-restrictive checks, and ancestor guards behind
// an `if: always()` / `if: !cancelled()` caller do not. Unrecognized shapes
// fail this test so a new pattern must be explicitly allowlisted and covered
// before it protects anything.
//
// This covers callers whose workflow files already contain the fix. Existing
// release tags that still ship the pre-fix attest-selftest.yml are a rollout
// gap documented in SECURITY.md / RELEASE.md — merge alone does not close #340
// for those refs.
func TestAttestDispatchCallersRequireRefGuards(t *testing.T) {
	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		if base == wfAttest {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		triggers := workflowTriggers(raw, t)
		if _, ok := triggers["workflow_dispatch"]; !ok {
			continue
		}

		jobs := loadJobsWithNeeds(t, raw, base)
		var callers []string
		for name, job := range jobs {
			if isAttestWorkflowCall(job.Uses) {
				callers = append(callers, name)
			}
		}
		if len(callers) == 0 {
			continue
		}
		sort.Strings(callers)

		for _, caller := range callers {
			if !jobOrAncestorHasRefGuard(jobs, caller) {
				t.Errorf("%s: job %q calls attest.yml and the workflow has workflow_dispatch, "+
					"but neither %q nor any needs-ancestor carries a recognized ref guard "+
					"(exact main-branch if: or release.yml's GITHUB_REF tag check); without "+
					"one a dispatch at a v* ref mints the release signing identity",
					base, caller, caller)
			}
		}
	}
}

// TestRefGuardRecognitionFailsClosed pins the unsafe shapes that still returned
// true under substring / fail-open recognition: a non-restrictive github.ref
// check, a GITHUB_REF echo with no comparison, an always() caller that inherits
// a guarded ancestor, and a !cancelled() caller that likewise inherits (GitHub's
// documented alternative to always() for overriding skipped-needs). Unrecognized
// shapes must fail closed; only the explicit tested patterns may pass.
func TestRefGuardRecognitionFailsClosed(t *testing.T) {
	t.Run("negatives", func(t *testing.T) {
		cases := []struct {
			name   string
			jobs   map[string]policyJob
			caller string
		}{
			{
				name: "nonrestrictive github.ref inequality",
				jobs: map[string]policyJob{
					jobCaller: {
						If:   "github.ref != ''",
						Uses: localAttestUses,
					},
				},
				caller: jobCaller,
			},
			{
				name: "GITHUB_REF echo is not a restriction",
				jobs: map[string]policyJob{
					jobCaller: {
						Uses: localAttestUses,
						Runs: []string{`echo "$GITHUB_REF"`},
					},
				},
				caller: jobCaller,
			},
			{
				name: "always caller does not inherit ancestor ref guard",
				jobs: map[string]policyJob{
					jobGuarded: {If: exactRepoAndMainIf},
					jobCaller: {
						If:    "always()",
						Needs: []string{jobGuarded},
						Uses:  localAttestUses,
					},
				},
				caller: jobCaller,
			},
			{
				name: "always with repo gate still does not inherit",
				jobs: map[string]policyJob{
					jobGuarded: {If: exactRepoAndMainIf},
					jobCaller: {
						If:    "always() && github.repository == 'NVIDIA/cluster-readiness-engine'",
						Needs: []string{jobGuarded},
						Uses:  localAttestUses,
					},
				},
				caller: jobCaller,
			},
			{
				name: "cancelled caller does not inherit ancestor ref guard",
				jobs: map[string]policyJob{
					jobGuarded: {If: exactRepoAndMainIf},
					jobCaller: {
						If:    "${{ !cancelled() }}",
						Needs: []string{jobGuarded},
						Uses:  localAttestUses,
					},
				},
				caller: jobCaller,
			},
			{
				name: "bare !cancelled() does not inherit ancestor ref guard",
				jobs: map[string]policyJob{
					jobGuarded: {If: exactRepoAndMainIf},
					jobCaller: {
						If:    "!cancelled()",
						Needs: []string{jobGuarded},
						Uses:  localAttestUses,
					},
				},
				caller: jobCaller,
			},
			{
				name: "release-tag comparison without exit 1 is not a guard",
				jobs: map[string]policyJob{
					jobReleaseTag: {
						If:   exactRepoOnlyIf,
						Runs: []string{"if " + exactReleaseTagRefCheck + "; then\n  :\nfi"},
					},
					jobCaller: {Needs: []string{jobReleaseTag}, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if jobHasRefGuard(tc.jobs[tc.caller]) {
					t.Errorf("jobHasRefGuard(%q) = true, want false; unrecognized shapes must fail closed", tc.name)
				}
				if jobOrAncestorHasRefGuard(tc.jobs, tc.caller) {
					t.Errorf("jobOrAncestorHasRefGuard(%q) = true, want false; unrecognized shapes must fail closed", tc.name)
				}
			})
		}
	})

	t.Run("positives", func(t *testing.T) {
		cases := []struct {
			name   string
			jobs   map[string]policyJob
			caller string
		}{
			{
				name: "exact main-branch if on caller",
				jobs: map[string]policyJob{
					jobCaller: {If: exactRepoAndMainIf, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
			{
				name: "exact always+main if on caller",
				jobs: map[string]policyJob{
					jobCaller: {If: exactAlwaysRepoAndMainIf, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
			{
				name: "inherit exact main-branch if from needs",
				jobs: map[string]policyJob{
					jobGuarded: {If: exactRepoAndMainIf},
					jobCaller:  {Needs: []string{jobGuarded}, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
			{
				name: "release-tag GITHUB_REF comparison on ancestor",
				jobs: map[string]policyJob{
					jobReleaseTag: {
						If:   exactRepoOnlyIf,
						Runs: []string{"if " + exactReleaseTagRefCheck + "; then\n  exit 1\nfi"},
					},
					jobCaller: {Needs: []string{jobReleaseTag}, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
			{
				name: "inherit release-tag check through repo-only intermediate",
				jobs: map[string]policyJob{
					jobReleaseTag: {
						If:   exactRepoOnlyIf,
						Runs: []string{"if " + exactReleaseTagRefCheck + "; then\n  exit 1\nfi"},
					},
					"build-cli": {If: exactRepoOnlyIf, Needs: []string{jobReleaseTag}},
					jobCaller:   {Needs: []string{"build-cli"}, Uses: localAttestUses},
				},
				caller: jobCaller,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if !jobOrAncestorHasRefGuard(tc.jobs, tc.caller) {
					t.Errorf("jobOrAncestorHasRefGuard(%q) = false, want true for an allowlisted pattern", tc.name)
				}
			})
		}
	})
}

// TestReleaseTagRefCheckFailsClosed executes release.yml's Resolve tag step and
// pins that the mismatched-ref branch actually rejects. Substring recognition of
// the comparison alone stayed green when that branch's `exit 1` was stubbed with
// `:` (kaynetu #341); the recognizer and this execution table both require the
// rejection to remain effective. The step must also be mandatory: no
// continue-on-error that would swallow a failed check before attest callers run.
func TestReleaseTagRefCheckFailsClosed(t *testing.T) {
	script, continueOnError := releaseTagResolveStep(t)
	if continueOnError {
		t.Fatalf("%s job %q step Resolve tag has continue-on-error; a failed "+
			"mismatched-ref check must fail the job so attest callers do not run",
			wfRelease, jobReleaseTag)
	}
	if !runHasEffectiveReleaseTagRefCheck(script) {
		t.Fatalf("%s Resolve tag step no longer carries an effective GITHUB_REF "+
			"mismatch rejection (comparison + exit 1 in the then-branch)", wfRelease)
	}

	// Stub only the mismatched-ref branch's exit 1 (the mutation kaynetu applied).
	idx := strings.Index(script, exactReleaseTagRefCheck)
	if idx < 0 {
		t.Fatal("Resolve tag step missing exactReleaseTagRefCheck")
	}
	rest := script[idx:]
	exitIdx := strings.Index(rest, "exit 1")
	if exitIdx < 0 {
		t.Fatal("Resolve tag step missing exit 1 after ref check")
	}
	stubbed := script[:idx+exitIdx] + ":" + script[idx+exitIdx+len("exit 1"):]
	if runHasEffectiveReleaseTagRefCheck(stubbed) {
		t.Fatalf("runHasEffectiveReleaseTagRefCheck still true after stubbing " +
			"mismatched-ref exit 1; recognizer must fail closed")
	}

	dir := t.TempDir()
	acceptEnv := []string{
		"GITHUB_EVENT_NAME=workflow_dispatch",
		"GITHUB_REF=refs/tags/v1.2.3",
		"INPUT_TAG=v1.2.3",
	}
	out, failed := runShell(t, dir, script, acceptEnv...)
	if failed {
		t.Fatalf("Resolve tag rejected a matching dispatch ref: %s", out)
	}

	rejectDir := t.TempDir()
	rejectEnv := []string{
		"GITHUB_EVENT_NAME=workflow_dispatch",
		"GITHUB_REF=refs/heads/main",
		"INPUT_TAG=v1.2.3",
	}
	out, failed = runShell(t, rejectDir, script, rejectEnv...)
	if !failed {
		t.Fatalf("Resolve tag accepted mismatched ref "+
			"(GITHUB_REF=refs/heads/main, INPUT_TAG=v1.2.3); want exit 1. output: %s", out)
	}
	if !strings.Contains(out, "dispatched with tag=v1.2.3") {
		t.Fatalf("mismatched-ref rejection missing expected error annotation; output: %s", out)
	}

	// Stubbed script must accept the mismatched ref (proves the mutation removed
	// the rejection the recognizer is supposed to require).
	stubDir := t.TempDir()
	out, failed = runShell(t, stubDir, stubbed, rejectEnv...)
	if failed {
		t.Fatalf("stubbed Resolve tag still rejected mismatched ref; mutation did not remove the guard: %s", out)
	}
}

// releaseTagResolveStep returns the body of release.yml's release-tag
// "Resolve tag" step and whether that step sets continue-on-error.
func releaseTagResolveStep(t *testing.T) (script string, continueOnError bool) {
	t.Helper()

	path := filepath.Join(workflowDir, wfRelease)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name            string `json:"name"`
				Run             string `json:"run"`
				ContinueOnError bool   `json:"continue-on-error"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	job, ok := doc.Jobs[jobReleaseTag]
	if !ok {
		t.Fatalf("%s missing job %q", wfRelease, jobReleaseTag)
	}
	for _, step := range job.Steps {
		if step.Name == "Resolve tag" {
			return step.Run, step.ContinueOnError
		}
	}
	t.Fatalf("%s job %q has no step named %q", wfRelease, jobReleaseTag, "Resolve tag")
	return "", false
}

// normalizeWorkflowIf collapses YAML folded-scalar whitespace so an exact
// expression comparison is stable across `>-` line breaks.
func normalizeWorkflowIf(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func jobIfCondition(t *testing.T, workflow, jobName string) string {
	t.Helper()

	path := filepath.Join(workflowDir, workflow)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
			If string `json:"if"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	job, ok := wf.Jobs[jobName]
	if !ok {
		t.Fatalf("%s is missing job %q", workflow, jobName)
	}
	return job.If
}

type policyJob struct {
	If    string
	Uses  string
	Needs []string
	Runs  []string
}

func loadJobsWithNeeds(t *testing.T, raw []byte, base string) map[string]policyJob {
	t.Helper()

	var doc struct {
		Jobs map[string]struct {
			If    string        `json:"if"`
			Uses  string        `json:"uses"`
			Needs stringOrSlice `json:"needs"`
			Steps []struct {
				Run string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", base, err)
	}
	out := make(map[string]policyJob, len(doc.Jobs))
	for name, job := range doc.Jobs {
		runs := make([]string, 0, len(job.Steps))
		for _, step := range job.Steps {
			if step.Run != "" {
				runs = append(runs, step.Run)
			}
		}
		out[name] = policyJob{
			If:    job.If,
			Uses:  job.Uses,
			Needs: append([]string(nil), job.Needs...),
			Runs:  runs,
		}
	}
	return out
}

// jobHasRefGuard reports whether job itself carries a recognized, effective
// ref restriction. Fail-closed: only exact allowlisted `if:` expressions and
// release.yml's release-tag shell check that both compares GITHUB_REF and
// exits non-zero on mismatch. A bare github.ref / GITHUB_REF mention, or a
// comparison whose rejection has been stubbed out, is not a guard.
func jobHasRefGuard(job policyJob) bool {
	switch normalizeWorkflowIf(job.If) {
	case exactRepoAndMainIf, exactAlwaysRepoAndMainIf:
		return true
	}
	return slices.ContainsFunc(job.Runs, runHasEffectiveReleaseTagRefCheck)
}

// runHasEffectiveReleaseTagRefCheck reports whether run contains release.yml's
// exact GITHUB_REF mismatch comparison *and* an `exit 1` in that then-branch.
// The comparison text alone is insufficient: stubbing the rejection with `:`
// left the previous substring recognizer green (kaynetu #341).
func runHasEffectiveReleaseTagRefCheck(run string) bool {
	norm := normalizeWorkflowIf(run)
	idx := strings.Index(norm, exactReleaseTagRefCheck)
	if idx < 0 {
		return false
	}
	rest := norm[idx:]
	// Bound the then-branch at the first " fi" after the comparison so a later
	// unrelated `exit 1` in the same step cannot satisfy this check.
	end := strings.Index(rest, " fi")
	if end < 0 {
		end = len(rest)
	}
	return strings.Contains(rest[:end], "exit 1")
}

// jobIfPropagatesSkippedNeeds reports whether job's if: is empty or the exact
// repository-only gate release.yml uses on intermediates. Those are the only
// conditions we allowlist for needs-walk traversal. always(), !cancelled(), and
// every other non-empty unrecognized condition can let the job run when a
// guarded ancestor is skipped, so inheritance through them fails closed
// (ndipebot #341).
func jobIfPropagatesSkippedNeeds(job policyJob) bool {
	switch normalizeWorkflowIf(stripExpressionWrappers(job.If)) {
	case "", exactRepoOnlyIf:
		return true
	default:
		return false
	}
}

// stripExpressionWrappers removes a single surrounding ${{ }} so normalized
// comparisons see the inner expression.
func stripExpressionWrappers(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") {
		return strings.TrimSpace(s[3 : len(s)-2])
	}
	return s
}

// jobOrAncestorHasRefGuard walks the needs graph from name and returns true
// only when a recognized ref guard sits on the caller or on a needs-ancestor
// reached only through empty if: conditions (skip-propagating). always(),
// !cancelled(), and any other non-empty unrecognized if: block inheritance:
// those jobs can still run when the guarded ancestor is skipped.
func jobOrAncestorHasRefGuard(jobs map[string]policyJob, name string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if seen[n] {
			return false
		}
		seen[n] = true
		job, ok := jobs[n]
		if !ok {
			return false
		}
		if jobHasRefGuard(job) {
			return true
		}
		if !jobIfPropagatesSkippedNeeds(job) {
			return false
		}
		return slices.ContainsFunc(job.Needs, walk)
	}
	return walk(name)
}
