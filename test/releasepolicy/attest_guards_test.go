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
