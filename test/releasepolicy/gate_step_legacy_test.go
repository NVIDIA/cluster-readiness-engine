// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #270 residual: skipping a signing *step* (the job still succeeds) is caught
// by verify-release's registry re-check, not by the needs.<job>.result guards.
// A live dispatch with the step disabled is not required: these tests extract
// the image and chart steps and drive them against a stub that mints nothing.
//
// #270 residual: a legacy-format bundle is rejected rather than read, for
// detached attest-blob assets and for registry subjects. Cosign's verify
// commands fall back to the old parser; the gate must refuse before that.

const (
	imageStepName = "Verify the container image"
	chartStepName = "Verify the Helm chart"
	assetStepName = "Verify every release asset and its bundle"
	publicStep    = "Re-check over the public channel"

	stubImage = "ghcr.io/nvidia/cluster-readiness-engine/manager"
	stubChart = "ghcr.io/nvidia/cluster-readiness-engine/cluster-readiness-engine"

	newFormatTree = "📦 Supply Chain Security Related artifacts\n" +
		"└── 🔗 https://sigstore.dev/cosign/sign/v1 artifacts via OCI referrer: " +
		stubImage + "@" + validDigest + "\n"

	legacySigTree = "└── 🔐 Signatures for an image tag: " + stubImage +
		":sha256-1111111111111111111111111111111111111111111111111111111111111111.sig\n"

	legacyAttTree = "└── 💾 Attestations for an image tag: " + stubImage +
		":sha256-1111111111111111111111111111111111111111111111111111111111111111.att\n"

	errLegacyRegistry = "has a legacy-format registry tag; refuse to read it"
	errLegacyBundle   = "has a legacy-format bundle"
	errLegacyDetached = "legacy detached signature"
	errNotJSON        = "bundle is not JSON; refuse to read it"
)

const registryHarness = `
set -uo pipefail
sleep() { :; }

cosign() {
  printf '%%s\n' "$*" >> "${STUB_COSIGN_LOG}"
  case "${1:-}" in
    tree)
      if [[ "${STUB_TREE_FAIL}" == "true" ]]; then return 1; fi
          printf '%%s\n' "${STUB_TREE}"
      return 0
      ;;
    verify)
      if [[ "${STUB_VERIFY_FAIL}" == "true" ]]; then return 1; fi
      local i
      for ((i = 1; i <= $#; i++)); do
        if [[ "${!i}" == "--output" ]]; then
          printf '%%s\n' "${STUB_VERIFY_JSON}"
          return 0
        fi
      done
      return 0
      ;;
    verify-attestation)
      if [[ "${STUB_VERIFY_FAIL}" == "true" ]]; then return 1; fi
      return 0
      ;;
    *)
      echo "unexpected cosign subcommand: $*" >&2
      return 1
      ;;
  esac
}

%s
`

func tagBindingJSON(digest string) string {
	return `[{"critical":{"image":{"docker-manifest-digest":"` + digest + `"}}}]`
}

func runRegistryStep(t *testing.T, step string, env []string) (string, bool, string) {
	t.Helper()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "cosign-calls.log")
	env = append(env,
		"STUB_COSIGN_LOG="+logPath,
		"STUB_VERIFY_JSON="+tagBindingJSON(validDigest),
	)
	out, failed := runShell(t, dir, fmt.Sprintf(registryHarness, step), env...)
	raw, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read cosign log: %v", err)
	}
	return out, failed, string(raw)
}

func imageEnv(tree, treeFail, verifyFail string) []string {
	return []string{
		"IDENTITY=" + wantIdentity,
		"OIDC_ISSUER=" + wantIssuer,
		"IMAGE=" + stubImage,
		"INDEX=" + validDigest,
		"AMD64=" + validDigest,
		"ARM64=" + validDigest,
		"TAG=" + predTag,
		"STUB_TREE=" + tree,
		"STUB_TREE_FAIL=" + treeFail,
		"STUB_VERIFY_FAIL=" + verifyFail,
	}
}

func chartEnv(tree, treeFail, verifyFail string) []string {
	return []string{
		"IDENTITY=" + wantIdentity,
		"OIDC_ISSUER=" + wantIssuer,
		"CHART_NAME=" + stubChart,
		"CHART_DIGEST=" + validDigest,
		"STUB_TREE=" + tree,
		"STUB_TREE_FAIL=" + treeFail,
		"STUB_VERIFY_FAIL=" + verifyFail,
	}
}

func logHasVerify(log string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if strings.HasPrefix(line, "verify") || strings.HasPrefix(line, "verify-attestation") {
			return true
		}
	}
	return false
}

func logHasTree(log string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if strings.HasPrefix(line, "tree ") {
			return true
		}
	}
	return false
}

