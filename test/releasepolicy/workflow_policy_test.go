// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// makefilePath and attestWorkflowName are local to this file; releaseWorkflow
// and workflowFiles are already defined elsewhere in this package (see
// predicate_parse_test.go and shell_scope_test.go) and are reused as-is.
const (
	makefilePath       = "../../Makefile"
	attestWorkflowName = "attest.yml"
)

// cosignSignCmds are the signing invocations that may exist only inside
// attest.yml. A second home for any of them widens the published identity
// contract: consumers pin one workflow path, so a signer elsewhere is accepted
// under a different SAN with no change to the pin.
var cosignSignCmds = regexp.MustCompile(`(?m)(?:^|[\s;|&])(?:retry\s+)?cosign\s+(sign|attest|attest-blob)\b`)

var attestProvenanceAction = regexp.MustCompile(`(^|/)actions/attest-build-provenance(@|$)`)

// shaRef is a full 40-character commit SHA. Tags, branches, and short SHAs are
// all mutable (or mutable-enough) references and fail the pin check.
var shaRef = regexp.MustCompile(`^[0-9a-f]{40}$`)

// nvcrectlBinaryName matches a published CLI binary name, not an SBOM beside it.
var nvcrectlBinaryName = regexp.MustCompile(`\bnvcrectl-([a-z0-9]+)-([a-z0-9]+)\b`)

