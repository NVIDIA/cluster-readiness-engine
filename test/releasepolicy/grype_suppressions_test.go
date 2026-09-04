// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// grypeConfig is grype's configuration for the weekly image scan. It is NOT the
// suppression file -- see openVEXDoc.
const grypeConfig = "../../.grype.yaml"

// vulnScanWorkflow is the weekly image scan that consumes both files.
const vulnScanWorkflow = "../../.github/workflows/vuln-scan-images.yml"

// TestGrypeConfigCarriesNoSuppressions keeps suppressions in one place.
//
// Grype will happily apply ignore rules from .grype.yaml and VEX statements from
// .openvex.json in the same run. Allowing both means the impact analysis for a
// CVE can live in either file, so answering "why is this not reported?" requires
// checking two -- and only one of them is covered by
// TestOpenVEXStatementsAreTriageable, so a suppression written in the other gets
// no product-PURL check, no justification enum, no impact statement, and nothing
// bringing it back for re-triage. The weaker mechanism would be the easier one
// to reach for, because it is three lines of YAML.
// grypeConfigAllowedKeys is what .grype.yaml may contain.
//
// An allowlist rather than a ban on `ignore:`, because `ignore:` is not the only
// grype key that removes findings -- `exclude:` drops path globs from the scan
// entirely, so packages under them never produce a match at all, which is a
// broader and less visible suppression than any ignore rule. Checking one key by
// name would leave that open while the file claims to carry no suppressions.
//
// Fail-closed also means a key added by a future grype version has to be
// considered here before it can be used.
var grypeConfigAllowedKeys = map[string]bool{"ignore": true}

