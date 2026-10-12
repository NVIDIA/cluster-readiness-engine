// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestAttestCosignVersionFloor pins the GHSA-fx35-mq7g-6g98 floor on
// attest.yml's cosign_version input.
//
// The validate step's vMAJOR.MINOR.PATCH regex accepts v3.1.2, which the
// advisory lists as affected. ndipebot called that out on #369 (1 Oct 2026):
// a follow-up must refuse well-formed pins below the first fixed v3 release.
// These cases fail if that comparison is removed or if the bound is lowered
// far enough to admit lastAffectedCosignV3.
func TestAttestCosignVersionFloor(t *testing.T) {
	script := validateScript(t)

	if !strings.Contains(script, minCosignVersionGHSA) {
		t.Fatalf("validate step does not mention %s; a regex-only check accepts "+
			"GHSA-fx35-mq7g-6g98-affected pins such as %s",
			minCosignVersionGHSA, lastAffectedCosignV3)
	}

	defaultVer := attestCosignVersionDefault(t)
	if order, err := compareVSemver(defaultVer, minCosignVersionGHSA); err != nil {
		t.Fatalf("attest.yml default cosign_version %q: %v", defaultVer, err)
	} else if order < 0 {
		t.Errorf("attest.yml default cosign_version %s is below the GHSA-fx35-mq7g-6g98 floor %s",
			defaultVer, minCosignVersionGHSA)
	}

	t.Run("rejects pins below the floor", func(t *testing.T) {
		below := []string{
			lastAffectedCosignV3, // last affected v3 release
			"v3.1.1",
			"v3.1.0",
			"v3.0.6",
			"v2.6.5", // patched on the v2 line; this workflow pins v3
			"v2.6.4",
			"v1.0.0",
			"v0.0.0",
		}
		for _, ver := range below {
			ok, out := runValidate(t, script, defaultInputs().with(inputs{inCosignVersion: ver}))
			if ok {
				t.Errorf("%s: validation accepted a pin below the GHSA floor %s",
					ver, minCosignVersionGHSA)
				continue
			}
			if !strings.Contains(out, errCosignBelowFloor) {
				t.Errorf("%s: rejected, but not by the version floor.\nwant error containing: %q\ngot:\n%s",
					ver, errCosignBelowFloor, out)
			}
		}
	})

	t.Run("accepts the floor and newer", func(t *testing.T) {
		above := []string{
			minCosignVersionGHSA,
			"v3.1.4",
			// Multi-digit components: a lexical compare sorts these below
			// v3.1.3 and would reject a legitimate future pin.
			"v3.1.10",
			"v3.10.0",
			"v10.0.0",
			"v3.2.0",
			"v4.0.0",
			defaultVer,
		}
		for _, ver := range above {
			ok, out := runValidate(t, script, defaultInputs().with(inputs{inCosignVersion: ver}))
			if !ok {
				t.Errorf("%s: validation rejected a pin at or above the GHSA floor %s:\n%s",
					ver, minCosignVersionGHSA, out)
			}
		}
	})

	t.Run("regex alone is not the floor", func(t *testing.T) {
		stripped := stripCosignVersionFloor(script)
		if stripped == script {
			t.Fatal("could not strip the numeric floor from the validate step; " +
				"the comparison must remain a distinct check after the vX.Y.Z regex")
		}
		ok, _ := runValidate(t, stripped, defaultInputs().with(inputs{
			inCosignVersion: lastAffectedCosignV3,
		}))
		if !ok {
			t.Fatalf("stripped script still rejected %s; the mutation must remove "+
				"the floor so this case proves the regex is not doing that work",
				lastAffectedCosignV3)
		}
		ok, out := runValidate(t, script, defaultInputs().with(inputs{
			inCosignVersion: lastAffectedCosignV3,
		}))
		if ok {
			t.Fatalf("unmutated script accepted %s; the floor is not rejecting the last affected v3 pin",
				lastAffectedCosignV3)
		}
		if !strings.Contains(out, errCosignBelowFloor) {
			t.Errorf("unmutated script rejected %s, but not by the version floor:\n%s",
				lastAffectedCosignV3, out)
		}
	})
}

// attestCosignVersionDefault returns the workflow_call input default from
// attest.yml. Reading it from the file is the point: a copy here would not
// notice the default dropping below the GHSA floor.
func attestCosignVersionDefault(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(attestWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflow, err)
	}
	return cosignVersionDefault(t, raw)
}