// TestGateRegistryRecheckRejectsUnsigned covers the step-disabled half of
// #270's "skipping any signing step fails the gate" criterion.
//
// Disable Sign and Attest inside attest.yml and the job can still succeed:
// there is no step-level `if: always()` counterpart to the three job-result
// guards. verify-release then asks the registry, and a signature that was
// never minted fails there. That is the path this test executes.
func TestGateRegistryRecheckRejectsUnsigned(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, log := runRegistryStep(t, step, imageEnv(newFormatTree, "false", "true"))
		if !failed {
			t.Fatalf("the image gate published a release whose registry signatures were never minted\n%s", out)
		}
		if !strings.Contains(out, "::error::") {
			t.Errorf("the image gate failed without saying why\n%s", out)
		}
		if !logHasTree(log) {
			t.Errorf("the image gate never inspected registry attachments; unsigned and legacy would look the same")
		}
		if !logHasVerify(log) {
			t.Errorf("the image gate did not ask cosign to verify; a tree-only step cannot catch a missing signature")
		}
	})

	t.Run("chart", func(t *testing.T) {
		step := gateStep(t, "verify-release", chartStepName)
		out, failed, log := runRegistryStep(t, step, chartEnv(newFormatTree, "false", "true"))
		if !failed {
			t.Fatalf("the chart gate published a release whose registry signatures were never minted\n%s", out)
		}
		if !strings.Contains(out, "::error::") {
			t.Errorf("the chart gate failed without saying why\n%s", out)
		}
		if !logHasVerify(log) {
			t.Errorf("the chart gate did not ask cosign to verify\n%s", out)
		}
	})

	t.Run("image with signatures present still passes", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, _ := runRegistryStep(t, step, imageEnv(newFormatTree, "false", "false"))
		if failed {
			t.Fatalf("the image gate rejected a signed image\n%s", out)
		}
	})

	t.Run("chart with signatures present still passes", func(t *testing.T) {
		step := gateStep(t, "verify-release", chartStepName)
		out, failed, _ := runRegistryStep(t, step, chartEnv(newFormatTree, "false", "false"))
		if failed {
			t.Fatalf("the chart gate rejected a signed chart\n%s", out)
		}
	})
}