// TestAttestIsSoleSigner keeps the published certificate identity contract true.
//
// Consumers pin
//
//	.../attest.yml@refs/tags/<TAG>
//
// so any second workflow that invokes cosign sign/attest/attest-blob (or
// actions/attest-build-provenance) silently widens what that pin accepts.
// attest.yml itself must stay workflow_call-only: any other trigger makes the
// signing identity reachable from a branch push or a dispatch.
func TestAttestIsSoleSigner(t *testing.T) {
	assertAttestIsWorkflowCallOnly(t)

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

// compositeActionFiles returns every local composite action.yml. Same glob as
// TestActionsAreSHAPinned so a new action cannot escape one check while
// remaining visible to the other.
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

// TestIDTokenWriteOnlyOnSigningJobs pins the OIDC credential to jobs that
// actually sign (or that call the reusable signer). A job that does not sign
// has no business minting a Fulcio-bound token.
//
// "Signing" here means: the job calls attest.yml, or it is a job whose steps
// run cosign sign/attest/attest-blob (today only attest.yml's attest job).
func TestIDTokenWriteOnlyOnSigningJobs(t *testing.T) {
	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		var doc struct {
			Permissions ghPermissions `json:"permissions"`
			Jobs        map[string]struct {
				Uses        string        `json:"uses"`
				Permissions ghPermissions `json:"permissions"`
				Steps       []policyStep  `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for jobName, job := range doc.Jobs {
			signs := jobSigns(job.Uses, job.Steps)
			// Job-level permissions fully replace workflow-level when present
			// (including permissions: {}); otherwise the job inherits.
			effective := doc.Permissions
			if job.Permissions.set {
				effective = job.Permissions
			}

			if signs {
				if !effective.idTokenWrite {
					t.Errorf("%s: signing job %q must have effective id-token: write "+
						"(workflow-level or job-level)",
						base, jobName)
				}
				continue
			}

			if effective.idTokenWrite {
				t.Errorf("%s: non-signing job %q has effective id-token: write; only jobs that sign "+
					"or call attest.yml may mint an OIDC token",
					base, jobName)
			}
		}
	}
}

// ghPermissions accepts the map form and the scalar read-all/write-all forms.
// write-all grants id-token write; read-all does not. set is true when the
// permissions key was present, so callers can distinguish omit from {}.
type ghPermissions struct {
	set          bool
	idTokenWrite bool
}

func (p *ghPermissions) UnmarshalJSON(b []byte) error {
	p.set = true

	var scalar string
	if err := yaml.Unmarshal(b, &scalar); err == nil {
		p.idTokenWrite = scalar == "write-all"
		return nil
	}

	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err != nil {
		return err
	}
	p.idTokenWrite = m["id-token"] == "write"
	return nil
}

func jobSigns(uses string, steps []policyStep) bool {
	if isAttestWorkflowCall(uses) {
		return true
	}
	for _, step := range steps {
		if cosignSignCmds.MatchString(step.Run) {
			return true
		}
		if attestProvenanceAction.MatchString(step.Uses) {
			return true
		}
	}
	return false
}

func isAttestWorkflowCall(uses string) bool {
	if uses == "" {
		return false
	}
	// Local reusable-workflow calls look like ./.github/workflows/attest.yml;
	// ignore a ref suffix if one is ever added.
	uses = strings.SplitN(uses, "@", 2)[0]
	return filepath.Base(uses) == attestWorkflowName
}

// TestActionsAreSHAPinned rejects mutable tags on every non-local `uses:`.
// A `@v4` that moves is an unreviewed change to whatever path that action is
// on — including the release path.
func TestActionsAreSHAPinned(t *testing.T) {
	paths := append([]string{}, workflowFiles(t)...)
	paths = append(paths, compositeActionFiles(t)...)

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, uses := range collectUses(raw, t) {
			if strings.HasPrefix(uses, "./") {
				continue
			}
			at := strings.LastIndex(uses, "@")
			if at < 0 {
				t.Errorf("%s: uses %q has no ref; pin to a 40-character commit SHA", relGithub(path), uses)
				continue
			}
			ref := uses[at+1:]
			if !shaRef.MatchString(ref) {
				t.Errorf("%s: uses %q is not pinned to a 40-character commit SHA", relGithub(path), uses)
			}
		}
	}
}

func collectUses(raw []byte, t *testing.T) []string {
	t.Helper()

	var out []string
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse for uses: %v", err)
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if u, ok := x["uses"].(string); ok && u != "" {
				out = append(out, u)
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}

func relGithub(path string) string {
	path = filepath.ToSlash(path)
	if i := strings.Index(path, ".github/"); i >= 0 {
		return path[i:]
	}
	return path
}

// TestExpectedAssetListsMatchNVCRECTLPlatforms is the test ADR-074 names.
//
// Assets are built from NVCRECTL_PLATFORMS and staged by glob, but four
// independent hardcoded lists decide what gets signed, what is checked before
// publish, and what the release-time gate expects. Adding a platform to the
// Makefile without updating every list would ship an unsigned binary with
// every gate still green.
func TestExpectedAssetListsMatchNVCRECTLPlatforms(t *testing.T) {
	want := nvcrectlBinariesFromMakefile(t)
	if len(want) == 0 {
		t.Fatal("NVCRECTL_PLATFORMS produced no binaries; the Makefile parse is broken")
	}

	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflow, err)
	}
	text := string(raw)

	lists := []struct {
		name string
		got  []string
	}{
		{"attest-binaries matrix", attestBinariesMatrixSubjects(text, t)},
		{"digest map", digestMapSubjects(text, t)},
		{"pre-publish bundle check", prePublishBundleSubjects(text, t)},
		{"release verification gate (#270)", releaseGateBinaries(text, t)},
	}

	wantSet := uniqueSorted(want)
	for _, list := range lists {
		gotSet := uniqueSorted(list.got)
		if !slices.Equal(gotSet, wantSet) {
			t.Errorf("%s binaries = %v, want %v (from NVCRECTL_PLATFORMS); adding a platform "+
				"requires updating every hardcoded asset list",
				list.name, gotSet, wantSet)
		}
	}
}

func nvcrectlBinariesFromMakefile(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	// Prefer the `?=` assignment that defines the default platform list.
	re := regexp.MustCompile(`(?m)^NVCRECTL_PLATFORMS\s*\?=\s*(.+)$`)
	m := re.FindSubmatch(raw)
	if m == nil {
		re = regexp.MustCompile(`(?m)^NVCRECTL_PLATFORMS\s*=\s*(.+)$`)
		m = re.FindSubmatch(raw)
	}
	if m == nil {
		t.Fatal("Makefile has no NVCRECTL_PLATFORMS assignment")
	}

	var out []string
	for p := range strings.SplitSeq(strings.TrimSpace(string(m[1])), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		osArch := strings.SplitN(p, "/", 2)
		if len(osArch) != 2 || osArch[0] == "" || osArch[1] == "" {
			t.Fatalf("NVCRECTL_PLATFORMS entry %q is not os/arch", p)
		}
		out = append(out, "nvcrectl-"+osArch[0]+"-"+osArch[1])
	}
	return out
}

func attestBinariesMatrixSubjects(text string, t *testing.T) []string {
	t.Helper()

	// Limit to the attest-binaries job so image/chart matrices cannot pollute.
	job := jobBody(text, "attest-binaries", t)
	matches := regexp.MustCompile(`(?m)^\s*-\s*subject:\s*(nvcrectl-[a-z0-9]+-[a-z0-9]+)\s*$`).
		FindAllStringSubmatch(job, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatal("attest-binaries matrix has no nvcrectl-* subjects")
	}
	return out
}

func digestMapSubjects(text string, t *testing.T) []string {
	t.Helper()

	block := forLoopBlock(text, regexp.MustCompile(`(?m)^\s*for want in `), t, "digest map")
	return nvcrectlBinariesIn(block, t, "digest map")
}

func prePublishBundleSubjects(text string, t *testing.T) []string {
	t.Helper()

	// The pre-publish check enumerates concrete asset names. The earlier
	// `for f in release-assets/nvcrectl-*` glob that builds SBOMs is not a list.
	block := forLoopBlock(text, regexp.MustCompile(`(?m)^\s*for f in nvcrectl-`), t, "pre-publish bundle check")
	return nvcrectlBinariesIn(block, t, "pre-publish bundle check")
}

// releaseGateBinaries returns the #270 gate's BINARIES= list. ADR-074 requires
// a static expected-asset list in the post-publish gate; without it, deleting
// an asset produces a clean run over the survivors.
func releaseGateBinaries(text string, t *testing.T) []string {
	t.Helper()

	re := regexp.MustCompile(`(?m)^\s*BINARIES="([^"]+)"\s*$`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		t.Fatal("release.yml post-publish gate has no BINARIES=\"...\" expected-asset list (ADR-074 / #270)")
	}
	var out []string
	for tok := range strings.FieldsSeq(m[1]) {
		if regexp.MustCompile(`^nvcrectl-[a-z0-9]+-[a-z0-9]+$`).MatchString(tok) {
			out = append(out, tok)
		}
	}
	if len(out) == 0 {
		t.Fatal("release.yml BINARIES= list contains no nvcrectl-* binaries")
	}
	return out
}

