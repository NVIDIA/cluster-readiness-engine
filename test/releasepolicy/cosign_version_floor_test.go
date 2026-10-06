// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"testing"
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
//
// The trigger block is not unmarshalled. YAML 1.1 reads a bare `on:` key as
// boolean true, which is why other tests in this package parse that block
// by hand.
func attestCosignVersionDefault(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(attestWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", attestWorkflow, err)
	}

	const inputKey = "      cosign_version:"
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if line != inputKey {
			continue
		}
		for _, inner := range lines[i+1:] {
			// Six-space indent is the next sibling key (crane_version, etc.).
			if strings.HasPrefix(inner, "      ") && !strings.HasPrefix(inner, "       ") {
				break
			}
			trim := strings.TrimSpace(inner)
			if !strings.HasPrefix(trim, "default:") {
				continue
			}
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trim, "default:")), `"'`)
			if v == "" {
				t.Fatalf("%s cosign_version default is empty", attestWorkflow)
			}
			return v
		}
		break
	}
	t.Fatalf("%s is missing a cosign_version workflow_call default", attestWorkflow)
	return ""
}

// stripCosignVersionFloor removes the numeric comparison that follows the
// vMAJOR.MINOR.PATCH regex, leaving the regex in place. Used to prove that
// the regex alone still accepts lastAffectedCosignV3.
func stripCosignVersionFloor(script string) string {
	const startNeedle = "# The regex accepts any vMAJOR.MINOR.PATCH"
	const endNeedle = `[[ "${IN_CRANE_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]`
	start := strings.Index(script, startNeedle)
	end := strings.Index(script, endNeedle)
	if start < 0 || end < 0 || end <= start {
		return script
	}
	return script[:start] + script[end:]
}

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

func parseVSemver(s string) (maj, min, pat int, err error) {
	n, scanErr := fmt.Sscanf(s, "v%d.%d.%d", &maj, &min, &pat)
	if scanErr != nil || n != 3 {
		return 0, 0, 0, fmt.Errorf("not vMAJOR.MINOR.PATCH: %q", s)
	}
	if fmt.Sprintf("v%d.%d.%d", maj, min, pat) != s {
		return 0, 0, 0, fmt.Errorf("not vMAJOR.MINOR.PATCH: %q", s)
	}
	return maj, min, pat, nil
}
