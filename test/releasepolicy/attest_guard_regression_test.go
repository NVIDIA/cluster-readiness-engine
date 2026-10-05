// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"strings"
	"testing"
)

const (
	jobSign         = "sign"
	jobWrap         = "wrap"
	selfWrapperUses = "$/.github/workflows/wrapper.yml"
)

// TestReleaseTagRefRecognitionMutations exercises the recognizer against valid
// shell that mentions the rejection without enforcing it (#351).
func TestReleaseTagRefRecognitionMutations(t *testing.T) {
	script, _ := releaseTagResolveStep(t)
	start := strings.Index(script, exactReleaseTagRefCheck)
	if start < 0 {
		t.Fatal("Resolve tag has no ref comparison")
	}
	offset := strings.Index(script[start:], "exit 1")
	if offset < 0 {
		t.Fatal("Resolve tag has no mismatch rejection")
	}
	offset += start
	cases := []struct {
		name        string
		replacement string
		want        bool
	}{
		{"original", "exit 1", true},
		{"trailing comment", "exit 1 # reject the mismatched ref", true},
		{"stubbed", ":", false},
		{"commented exit", ": # exit 1", false},
		{"quoted exit", "printf 'exit 1\\n'", false},
		{"else branch", ":\n            else\n              exit 1", false},
		{"unreachable exit", "if false; then exit 1; fi", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := script[:offset] + tc.replacement + script[offset+len("exit 1"):]
			if got := runHasEffectiveReleaseTagRefCheck(t, mutated); got != tc.want {
				t.Errorf("recognizes effective ref guard = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAttestWrapperReachability pins dispatch-to-wrapper-to-attest paths without
// changing the real workflows or dispatching a signing job.
func TestAttestWrapperReachability(t *testing.T) {
	workflows := map[string]map[string]policyJob{
		"wrapper.yml":     {jobSign: {Uses: localAttestUses}},
		"outer.yaml":      {jobWrap: {Uses: "./.github/workflows/wrapper.yml"}},
		"plain.yml":       {"build": {}},
		"self-outer.yaml": {jobWrap: {Uses: selfWrapperUses}},
		"cycle-a.yml":     {"next": {Uses: "./.github/workflows/cycle-b.yml"}},
		"cycle-b.yml":     {"next": {Uses: "./.github/workflows/cycle-a.yml"}},
		"cycle-sign.yml": {
			"cycle": {Uses: "./.github/workflows/cycle-sign.yml"},
			jobSign: {Uses: localAttestUses},
		},
	}
	cases := []struct {
		name, uses string
		want       bool
	}{
		{"direct", localAttestUses, true},
		{"one wrapper", "./.github/workflows/wrapper.yml", true},
		{"self-repository wrapper", selfWrapperUses, true},
		{"two self-repository wrappers", "$/.github/workflows/self-outer.yaml", true},
		{"mixed wrapper prefixes", "./.github/workflows/self-outer.yaml", true},
		{"two wrappers and yaml extension", "./.github/workflows/outer.yaml", true},
		{"ordinary workflow", "./.github/workflows/plain.yml", false},
		{"no reusable call", "", false},
		{"cycle without attest", "./.github/workflows/cycle-a.yml", false},
		{"cycle with attest", "./.github/workflows/cycle-sign.yml", true},
		{"remote wrapper is not the local file", "other/repo/.github/workflows/wrapper.yml@main", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := callsAttestThroughLocalWorkflows(workflows, tc.uses); got != tc.want {
				t.Errorf("reaches attest = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadedReleaseTagGuardMustBeMandatory prevents a skipped Resolve tag step
// or a swallowed failure from being credited as a shell guard.
func TestLoadedReleaseTagGuardMustBeMandatory(t *testing.T) {
	script, _ := releaseTagResolveStep(t)
	cases := []struct {
		name, workflow, stepExtra string
		want                      bool
	}{
		{"mandatory", wfRelease, "", true},
		{"conditional", wfRelease, "if: false", false},
		{"continue on error", wfRelease, "continue-on-error: true", false},
		{"expression continue on error", wfRelease, "continue-on-error: ${{ true }}", false},
		{"different workflow", "other.yml", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "jobs:\n  release-tag:\n    steps:\n      - name: Resolve tag\n"
			if tc.stepExtra != "" {
				raw += "        " + tc.stepExtra + "\n"
			}
			raw += "        run: |\n" + "          " + strings.ReplaceAll(strings.TrimSpace(script), "\n", "\n          ")
			jobs := loadJobsWithNeeds(t, []byte(raw), tc.workflow)
			if got := jobOrAncestorHasRefGuard(t, jobs, jobReleaseTag); got != tc.want {
				t.Errorf("loaded guard recognized = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWrappedAttestCallersRequireRefGuards exercises the same caller enumeration
// as the real workflow policy, with both unsafe and properly gated wrappers.
func TestWrappedAttestCallersRequireRefGuards(t *testing.T) {
	const wrapper = "./.github/workflows/wrapper.yml"
	workflows := map[string]map[string]policyJob{
		"wrapper.yml":         {jobSign: {Uses: localAttestUses}},
		"outer.yaml":          {jobWrap: {Uses: wrapper}},
		"plain.yml":           {"build": {}},
		"self-outer.yaml":     {jobWrap: {Uses: selfWrapperUses}},
		"guarded-wrapper.yml": {jobSign: {Uses: localAttestUses, If: exactRepoAndMainIf}},
		"guarded-needs.yml": {
			jobGuarded: {If: exactRepoAndMainIf},
			jobSign:    {Uses: localAttestUses, Needs: []string{jobGuarded}},
		},
		"unrelated-guard.yml": {
			jobGuarded: {If: exactRepoAndMainIf},
			jobSign:    {Uses: localAttestUses},
		},
		"cancelled-bypass.yml": {
			jobGuarded: {If: exactRepoAndMainIf},
			jobSign:    {Uses: localAttestUses, Needs: []string{jobGuarded}, If: notCancelledIf},
		},
		"partly-guarded.yml": {
			jobSign:  {Uses: localAttestUses, If: exactRepoAndMainIf},
			"resign": {Uses: localAttestUses},
		},
		"shared-wrapper.yml": {
			"guarded-hop": {Uses: wrapper, If: exactRepoAndMainIf},
			"open-hop":    {Uses: wrapper},
		},
		"guarded-outer.yaml": {jobWrap: {Uses: wrapper, If: exactRepoAndMainIf}},
		"outer-guarded.yaml": {jobWrap: {Uses: "./.github/workflows/guarded-wrapper.yml"}},
	}
	cases := []struct {
		name   string
		caller policyJob
		guard  policyJob
		want   int
	}{
		{"unguarded wrapper", policyJob{Uses: wrapper}, policyJob{}, 1},
		{"unguarded self-repository wrapper", policyJob{Uses: selfWrapperUses}, policyJob{}, 1},
		{"two self-repository wrappers", policyJob{Uses: "$/.github/workflows/self-outer.yaml"}, policyJob{}, 1},
		{"guarded self-repository wrapper", policyJob{Uses: selfWrapperUses, If: exactRepoAndMainIf}, policyJob{}, 0},
		{"two wrappers", policyJob{Uses: "./.github/workflows/outer.yaml"}, policyJob{}, 1},
		{"guarded caller", policyJob{Uses: wrapper, If: exactRepoAndMainIf}, policyJob{}, 0},
		{"guarded ancestor", policyJob{Uses: wrapper, Needs: []string{jobGuarded}},
			policyJob{If: exactRepoAndMainIf}, 0},
		{"unrelated guard", policyJob{Uses: wrapper}, policyJob{If: exactRepoAndMainIf}, 1},
		{"cancelled bypass", policyJob{Uses: wrapper, Needs: []string{jobGuarded}, If: notCancelledIf},
			policyJob{If: exactRepoAndMainIf}, 1},
		{"no attest path", policyJob{Uses: "./.github/workflows/plain.yml"}, policyJob{}, 0},
		{"guard inside the wrapper", policyJob{Uses: "./.github/workflows/guarded-wrapper.yml"}, policyJob{}, 0},
		{"guard inside a self-repository wrapper", policyJob{Uses: "$/.github/workflows/guarded-wrapper.yml"},
			policyJob{}, 0},
		{"guarded ancestor inside the wrapper", policyJob{Uses: "./.github/workflows/guarded-needs.yml"},
			policyJob{}, 0},
		{"unrelated guard inside the wrapper", policyJob{Uses: "./.github/workflows/unrelated-guard.yml"},
			policyJob{}, 1},
		{"cancelled bypass inside the wrapper", policyJob{Uses: "./.github/workflows/cancelled-bypass.yml"},
			policyJob{}, 1},
		{"one unguarded path inside the wrapper", policyJob{Uses: "./.github/workflows/partly-guarded.yml"},
			policyJob{}, 1},
		{"guarded and unguarded hops share a wrapper", policyJob{Uses: "./.github/workflows/shared-wrapper.yml"},
			policyJob{}, 1},
		{"guard on the middle wrapper", policyJob{Uses: "./.github/workflows/guarded-outer.yaml"}, policyJob{}, 0},
		{"guard on the inner wrapper", policyJob{Uses: "./.github/workflows/outer-guarded.yaml"}, policyJob{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := map[string]policyJob{jobCaller: tc.caller, jobGuarded: tc.guard}
			got := unguardedAttestCallers(t, workflows, jobs)
			if len(got) != tc.want || (len(got) == 1 && got[0] != jobCaller) {
				t.Errorf("unguarded callers = %v, want %d caller", got, tc.want)
			}
		})
	}
}
