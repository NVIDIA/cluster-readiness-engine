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
//
// The criterion says "test by dispatch with the step disabled". These tests
// extract the image and chart steps and drive them against a stub instead.
// A stub cannot prove live dispatch wiring (permissions, needs, if:). It can
// fail each signature type (sign / provenance / SBOM) on its own, which a
// single dispatch that disables the whole Sign and Attest step cannot. That
// is the tradeoff: per-call coverage here, dispatch wiring still unexecuted.
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

	// Distinct from validDigest (the index) so a single surviving tree/crane
	// call cannot reproduce the observable behaviour of all three.
	amd64Digest  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	arm64Digest  = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	tagAltDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	emptyTree = "📦 Supply Chain Security Related artifacts\n" +
		"No Supply Chain Security Related Artifacts found\n"

	errLegacyRegistry = "has a legacy-format registry tag; refuse to read it"
	errLegacyBundle   = "has a legacy-format bundle"
	errLegacyDetached = "legacy detached signature"
	errNotJSON        = "bundle is not JSON; refuse to read it"
	errInspect        = "could not inspect registry attachments"
)

const registryHarness = `
set -uo pipefail
sleep() { :; }

# timeout is GNU coreutils and is what retry() wraps every call in. Drop the
# flag and duration and exec the rest so the stub cosign/crane functions run.
timeout() {
  [[ "${1:-}" == "--foreground" ]] && shift
  shift
  "$@"
}

# The subject is the first non-flag argument after the subcommand. Identity
# flags are appended after it, so ${!#} is the issuer, not the digest.
cosign_subject() {
  local i=2
  while (( i <= $# )); do
    case "${!i}" in
      --output|--type|--certificate-identity|--certificate-oidc-issuer|--bundle)
        i=$((i + 2))
        ;;
      --*)
        i=$((i + 1))
        ;;
      *)
        printf '%%s' "${!i}"
        return 0
        ;;
    esac
  done
  return 1
}

cosign() {
  printf '%%s\n' "$*" >> "${STUB_COSIGN_LOG}"
  case "${1:-}" in
    tree)
      local tree_ref="${2:-}"
      if [[ "${STUB_TREE_FAIL}" == "true" ]]; then return 1; fi
      case " ${STUB_TREE_FAIL_REFS} " in
        *" ${tree_ref} "*) return 1 ;;
      esac
      printf '%%s\n' "${STUB_TREE}"
      return 0
      ;;
    verify)
      local i selector="sign:$(cosign_subject "$@")"
      for ((i = 1; i <= $#; i++)); do
        if [[ "${!i}" == "--output" ]]; then
          selector="tag-binding"
          printf '%%s\n' "${STUB_VERIFY_JSON}"
          break
        fi
      done
      case " ${STUB_VERIFY_FAIL} " in
        *" ${selector} "*) return 1 ;;
      esac
      return 0
      ;;
    verify-attestation)
      local i j ptype="" selector
      for ((i = 1; i <= $#; i++)); do
        if [[ "${!i}" == "--type" ]]; then
          j=$((i + 1))
          ptype="${!j}"
        fi
      done
      selector="provenance:$(cosign_subject "$@")"
      if [[ "${ptype}" == "cyclonedx" ]]; then
        selector="sbom:$(cosign_subject "$@")"
      fi
      case " ${STUB_VERIFY_FAIL} " in
        *" ${selector} "*) return 1 ;;
      esac
      return 0
      ;;
    *)
      echo "unexpected cosign subcommand: $*" >&2
      return 1
      ;;
  esac
}

crane() {
  printf '%%s\n' "$*" >> "${STUB_CRANE_LOG}"
  local ref="${!#}"
  case " ${STUB_CRANE_LEGACY} " in
    *" ${ref} "*) return 0 ;;
  esac
  case " ${STUB_CRANE_ERROR} " in
    *" ${ref} "*)
      echo "Error: fetching manifest ${ref}: INTERNAL_ERROR: 500" >&2
      return 1
      ;;
  esac
  if [[ "${1:-}" == "digest" ]]; then
    printf '%%s\n' "${STUB_TAG_DIGEST}"
    return 0
  fi
  echo "Error: fetching manifest ${ref}: GET https://ghcr.io/v2/example/manifests/${ref##*:}: ` +
	`MANIFEST_UNKNOWN: manifest unknown" >&2
  return 1
}

%s
`