func TestGrypeConfigCarriesNoSuppressions(t *testing.T) {
	raw, err := os.ReadFile(grypeConfig)
	if err != nil {
		t.Fatalf("read %s: %v", grypeConfig, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", grypeConfig, err)
	}

	for k := range doc {
		if !grypeConfigAllowedKeys[k] {
			t.Errorf("%s sets %q. Only %v may be set here, because other grype keys "+
				"remove findings too -- `exclude:` drops whole paths from the scan. If "+
				"this key is genuinely not a suppression, add it to the allowlist "+
				"deliberately.", grypeConfig, k, keysOf(grypeConfigAllowedKeys))
		}
	}

	if ignore, ok := doc["ignore"].([]any); ok && len(ignore) > 0 {
		t.Errorf("%s carries %d ignore rule(s); suppressions belong in %s, where the "+
			"product PURL, subcomponent scope, justification, impact statement and "+
			"re-affirmation date are enforced. Move them and leave `ignore: []` here.",
			grypeConfig, len(ignore), openVEXDoc)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestScanWaitsForSuppressionValidation holds the ordering edge that makes the
// re-affirmation check gate the scan.
//
// `validate-suppressions` produces no output the scan consumes, so the edge is
// ordering-only -- which means it generates no `needs.<job>.outputs.<field>`
// reference and TestJobOutputReferencesResolve cannot see it. Removing it as
// "these don't depend on each other, run them in parallel" leaves the scan
// applying a statement that lapsed, on a green run, which is the exact scenario
// the job's own header says it exists to prevent.
func TestScanWaitsForSuppressionValidation(t *testing.T) {
	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Needs stringOrSlice `json:"needs"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}

	scan, ok := doc.Jobs["scan"]
	if !ok {
		t.Fatalf("%s has no \"scan\" job; this test no longer covers what it claims",
			vulnScanWorkflow)
	}
	if !slices.Contains(scan.Needs, "validate-suppressions") {
		t.Errorf("%s: job \"scan\" does not declare validate-suppressions in needs "+
			"(declares %v), so a lapsed or malformed statement is applied by the scan "+
			"instead of failing it", vulnScanWorkflow, []string(scan.Needs))
	}
}

// scanStep returns the `with:` block of the scan step, by job and action prefix.
func scanStepWith(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string         `json:"uses"`
				With map[string]any `json:"with"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}
	for _, s := range doc.Jobs["scan"].Steps {
		if strings.HasPrefix(s.Uses, "anchore/scan-action@") {
			return s.With
		}
	}
	t.Fatalf("%s has no anchore/scan-action step in job \"scan\"; this test no longer "+
		"covers what it claims and needs updating to match the scanner in use",
		vulnScanWorkflow)
	return nil
}

// TestVulnScanPassesTheVexDocument holds the wiring that makes every statement
// in .openvex.json have any effect.
//
// The document is inert unless the scan forwards it to grype. Drop the `vex:`
// input and grype never reads the file: no error, no warning, no log line --
// every suppression stops applying at once and the scan simply reports more,
// which reads as a bad week upstream rather than a broken config.
//
// Also asserts `config:` stays unset. Setting it disables grype's auto-detection
// of .grype.yaml, which is how the config would silently stop being read.
func TestVulnScanPassesTheVexDocument(t *testing.T) {
	with := scanStepWith(t)

	vex, _ := with["vex"].(string)
	if vex != ".openvex.json" {
		t.Errorf("%s: the scan step passes vex=%q, want %q; without it grype never "+
			"reads the suppression document and every statement silently stops applying",
			vulnScanWorkflow, vex, ".openvex.json")
	}

	if cfg, ok := with["config"]; ok {
		t.Errorf("%s: the scan step sets config=%v, which disables grype's "+
			"auto-detection of .grype.yaml", vulnScanWorkflow, cfg)
	}
}

// repoFiles are the workspace-relative paths this workflow reads.
var repoFiles = []string{".openvex.json", ".grype.yaml"}

// TestWorkflowJobsCheckOutBeforeReadingRepoFiles generalises the checkout guard
// to every job, not just the one that scans.
//
// The narrower version covered only the step running anchore/scan-action, so
// when a later step in a DIFFERENT job started reading .openvex.json from the
// workspace, nothing noticed that job had no checkout. jq exits non-zero on the
// missing file and `set -euo pipefail` fails the step before any no-op guard
// inside it runs -- so the job failed on every run, including with an empty
// document, taking the summary and the findings alert with it.
//
// Referencing a repository path from a job that never checked the repository out
// is the general shape; this checks for it wherever it appears.
func TestWorkflowJobsCheckOutBeforeReadingRepoFiles(t *testing.T) {
	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `json:"name"`
				Uses string            `json:"uses"`
				Run  string            `json:"run"`
				With map[string]string `json:"with"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}

	checked := 0
	for jobName, j := range doc.Jobs {
		checkedOut := false
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				checkedOut = true
				continue
			}

			// Comments inside a run body are not references; strip them so a
			// step that only mentions a path in prose is not flagged.
			var body strings.Builder
			for line := range strings.SplitSeq(s.Run, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(line), "#") {
					body.WriteString(line)
					body.WriteString("\n")
				}
			}
			for _, v := range s.With {
				body.WriteString(v)
				body.WriteString("\n")
			}

			for _, f := range repoFiles {
				if !strings.Contains(body.String(), f) {
					continue
				}
				checked++
				if !checkedOut {
					t.Errorf("%s: job %q step %q reads %s from the workspace but the job "+
						"has no preceding actions/checkout; the step fails on every run",
						vulnScanWorkflow, jobName, s.Name, f)
				}
			}
		}
	}

	// Guards the guard: if the paths are renamed, the loop above matches nothing
	// and passes without having checked anything.
	if checked == 0 {
		t.Fatalf("%s: no step references any of %v; this test no longer covers what it "+
			"claims and needs updating", vulnScanWorkflow, repoFiles)
	}
}

// TestVulnScanChecksOutBeforeScanning holds the only thing that makes
// .openvex.json and .grype.yaml reachable at all.
//
// scan-action passes neither --config nor a cwd, so grype runs in
// GITHUB_WORKSPACE: it resolves the relative --vex path from there and finds
// ./.grype.yaml through its default config search. Both work solely because the
// job checks the repository out first. Nothing else in the scan job needs the
// source -- it scans a registry digest.
//
// So the checkout reads as removable, and removing it disarms every suppression
// without failing anything. The scan simply starts reporting findings that were
// triaged, which looks like a bad week upstream rather than a broken config --
// and the fix people reach for is another statement that also does nothing.
func TestVulnScanChecksOutBeforeScanning(t *testing.T) {
	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}

	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `json:"name"`
				Uses string `json:"uses"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}

	scanned := false
	for jobName, j := range doc.Jobs {
		checkedOut := false
		for _, s := range j.Steps {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				checkedOut = true
			}
			if !strings.HasPrefix(s.Uses, "anchore/scan-action@") {
				continue
			}
			scanned = true
			if !checkedOut {
				t.Errorf("%s: job %q runs anchore/scan-action with no preceding actions/checkout; "+
					"grype resolves .openvex.json and .grype.yaml from the workspace, so every "+
					"suppression silently stops applying", vulnScanWorkflow, jobName)
			}
		}
	}

	// Guards the guard. If the action is ever renamed or replaced, the loop
	// above matches nothing and passes without having checked anything.
	if !scanned {
		t.Fatalf("%s: found no anchore/scan-action step; this test no longer covers "+
			"what it claims and needs updating to match the scanner in use", vulnScanWorkflow)
	}
}
