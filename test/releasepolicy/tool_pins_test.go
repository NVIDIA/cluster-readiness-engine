// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	wfDocsVerify     = "docs-verify.yml"
	wfCI             = "ci.yml"
	wfUAT            = "uat.yml"
	readAttestPins   = "./.github/actions/read-attest-pins"
	setupHelmAction  = "./.github/actions/setup-helm"
	readAttestScript = "../../.github/actions/read-attest-pins/read.py"
	dependabotFile   = "../../.github/dependabot.yml"
	setupHelmFile    = "../../.github/actions/setup-helm/action.yml"
)

// TestVerifierJobsReadAttestPins is the check kaynetu's #294 review asked for.
//
// Dependabot's github-actions ecosystem updates `uses:` SHAs only. The signer
// pin is the attest.yml workflow_call default. A second COSIGN_VERSION,
// literal cosign-release, or CRANE_VERSION/CRANE_SHA256 pair on a verifier
// job lets verification drift from the bundle format the signer emits.
func TestVerifierJobsReadAttestPins(t *testing.T) {
	pins := readAttestPinsFromScript(t)

	for _, name := range []string{wfRelease, wfDocsVerify} {
		body := readRepoFile(t, filepath.Join(workflowDir, name))
		if !strings.Contains(body, readAttestPins) {
			t.Errorf("%s does not use %s; verifier jobs must read the attest.yml defaults",
				name, readAttestPins)
		}
		for key, value := range pins {
			if strings.Contains(body, value) {
				t.Errorf("%s still embeds %s %s; read it from attest.yml so the pin has one home",
					name, key, value)
			}
		}
	}
}

// TestHelmCLIPinIsCentralized keeps the CLI version from drifting across the
// three workflows that install helm. Dependabot updates the setup-helm action
// SHA inside the composite action; it does not touch `version:`.
func TestHelmCLIPinIsCentralized(t *testing.T) {
	action := readRepoFile(t, setupHelmFile)
	if !strings.Contains(action, "azure/setup-helm@") {
		t.Fatalf("%s no longer calls azure/setup-helm; the composite action is the pin", setupHelmFile)
	}
	if !strings.Contains(action, "version:") {
		t.Fatalf("%s has no Helm CLI version: input; that is the manual pin", setupHelmFile)
	}

	for _, name := range []string{wfCI, wfRelease, wfUAT} {
		body := readRepoFile(t, filepath.Join(workflowDir, name))
		if strings.Contains(body, "azure/setup-helm") {
			t.Errorf("%s still calls azure/setup-helm directly; use %s so the CLI version cannot drift",
				name, setupHelmAction)
		}
		if !strings.Contains(body, setupHelmAction) {
			t.Errorf("%s does not use %s", name, setupHelmAction)
		}
	}
}

// TestDependabotDocumentsManualPins is the #279 comment's tracking artifact.
// A pin that drops off this list is a pin that rots: Dependabot cannot see it.
func TestDependabotDocumentsManualPins(t *testing.T) {
	body := readRepoFile(t, dependabotFile)
	for _, needle := range []string{
		"cosign_version",
		"crane_version",
		"CRANE_SHA256",
		"read-attest-pins",
		"setup-helm",
		"SYFT_VERSION",
		"SYFT_SHA256",
		"YQ_VERSION",
		"KIND_VERSION",
		"TILT_VERSION",
		"KWOK_VERSION",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("dependabot.yml manual-pin list does not mention %s", needle)
		}
	}
}

func readAttestPinsFromScript(t *testing.T) map[string]string {
	t.Helper()

	cmd := exec.Command("python3", readAttestScript)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read-attest-pins: %v\n%s", err, out)
	}

	pins := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok || key == "" || value == "" {
			t.Fatalf("read-attest-pins emitted a malformed line %q", sc.Text())
		}
		pins[key] = value
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan read-attest-pins output: %v", err)
	}
	for _, key := range []string{"cosign_version", "crane_version", "crane_sha256"} {
		if pins[key] == "" {
			t.Fatalf("read-attest-pins omitted %s", key)
		}
	}
	return pins
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
