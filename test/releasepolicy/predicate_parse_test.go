// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const releaseWorkflow = "../../.github/workflows/release.yml"

// Values the extracted step compares the predicate against. They stand in for
// the workflow's own environment, so a case fails on the field it is about.
const (
	predRepo   = "NVIDIA/cluster-readiness-engine"
	predTag    = "v1.2.3"
	predRef    = "refs/tags/" + predTag
	predCommit = "1111111111111111111111111111111111111111"
)

// predicateScript returns the body of release.yml's provenance-predicate step.
//
// Extracted rather than copied for the same reason as attest.yml's validation:
// a copy would drift, and a drifted copy would keep passing while the real
// check rotted.
func predicateScript(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflow, err)
	}

	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `json:"name"`
				Run  string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", releaseWorkflow, err)
	}

	job, ok := wf.Jobs["verify-release"]
	if !ok {
		t.Fatalf("%s has no `verify-release` job; the release gate must not be removed", releaseWorkflow)
	}
	for _, step := range job.Steps {
		if step.Name == "Verify the provenance predicate" {
			return step.Run
		}
	}
	t.Fatalf("%s `verify-release` has no `Verify the provenance predicate` step", releaseWorkflow)
	return ""
}

// envelope renders one DSSE envelope as cosign prints it: a JSON object whose
// `payload` is the base64-encoded in-toto statement.
func envelope(repo, ref, commit string) string {
	statement := map[string]any{
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"externalParameters": map[string]any{
					"repository": repo,
					"ref":        ref,
				},
				"resolvedDependencies": []any{
					map[string]any{"digest": map[string]any{"gitCommit": commit}},
				},
			},
		},
	}
	body, err := json.Marshal(statement)
	if err != nil {
		panic(err)
	}
	out, err := json.Marshal(map[string]string{
		"payload": base64.StdEncoding.EncodeToString(body),
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

// runPredicate executes the extracted step with a stub cosign that prints the
// given envelopes, one per line, the way cosign emits one per verified
// attestation.
func runPredicate(t *testing.T, envelopes []string) (string, error) {
	t.Helper()

	dir := t.TempDir()
	stub := "#!/usr/bin/env bash\n"
	if len(envelopes) == 0 {
		stub += "exit 1\n"
	} else {
		stub += fmt.Sprintf("cat <<'ENVELOPES'\n%s\nENVELOPES\n", strings.Join(envelopes, "\n"))
	}
	stubPath := filepath.Join(dir, "cosign")
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write cosign stub: %v", err)
	}

	cmd := exec.Command("bash", "-c", predicateScript(t))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"IDENTITY=https://github.com/"+predRepo+"/.github/workflows/attest.yml@"+predRef,
		"OIDC_ISSUER=https://token.actions.githubusercontent.com",
		"IMAGE=ghcr.io/nvidia/cluster-readiness-engine/manager",
		"INDEX="+validDigest,
		"GITHUB_REPOSITORY="+predRepo,
		"GITHUB_SHA="+predCommit,
		"TAG="+predTag,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestPredicateCheckToleratesDuplicateAttestations pins the contract attest.yml
// states about its own retries: an attempt killed after its registry push and
// Rekor entry landed publishes a second valid attestation for the same digest,
// and "any consumer that walks attestations must therefore tolerate more than
// one attestation per digest and must not treat a duplicate as tampering".
//
// This gate is that consumer. Reading cosign's output into a single variable
// concatenates the statements, so every scalar comparison sees a multi-line
// value and fails — turning a harmless duplicate into a tampering-shaped error
// that strands a correct release as an invisible draft.
func TestPredicateCheckToleratesDuplicateAttestations(t *testing.T) {
	good := envelope(predRepo, predRef, predCommit)

	cases := []struct {
		name      string
		envelopes []string
		wantPass  bool
		wantText  string
	}{
		{
			name:      "one attestation",
			envelopes: []string{good},
			wantPass:  true,
		},
		{
			name:      "duplicate attestations from a retry",
			envelopes: []string{good, good},
			wantPass:  true,
			wantText:  "2 provenance attestation(s) checked",
		},
		{
			name:      "one good and one for a different commit",
			envelopes: []string{good, envelope(predRepo, predRef, "deadbeef")},
			wantPass:  false,
			wantText:  "names commit deadbeef",
		},
		{
			name:      "wrong repository",
			envelopes: []string{envelope("attacker/repo", predRef, predCommit)},
			wantPass:  false,
			wantText:  "names repository attacker/repo",
		},
		{
			name:      "wrong ref",
			envelopes: []string{envelope(predRepo, "refs/heads/main", predCommit)},
			wantPass:  false,
			wantText:  "names ref refs/heads/main",
		},
		{
			name:      "no attestation at all",
			envelopes: nil,
			wantPass:  false,
			wantText:  "could not read the provenance predicate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPredicate(t, tc.envelopes)
			if tc.wantPass && err != nil {
				t.Errorf("expected the predicate check to pass, got %v\n%s", err, out)
			}
			if !tc.wantPass && err == nil {
				t.Errorf("expected the predicate check to fail, it passed\n%s", out)
			}
			if tc.wantText != "" && !strings.Contains(out, tc.wantText) {
				t.Errorf("expected output to contain %q, got:\n%s", tc.wantText, out)
			}
		})
	}
}