func jobBody(text, jobName string, t *testing.T) string {
	t.Helper()

	// Jobs live two spaces under `jobs:`.
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(jobName) + `:\s*$`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		t.Fatalf("release.yml has no %q job", jobName)
	}
	rest := text[loc[1]:]
	// Next sibling job key at the same indent.
	next := regexp.MustCompile(`(?m)^  [a-zA-Z0-9_-]+:\s*$`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

func forLoopBlock(text string, start *regexp.Regexp, t *testing.T, name string) string {
	t.Helper()

	loc := start.FindStringIndex(text)
	if loc == nil {
		t.Fatalf("release.yml has no %s for-loop", name)
	}
	rest := text[loc[0]:]
	// The list is written as a line-continued `for x in a b \` block ending at `do`.
	end := regexp.MustCompile(`(?m)\bdo\b`).FindStringIndex(rest)
	if end == nil {
		t.Fatalf("%s for-loop has no `do`", name)
	}
	return rest[:end[0]]
}

func nvcrectlBinariesIn(block string, t *testing.T, name string) []string {
	t.Helper()

	// Word-boundary matching pulls `nvcrectl-os-arch` out of both the bare
	// binary token and the `nvcrectl-os-arch.cyclonedx.json` form; uniqueSorted
	// collapses the duplicates.
	seen := map[string]bool{}
	var out []string
	for _, m := range nvcrectlBinaryName.FindAllStringSubmatch(block, -1) {
		bin := m[0]
		if !seen[bin] {
			seen[bin] = true
			out = append(out, bin)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s lists no nvcrectl-* binaries", name)
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