// TestGateRejectsLegacyBundles covers #270's "a legacy-format bundle is
// rejected rather than read" criterion, for both detached attest-blob
// bundles and registry subjects.
func TestGateRejectsLegacyBundles(t *testing.T) {
	t.Run("detached LocalSignedPayload", func(t *testing.T) {
		step := gateStep(t, "verify-release", assetStepName)
		dir := stageRelease(t, "", "")
		path := filepath.Join(dir, "verify", installerBundleFile)
		if err := os.WriteFile(path, []byte(legacyBlobBundle), 0o600); err != nil {
			t.Fatalf("write legacy bundle: %v", err)
		}
		out, failed, calls := runAssetGate(t, step, dir, "", true)
		if !failed {
			t.Fatalf("the gate verified a legacy-format blob bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		for _, c := range calls {
			if c.bundle == installerBundleFile {
				t.Errorf("the gate called cosign on the legacy bundle %s; refuse means do not read it", c.bundle)
			}
		}
	})

	t.Run("detached non-JSON bundle", func(t *testing.T) {
		step := gateStep(t, "verify-release", assetStepName)
		dir := stageRelease(t, "", "")
		path := filepath.Join(dir, "verify", installerBundleFile)
		if err := os.WriteFile(path, []byte("not-json\n"), 0o600); err != nil {
			t.Fatalf("write invalid bundle: %v", err)
		}
		out, failed, _ := runAssetGate(t, step, dir, "", true)
		if !failed {
			t.Fatalf("the gate verified a non-JSON bundle\n%s", out)
		}
		if !strings.Contains(out, errNotJSON) {
			t.Errorf("want a JSON rejection, got:\n%s", out)
		}
	})

	t.Run("detached sibling .sig", func(t *testing.T) {
		step := gateStep(t, "verify-release", assetStepName)
		dir := stageRelease(t, "", "")
		path := filepath.Join(dir, "verify", "installer.sig")
		if err := os.WriteFile(path, []byte("legacy-sig\n"), 0o600); err != nil {
			t.Fatalf("write .sig: %v", err)
		}
		out, failed, calls := runAssetGate(t, step, dir, "", true)
		if !failed {
			t.Fatalf("the gate accepted a sibling .sig next to a protobuf bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyDetached) {
			t.Errorf("want a detached .sig rejection, got:\n%s", out)
		}
		for _, c := range calls {
			if c.bundle == installerBundleFile {
				t.Errorf("the gate called cosign despite installer.sig; refuse means do not read it")
			}
		}
	})

	t.Run("detached SBOM-binding LocalSignedPayload", func(t *testing.T) {
		step := gateStep(t, "verify-release", assetStepName)
		dir := stageRelease(t, "", "")
		path := filepath.Join(dir, "verify", darwinARM64+".cyclonedx.sigstore.json")
		if err := os.WriteFile(path, []byte(legacyBlobBundle), 0o600); err != nil {
			t.Fatalf("write legacy SBOM binding: %v", err)
		}
		out, failed, calls := runAssetGate(t, step, dir, "", true)
		if !failed {
			t.Fatalf("the gate verified a legacy-format SBOM-binding bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		want := darwinARM64 + ".cyclonedx.sigstore.json"
		for _, c := range calls {
			if c.bundle == want {
				t.Errorf("the gate called cosign on the legacy SBOM binding; refuse means do not read it")
			}
		}
	})
}

// TestGateRejectsLegacyRegistryTags is the registry half of the same
// criterion: a sha256-<digest>.sig / .att sidecar is refused without a
// subsequent cosign verify.
func TestGateRejectsLegacyRegistryTags(t *testing.T) {
	t.Run("registry .sig sidecar on the image", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, log := runRegistryStep(t, step, imageEnv(legacySigTree, "false", "false"))
		if !failed {
			t.Fatalf("the image gate read a legacy sha256-<digest>.sig tag\n%s", out)
		}
		if !strings.Contains(out, errLegacyRegistry) {
			t.Errorf("want a legacy-tag rejection, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the image gate called cosign verify after seeing a legacy tag:\n%s", log)
		}
	})

	t.Run("registry .att sidecar on the image", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, log := runRegistryStep(t, step, imageEnv(legacyAttTree, "false", "false"))
		if !failed {
			t.Fatalf("the image gate read a legacy sha256-<digest>.att tag\n%s", out)
		}
		if !strings.Contains(out, errLegacyRegistry) {
			t.Errorf("want a legacy-tag rejection, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the image gate called cosign verify after seeing a legacy tag:\n%s", log)
		}
	})

	t.Run("registry .sig sidecar on the chart", func(t *testing.T) {
		step := gateStep(t, "verify-release", chartStepName)
		out, failed, log := runRegistryStep(t, step, chartEnv(legacySigTree, "false", "false"))
		if !failed {
			t.Fatalf("the chart gate read a legacy sha256-<digest>.sig tag\n%s", out)
		}
		if !strings.Contains(out, errLegacyRegistry) {
			t.Errorf("want a legacy-tag rejection, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the chart gate called cosign verify after seeing a legacy tag:\n%s", log)
		}
	})

	t.Run("registry inspect failure is fail-closed", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, log := runRegistryStep(t, step, imageEnv(newFormatTree, "true", "false"))
		if !failed {
			t.Fatalf("the image gate skipped the format check when tree failed\n%s", out)
		}
		if !strings.Contains(out, "could not inspect registry attachments") {
			t.Errorf("want an inspect-failure error, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the image gate called verify without inspecting attachments:\n%s", log)
		}
	})
}

const publicHarness = `
set -uo pipefail
sleep() { :; }
gh() { echo public; }
curl() {
  local out=""
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "-o" ]]; then
      out="$2"; shift 2; continue
    fi
    shift
  done
  cp "${STUB_FIXTURE}/$(basename "${out}")" "${out}"
}
cosign() {
  printf '%%s\n' "$*" >> "${STUB_COSIGN_LOG}"
  return 0
}

%s
`

func stagePublicFixture(t *testing.T, installerBundle string) string {
	t.Helper()

	dir := t.TempDir()
	fx := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(fx, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	files := map[string]string{
		"installer":                          "#!/bin/sh\ntrue\n",
		installerBundleFile:                  installerBundle,
		"nvcrectl-linux-amd64":               "binary\n",
		"nvcrectl-linux-amd64.sigstore.json": protobufBundle,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(fx, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func runPublicStep(t *testing.T, step, dir string) (string, bool, string) {
	t.Helper()

	logPath := filepath.Join(dir, "cosign-calls.log")
	fx := filepath.Join(dir, "fixture")
	out, failed := runShell(t, dir, fmt.Sprintf(publicHarness, step),
		"STUB_FIXTURE="+fx,
		"STUB_COSIGN_LOG="+logPath,
		"IDENTITY="+wantIdentity,
		"OIDC_ISSUER="+wantIssuer,
		"GITHUB_REPOSITORY="+predRepo,
		"TAG="+predTag,
		"GH_TOKEN=test-token",
	)
	raw, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read cosign log: %v", err)
	}
	return out, failed, string(raw)
}

// TestPublicRecheckRejectsLegacyBundle drives the post-publish public-channel
// step, which is the other detached attest-blob path the criterion names.
func TestPublicRecheckRejectsLegacyBundle(t *testing.T) {
	step := gateStep(t, "verify-release", publicStep)

	t.Run("protobuf bundles pass", func(t *testing.T) {
		dir := stagePublicFixture(t, protobufBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if failed {
			t.Fatalf("the public re-check rejected a protobuf bundle\n%s", out)
		}
		if !strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check never verified the downloaded bytes\n%s", out)
		}
	})

	t.Run("legacy bundle is refused without calling cosign", func(t *testing.T) {
		dir := stagePublicFixture(t, legacyBlobBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if !failed {
			t.Fatalf("the public re-check verified a legacy-format bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		if strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check called cosign on a legacy bundle:\n%s", log)
		}
	})
}