// cosignVersionDefault unmarshals the workflow and returns
// on.workflow_call.inputs.cosign_version.default. workflowTriggers handles the
// YAML 1.1 bare `on:` key. Parsing the YAML rather than scanning lines means a
// `default:` written inside the description cannot stand in for the real one.
func cosignVersionDefault(t *testing.T, raw []byte) string {
	t.Helper()

	call, ok := workflowTriggers(raw, t)["workflow_call"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no workflow_call trigger block", attestWorkflow)
	}
	inputs, _ := call["inputs"].(map[string]any)
	input, ok := inputs["cosign_version"].(map[string]any)
	if !ok {
		t.Fatalf("%s is missing the cosign_version workflow_call input", attestWorkflow)
	}
	v, ok := input["default"].(string)
	if !ok || v == "" {
		t.Fatalf("%s cosign_version has no string default (got %#v)", attestWorkflow, input["default"])
	}
	return v
}

// TestCosignVersionDefaultIgnoresDescription keeps the default reader on the
// parsed YAML. The description is a folded scalar, so a continuation line can
// start with `default:`; a line scanner that reaches it first would report
// v4.0.0 here and let a real default of v3.1.2 pass the floor.
func TestCosignVersionDefaultIgnoresDescription(t *testing.T) {
	const poisoned = `on:
  workflow_call:
    inputs:
      cosign_version:
        description: >-
          Pinned cosign version.
          default: v4.0.0
        required: false
        type: string
        default: 'v3.1.2'
`
	if got := cosignVersionDefault(t, []byte(poisoned)); got != lastAffectedCosignV3 {
		t.Fatalf("cosignVersionDefault = %q, want %q (the YAML default, not the description text)",
			got, lastAffectedCosignV3)
	}
}

// cosignInstaller matches the sigstore/cosign-installer action reference.
var cosignInstaller = regexp.MustCompile(`(^|/)sigstore/cosign-installer(@|$)`)

// envRef is a whole-value `${{ env.NAME }}` expression.
var envRef = regexp.MustCompile(`^\$\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)

// attestValidatedCosignRelease is the one non-literal cosign-release allowed:
// attest.yml installs the version its validate job already held to the floor.
const attestValidatedCosignRelease = "${{ needs.validate.outputs.cosign_version }}"

// TestCosignInstallPinsMeetGHSAFloor holds every cosign-installer pin in the
// workflows to the GHSA-fx35-mq7g-6g98 floor, not just attest.yml's input.
// release.yml's verify-release job and docs-verify.yml both run cosign
// verification against published artifacts; lowering either pin alone to
// v3.1.2 must fail here.
func TestCosignInstallPinsMeetGHSAFloor(t *testing.T) {
	type envMap map[string]any
	type step struct {
		Name string         `json:"name"`
		Uses string         `json:"uses"`
		Env  envMap         `json:"env"`
		With map[string]any `json:"with"`
	}
	var checked = map[string]int{}

	for _, path := range workflowFiles(t) {
		base := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc struct {
			Env  envMap `json:"env"`
			Jobs map[string]struct {
				Env   envMap `json:"env"`
				Steps []step `json:"steps"`
			} `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for jobName, job := range doc.Jobs {
			for _, st := range job.Steps {
				if !cosignInstaller.MatchString(st.Uses) {
					continue
				}
				where := fmt.Sprintf("%s job %q step %q", base, jobName, st.Name)
				release, ok := st.With["cosign-release"].(string)
				if !ok || strings.TrimSpace(release) == "" {
					t.Errorf("%s: cosign-installer has no cosign-release pin; the installer default "+
						"is not held to the GHSA floor %s", where, minCosignVersionGHSA)
					continue
				}
				release = strings.TrimSpace(release)
				if base == attestWorkflowName && release == attestValidatedCosignRelease {
					continue // floored by attest.yml's validate step
				}
				if m := envRef.FindStringSubmatch(release); m != nil {
					resolved, found := "", false
					for _, scope := range []envMap{st.Env, job.Env, doc.Env} {
						if v, ok := scope[m[1]]; ok {
							resolved, found = fmt.Sprint(v), true
							break
						}
					}
					if !found {
						t.Errorf("%s: cosign-release %s does not resolve to a step, job or workflow env value",
							where, release)
						continue
					}
					release = resolved
				}
				checked[base]++
				order, err := compareVSemver(release, minCosignVersionGHSA)
				if err != nil {
					t.Errorf("%s: cosign-release %q must be a literal vMAJOR.MINOR.PATCH so the "+
						"GHSA floor can be checked: %v", where, release, err)
					continue
				}
				if order < 0 {
					t.Errorf("%s: cosign-release %s is below the GHSA-fx35-mq7g-6g98 floor %s",
						where, release, minCosignVersionGHSA)
				}
			}
		}
	}

	// Guard against the scan silently matching nothing: these two workflows
	// verify published artifacts and must keep a pin this test can check.
	for _, want := range []string{"release.yml", "docs-verify.yml"} {
		if checked[want] == 0 {
			t.Errorf("%s: no cosign-installer pin was found to check against the GHSA floor", want)
		}
	}
}