func tagBindingJSON(digest string) string {
	return `[{"critical":{"image":{"docker-manifest-digest":"` + digest + `"}}}]`
}

func sidecarTag(image, digest, suffix string) string {
	return image + ":sha256-" + strings.TrimPrefix(digest, "sha256:") + "." + suffix
}

type registryStub struct {
	tree         string
	treeFail     string
	treeFailRefs string
	verifyFail   string
	craneLegacy  string
	craneError   string
	tagDigest    string
}

func (s registryStub) withDefaults() registryStub {
	if s.tree == "" {
		s.tree = emptyTree
	}
	if s.treeFail == "" {
		s.treeFail = "false"
	}
	if s.tagDigest == "" {
		s.tagDigest = validDigest
	}
	return s
}

func runRegistryStep(t *testing.T, step string, env []string) (string, bool, string, string) {
	t.Helper()

	dir := t.TempDir()
	cosignLog := filepath.Join(dir, "cosign-calls.log")
	craneLog := filepath.Join(dir, "crane-calls.log")
	env = append(env,
		"STUB_COSIGN_LOG="+cosignLog,
		"STUB_CRANE_LOG="+craneLog,
		"STUB_VERIFY_JSON="+tagBindingJSON(validDigest),
	)
	out, failed := runShell(t, dir, fmt.Sprintf(registryHarness, step), env...)
	return out, failed, readLogFile(t, cosignLog), readLogFile(t, craneLog)
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func imageEnv(s registryStub) []string {
	s = s.withDefaults()
	return []string{
		"IDENTITY=" + wantIdentity,
		"OIDC_ISSUER=" + wantIssuer,
		"IMAGE=" + stubImage,
		"INDEX=" + validDigest,
		"AMD64=" + amd64Digest,
		"ARM64=" + arm64Digest,
		"TAG=" + predTag,
		"STUB_TREE=" + s.tree,
		"STUB_TREE_FAIL=" + s.treeFail,
		"STUB_TREE_FAIL_REFS=" + s.treeFailRefs,
		"STUB_VERIFY_FAIL=" + s.verifyFail,
		"STUB_CRANE_LEGACY=" + s.craneLegacy,
		"STUB_CRANE_ERROR=" + s.craneError,
		"STUB_TAG_DIGEST=" + s.tagDigest,
	}
}

func chartEnv(s registryStub) []string {
	s = s.withDefaults()
	return []string{
		"IDENTITY=" + wantIdentity,
		"OIDC_ISSUER=" + wantIssuer,
		"CHART_NAME=" + stubChart,
		"CHART_DIGEST=" + validDigest,
		"STUB_TREE=" + s.tree,
		"STUB_TREE_FAIL=" + s.treeFail,
		"STUB_TREE_FAIL_REFS=" + s.treeFailRefs,
		"STUB_VERIFY_FAIL=" + s.verifyFail,
		"STUB_CRANE_LEGACY=" + s.craneLegacy,
		"STUB_CRANE_ERROR=" + s.craneError,
		"STUB_TAG_DIGEST=" + s.tagDigest,
	}
}

func parseFlag(line, flag string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// logHasVerify reports any cosign verify* invocation, including the tag-binding
// `verify --output json` that runs before the four digest verifies. Use this at
// negative sites where that call is the leak to catch.
func logHasVerify(log string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if strings.HasPrefix(line, "verify ") || strings.HasPrefix(line, "verify-attestation ") {
			return true
		}
	}
	return false
}

// logHasDigestVerify reports a signature or attestation check, not the
// tag-binding `verify --output json`. The positive unsigned-registry cases
// must use this: otherwise that one call satisfies them with all four
// verify() calls deleted.
func logHasDigestVerify(log string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if strings.HasPrefix(line, "verify-attestation ") {
			return true
		}
		if strings.HasPrefix(line, "verify ") && !strings.Contains(line, "--output") {
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

func assertPinnedIdentity(t *testing.T, log string) {
	t.Helper()
	n := 0
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		cmd, _, _ := strings.Cut(line, " ")
		if cmd != "verify" && cmd != "verify-attestation" && cmd != "verify-blob-attestation" {
			continue
		}
		n++
		if id := parseFlag(line, "--certificate-identity"); id != wantIdentity {
			t.Errorf("call %q pinned identity %q, want %q", line, id, wantIdentity)
		}
		if iss := parseFlag(line, "--certificate-oidc-issuer"); iss != wantIssuer {
			t.Errorf("call %q pinned issuer %q, want %q", line, iss, wantIssuer)
		}
	}
	if n == 0 {
		t.Errorf("no verify* calls to pin identity on:\n%s", log)
	}
}

func assertRegistryTimeout(t *testing.T, step string) {
	t.Helper()
	if !strings.Contains(step, "timeout --foreground 120s") {
		t.Errorf("the registry inspect retry has no per-call timeout")
	}
}

// TestGateRegistryRecheckRejectsUnsigned covers the step-disabled half of
// #270's "skipping any signing step fails the gate" criterion.
//
// Disable Sign and Attest inside attest.yml and the job can still succeed:
// there is no step-level `if: always()` counterpart to the three job-result
// guards. verify-release then asks the registry, and a signature that was
// never minted fails there. That is the path this test executes.
//
// Sign and Attest issues up to three cosign calls per subject (sign,
// provenance, SBOM). A stub that fails every verify at once goes red at the
// tag-binding check and never sees the rest, so each selector below fails
// one call and leaves the others green.
func TestGateRegistryRecheckRejectsUnsigned(t *testing.T) {
	t.Run("image", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		assertRegistryTimeout(t, step)
		for _, tc := range []struct{ name, fail, want string }{
			{"index signature", "sign:" + stubImage + "@" + validDigest, "index signature does not verify"},
			{"index provenance", "provenance:" + stubImage + "@" + validDigest, "index provenance does not verify"},
			{"amd64 SBOM", "sbom:" + stubImage + "@" + amd64Digest, "amd64 SBOM does not verify"},
			{"arm64 SBOM", "sbom:" + stubImage + "@" + arm64Digest, "arm64 SBOM does not verify"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, failed, log, _ := runRegistryStep(t, step, imageEnv(registryStub{verifyFail: tc.fail}))
				if !failed {
					t.Fatalf("the image gate published a release whose %s was never minted\n%s", tc.name, out)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("want %q, got:\n%s", tc.want, out)
				}
				if !logHasTree(log) {
					t.Errorf("the image gate never inspected registry attachments; unsigned and legacy would look the same")
				}
				if !logHasDigestVerify(log) {
					t.Errorf("the image gate did not ask cosign to verify a digest; " +
						"tag-binding verify --output json cannot catch a missing signature")
				}
			})
		}
	})

	t.Run("chart", func(t *testing.T) {
		step := gateStep(t, "verify-release", chartStepName)
		assertRegistryTimeout(t, step)
		for _, tc := range []struct{ name, fail, want string }{
			{"chart signature", "sign:" + stubChart + "@" + validDigest, "chart signature does not verify"},
			{"chart provenance", "provenance:" + stubChart + "@" + validDigest, "chart provenance does not verify"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, failed, log, _ := runRegistryStep(t, step, chartEnv(registryStub{verifyFail: tc.fail}))
				if !failed {
					t.Fatalf("the chart gate published a release whose %s was never minted\n%s", tc.name, out)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("want %q, got:\n%s", tc.want, out)
				}
				if !logHasDigestVerify(log) {
					t.Errorf("the chart gate did not ask cosign to verify\n%s", out)
				}
			})
		}
	})

	t.Run("image with signatures present still passes", func(t *testing.T) {
		step := gateStep(t, "verify-release", imageStepName)
		out, failed, log, _ := runRegistryStep(t, step, imageEnv(registryStub{}))
		if failed {
			t.Fatalf("the image gate rejected a signed image\n%s", out)
		}
		assertPinnedIdentity(t, log)
		if !logHasDigestVerify(log) {
			t.Errorf("a passing image gate never called digest verify; tag-binding alone is not a signature check")
		}
	})

	t.Run("chart with signatures present still passes", func(t *testing.T) {
		step := gateStep(t, "verify-release", chartStepName)
		out, failed, log, _ := runRegistryStep(t, step, chartEnv(registryStub{}))
		if failed {
			t.Fatalf("the chart gate rejected a signed chart\n%s", out)
		}
		assertPinnedIdentity(t, log)
	})
}