// stripCosignVersionFloor removes the numeric comparison that follows the
// vMAJOR.MINOR.PATCH regex, leaving the regex in place. Used to prove that
// the regex alone still accepts lastAffectedCosignV3. Both needles are code,
// so rewording the comments in the validate step does not break the mutation.
func stripCosignVersionFloor(script string) string {
	const startNeedle = `cosign_rest="${IN_COSIGN_VERSION#v}"`
	const endNeedle = `[[ "${IN_CRANE_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]`
	start := strings.Index(script, startNeedle)
	end := strings.Index(script, endNeedle)
	if start < 0 || end < 0 || end <= start {
		return script
	}
	return script[:start] + script[end:]
}

// vSemver is vMAJOR.MINOR.PATCH with non-negative decimal components and no
// leading zeros or signs. Matching the shape first means Sscanf-style quirks
// (a minus sign read by %d, for one) cannot produce a component.
var vSemver = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// compareVSemver orders two vMAJOR.MINOR.PATCH strings numerically.
func compareVSemver(a, b string) (int, error) {
	am, ai, ap, err := parseVSemver(a)
	if err != nil {
		return 0, err
	}
	bm, bi, bp, err := parseVSemver(b)
	if err != nil {
		return 0, err
	}
	switch {
	case am != bm:
		return cmp.Compare(am, bm), nil
	case ai != bi:
		return cmp.Compare(ai, bi), nil
	default:
		return cmp.Compare(ap, bp), nil
	}
}

// parseVSemver splits a vMAJOR.MINOR.PATCH string into its three components.
func parseVSemver(s string) (maj, min, pat int, err error) {
	m := vSemver.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("not vMAJOR.MINOR.PATCH: %q", s)
	}
	var parts [3]int
	for i, field := range m[1:] {
		n, convErr := strconv.Atoi(field)
		if convErr != nil {
			return 0, 0, 0, fmt.Errorf("not vMAJOR.MINOR.PATCH: %q: %w", s, convErr)
		}
		parts[i] = n
	}
	return parts[0], parts[1], parts[2], nil
}

// TestParseVSemverRejectsMalformed pins the parser the floor checks rely on.
func TestParseVSemverRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"v3.2.-1",
		"v-3.1.3",
		"v3.-1.4",
		"v+3.1.3",
		"v03.1.3",
		"3.1.3",
		"v3.1",
		"v3.1.3-rc.1",
		"v3.1.3 ",
		"v99999999999999999999.0.0",
	} {
		if _, _, _, err := parseVSemver(s); err == nil {
			t.Errorf("parseVSemver(%q) accepted a malformed version", s)
		}
	}
	if order, err := compareVSemver("v3.2.-1", minCosignVersionGHSA); err == nil {
		t.Errorf("compareVSemver(%q, %q) = %d with no error; a negative component must be refused",
			"v3.2.-1", minCosignVersionGHSA, order)
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v3.1.10", minCosignVersionGHSA, 1},
		{"v3.10.0", "v3.2.0", 1},
		{"v10.0.0", "v9.9.9", 1},
		{lastAffectedCosignV3, minCosignVersionGHSA, -1},
		{minCosignVersionGHSA, minCosignVersionGHSA, 0},
	} {
		got, err := compareVSemver(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Errorf("compareVSemver(%q, %q) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
}