func bundleFiles() []string {
	var out []string
	for _, f := range releaseFiles() {
		if strings.HasSuffix(f, ".sigstore.json") {
			out = append(out, f)
		}
	}
	return out
}

// TestGateRejectsLegacyBundles covers #270's "a legacy-format bundle is
// rejected rather than read" criterion, for both detached attest-blob
// bundles and registry subjects.
func TestGateRejectsLegacyBundles(t *testing.T) {
	step := gateStep(t, "verify-release", assetStepName)

	for _, tc := range []struct{ name, body, want string }{
		{"LocalSignedPayload", legacyBlobBundle, errLegacyBundle},
		{"hybrid mediaType plus legacy fields", hybridBlobBundle, errLegacyBundle},
		{"mediaType only", `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}` + "\n", errLegacyBundle},
	} {
		for _, file := range bundleFiles() {
			t.Run(tc.name+" on "+file, func(t *testing.T) {
				dir := stageRelease(t, "", "")
				path := filepath.Join(dir, "verify", file)
				if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
					t.Fatalf("write %s: %v", file, err)
				}
				out, failed, calls := runAssetGate(t, step, dir, "", true)
				if !failed {
					t.Fatalf("the gate verified a legacy-format blob bundle on %s\n%s", file, out)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("want a legacy-format rejection, got:\n%s", out)
				}
				for _, c := range calls {
					if c.bundle == file {
						t.Errorf("the gate called cosign on the legacy bundle %s; refuse means do not read it", c.bundle)
					}
				}
			})
		}
	}

	t.Run("detached non-JSON bundle", func(t *testing.T) {
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

	t.Run("detached sibling .cyclonedx.sig", func(t *testing.T) {
		dir := stageRelease(t, "", "")
		path := filepath.Join(dir, "verify", darwinARM64+".cyclonedx.sig")
		if err := os.WriteFile(path, []byte("legacy-sig\n"), 0o600); err != nil {
			t.Fatalf("write .cyclonedx.sig: %v", err)
		}
		out, failed, calls := runAssetGate(t, step, dir, "", true)
		if !failed {
			t.Fatalf("the gate accepted a sibling .cyclonedx.sig\n%s", out)
		}
		if !strings.Contains(out, errLegacyDetached) {
			t.Errorf("want a detached .sig rejection, got:\n%s", out)
		}
		want := darwinARM64 + ".cyclonedx.sigstore.json"
		for _, c := range calls {
			if c.bundle == want {
				t.Errorf("the gate called cosign despite %s.cyclonedx.sig; refuse means do not read it", darwinARM64)
			}
		}
	})
}

// TestGateRejectsLegacyRegistryTags is the registry half of the same
// criterion: a sha256-<digest>.sig / .att sidecar is refused without a
// subsequent cosign verify. Each digest is distinct and the stub cases on
// the ref, so deleting any one reject_legacy_registry call goes red.
func TestGateRejectsLegacyRegistryTags(t *testing.T) {
	imageStep := gateStep(t, "verify-release", imageStepName)
	chartStep := gateStep(t, "verify-release", chartStepName)

	t.Run("per-subject crane probe", func(t *testing.T) {
		for _, tc := range []struct {
			name, sidecar, want string
			env                 []string
			step                string
		}{
			{
				name:    "index .sig",
				sidecar: sidecarTag(stubImage, validDigest, "sig"),
				want:    "image index",
				env: imageEnv(registryStub{
					craneLegacy: sidecarTag(stubImage, validDigest, "sig"),
				}),
				step: imageStep,
			},
			{
				name:    "amd64 .sig",
				sidecar: sidecarTag(stubImage, amd64Digest, "sig"),
				want:    "amd64 image",
				env: imageEnv(registryStub{
					craneLegacy: sidecarTag(stubImage, amd64Digest, "sig"),
				}),
				step: imageStep,
			},
			{
				name:    "arm64 .att",
				sidecar: sidecarTag(stubImage, arm64Digest, "att"),
				want:    "arm64 image",
				env: imageEnv(registryStub{
					craneLegacy: sidecarTag(stubImage, arm64Digest, "att"),
				}),
				step: imageStep,
			},
			{
				name:    "mutable tag .sig",
				sidecar: sidecarTag(stubImage, tagAltDigest, "sig"),
				want:    stubImage + ":" + predTag,
				env: imageEnv(registryStub{
					craneLegacy: sidecarTag(stubImage, tagAltDigest, "sig"),
					tagDigest:   tagAltDigest,
				}),
				step: imageStep,
			},
			{
				name:    "chart .sig",
				sidecar: sidecarTag(stubChart, validDigest, "sig"),
				want:    "Helm chart",
				env: chartEnv(registryStub{
					craneLegacy: sidecarTag(stubChart, validDigest, "sig"),
				}),
				step: chartStep,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, failed, log, craneLog := runRegistryStep(t, tc.step, tc.env)
				if !failed {
					t.Fatalf("the gate read a legacy sidecar on %s\n%s", tc.name, out)
				}
				if !strings.Contains(out, errLegacyRegistry) {
					t.Errorf("want a legacy-tag rejection, got:\n%s", out)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("want the error to name %q, got:\n%s", tc.want, out)
				}
				if logHasVerify(log) {
					t.Errorf("the gate called cosign verify after seeing a legacy tag:\n%s", log)
				}
				if !strings.Contains(craneLog, tc.sidecar) {
					t.Errorf("never probed %s:\n%s", tc.sidecar, craneLog)
				}
			})
		}
	})

	t.Run("per-subject tree call", func(t *testing.T) {
		for _, tc := range []struct {
			name, ref, want string
			env             []string
			step            string
		}{
			{
				name: "index",
				ref:  stubImage + "@" + validDigest,
				want: "image index",
				env: imageEnv(registryStub{
					treeFailRefs: stubImage + "@" + validDigest,
				}),
				step: imageStep,
			},
			{
				name: "amd64",
				ref:  stubImage + "@" + amd64Digest,
				want: "amd64 image",
				env: imageEnv(registryStub{
					treeFailRefs: stubImage + "@" + amd64Digest,
				}),
				step: imageStep,
			},
			{
				name: "arm64",
				ref:  stubImage + "@" + arm64Digest,
				want: "arm64 image",
				env: imageEnv(registryStub{
					treeFailRefs: stubImage + "@" + arm64Digest,
				}),
				step: imageStep,
			},
			{
				name: "mutable tag",
				ref:  stubImage + ":" + predTag,
				want: stubImage + ":" + predTag,
				env: imageEnv(registryStub{
					treeFailRefs: stubImage + ":" + predTag,
				}),
				step: imageStep,
			},
			{
				name: "chart",
				ref:  stubChart + "@" + validDigest,
				want: "Helm chart",
				env: chartEnv(registryStub{
					treeFailRefs: stubChart + "@" + validDigest,
				}),
				step: chartStep,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, failed, log, _ := runRegistryStep(t, tc.step, tc.env)
				if !failed {
					t.Fatalf("the gate skipped tree on %s\n%s", tc.name, out)
				}
				if !strings.Contains(out, errInspect) {
					t.Errorf("want an inspect-failure error, got:\n%s", out)
				}
				if !strings.Contains(out, tc.want) {
					t.Errorf("want the error to name %q, got:\n%s", tc.want, out)
				}
				if logHasVerify(log) {
					t.Errorf("the gate called verify without inspecting attachments:\n%s", log)
				}
			})
		}
	})

	t.Run("tree non-zero exit is fail-closed", func(t *testing.T) {
		out, failed, log, _ := runRegistryStep(t, imageStep, imageEnv(registryStub{treeFail: boolTrue}))
		if !failed {
			t.Fatalf("the image gate skipped the format check when tree failed\n%s", out)
		}
		if !strings.Contains(out, errInspect) {
			t.Errorf("want an inspect-failure error, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the image gate called verify without inspecting attachments:\n%s", log)
		}
	})

	t.Run("tree exit-0 empty list is not treated as absence", func(t *testing.T) {
		// Real cosign tree swallows a non-404 on the sidecar fetch, prints
		// this shape, and exits 0. Grep-for-missing-line would pass; the
		// crane probe must still fail-closed when the sidecar is there.
		legacy := sidecarTag(stubImage, amd64Digest, "sig")
		out, failed, log, _ := runRegistryStep(t, imageStep, imageEnv(registryStub{
			tree:        emptyTree,
			craneLegacy: legacy,
		}))
		if !failed {
			t.Fatalf("an empty tree plus a present sidecar was treated as new-format\n%s", out)
		}
		if !strings.Contains(out, errLegacyRegistry) {
			t.Errorf("want a legacy-tag rejection, got:\n%s", out)
		}
		if logHasVerify(log) {
			t.Errorf("the image gate called verify after a fail-open tree:\n%s", log)
		}
	})

	t.Run("non-404 crane error is fail-closed", func(t *testing.T) {
		// Real crane prefixes stderr with the probe ref, so a digest hex
		// containing "404" sits on the same line as INTERNAL_ERROR: 500.
		// Grep for a bare "404" would treat that as absence and fail open.
		for _, tc := range []struct {
			name string
			env  []string
			step string
		}{
			{"image", imageEnv(registryStub{craneError: sidecarTag(stubImage, validDigest, "sig")}), imageStep},
			{"chart", chartEnv(registryStub{craneError: sidecarTag(stubChart, validDigest, "sig")}), chartStep},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, failed, log, _ := runRegistryStep(t, tc.step, tc.env)
				if !failed {
					t.Fatalf("the %s gate treated a registry 5xx as a missing sidecar\n%s", tc.name, out)
				}
				if !strings.Contains(out, errInspect) {
					t.Errorf("want an inspect-failure error, got:\n%s", out)
				}
				if logHasVerify(log) {
					t.Errorf("the %s gate called verify after a crane inspect failure:\n%s", tc.name, log)
				}
			})
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

func stagePublicFixture(t *testing.T, installerBundle, binaryBundle string) string {
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
		"nvcrectl-linux-amd64.sigstore.json": binaryBundle,
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
	return out, failed, readLogFile(t, logPath)
}

// TestPublicRecheckRejectsLegacyBundle drives the post-publish public-channel
// step, which is the other detached attest-blob path the criterion names.
func TestPublicRecheckRejectsLegacyBundle(t *testing.T) {
	step := gateStep(t, "verify-release", publicStep)

	t.Run("protobuf bundles pass", func(t *testing.T) {
		dir := stagePublicFixture(t, protobufBundle, protobufBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if failed {
			t.Fatalf("the public re-check rejected a protobuf bundle\n%s", out)
		}
		if !strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check never verified the downloaded bytes\n%s", out)
		}
		assertPinnedIdentity(t, log)
		if !strings.Contains(log, "nvcrectl-linux-amd64.sigstore.json") {
			t.Errorf("the public re-check never verified the binary bundle; deleting that block still reports the channel good")
		}
	})

	t.Run("legacy installer bundle is refused without calling cosign", func(t *testing.T) {
		dir := stagePublicFixture(t, legacyBlobBundle, protobufBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if !failed {
			t.Fatalf("the public re-check verified a legacy-format installer bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		if strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check called cosign on a legacy bundle:\n%s", log)
		}
	})

	t.Run("legacy binary bundle is refused without calling cosign", func(t *testing.T) {
		dir := stagePublicFixture(t, protobufBundle, legacyBlobBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if !failed {
			t.Fatalf("the public re-check verified a legacy-format binary bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		if strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check called cosign on a legacy bundle:\n%s", log)
		}
	})

	t.Run("hybrid binary bundle is refused without calling cosign", func(t *testing.T) {
		dir := stagePublicFixture(t, protobufBundle, hybridBlobBundle)
		out, failed, log := runPublicStep(t, step, dir)
		if !failed {
			t.Fatalf("the public re-check verified a hybrid-format binary bundle\n%s", out)
		}
		if !strings.Contains(out, errLegacyBundle) {
			t.Errorf("want a legacy-format rejection, got:\n%s", out)
		}
		if strings.Contains(log, "verify-blob-attestation") {
			t.Errorf("the public re-check called cosign on a hybrid bundle:\n%s", log)
		}
	})
}
