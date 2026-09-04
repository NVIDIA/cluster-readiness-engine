// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// grypeConfig holds the suppressions the weekly image scan applies.
const grypeConfig = "../../.grype.yaml"

// expiryComment matches the `# expires: YYYY-MM-DD` line a rule must carry.
// The date lives in a comment because grype has no such field -- it would
// reject an unknown key -- so the deadline is enforced here instead.
var expiryComment = regexp.MustCompile(`(?m)^\s*#\s*expires:\s*(\d{4}-\d{2}-\d{2})\s*$`)

// justificationComment matches the prose that says why a rule applies.
var justificationComment = regexp.MustCompile(`(?m)^\s*#\s*justification:\s*(\S.*)$`)

// checkGrypeConfig returns one message per problem found, empty if the config
// is triageable.
//
// Split out from the test that reads the real file so the rules can be run
// against configs that are deliberately wrong. With `ignore: []` -- the
// committed state, and the state this file will be in most of the time -- every
// assertion below is vacuously true: zero rules match zero comments and neither
// loop executes. A test that only ever sees that input passes with both regexes
// broken, which is indistinguishable from a test that works.
//
// `now` is a parameter rather than time.Now() so expiry can be tested at all.
func checkGrypeConfig(raw []byte, now time.Time) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	var doc struct {
		Ignore []struct {
			Vulnerability string `json:"vulnerability"`
			Package       struct {
				Name string `json:"name"`
			} `json:"package"`
		} `json:"ignore"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf("parse: %v", err)}
	}

	body := string(raw)
	expiries := expiryComment.FindAllStringSubmatch(body, -1)
	justifications := justificationComment.FindAllStringSubmatch(body, -1)

	// One of each per rule. Counting rather than associating keeps this simple,
	// and a mismatch is exactly the case worth failing on: a rule added without
	// its comment block, or a comment block left behind by a deleted rule.
	if len(expiries) != len(doc.Ignore) {
		report("%d ignore rules but %d `# expires:` comments; "+
			"every suppression needs a date after which it stops applying",
			len(doc.Ignore), len(expiries))
	}
	if len(justifications) != len(doc.Ignore) {
		report("%d ignore rules but %d `# justification:` comments; "+
			"a suppression without a stated reason cannot be re-triaged by anyone else",
			len(doc.Ignore), len(justifications))
	}

	for i, rule := range doc.Ignore {
		if strings.TrimSpace(rule.Vulnerability) == "" {
			report("ignore rule %d names no vulnerability", i+1)
		}
		// A rule with no package suppresses the id everywhere, including in a
		// package that really is affected.
		if strings.TrimSpace(rule.Package.Name) == "" {
			report("ignore rule %d (%s) names no package; "+
				"suppressing an id globally hides it in packages that are affected",
				i+1, rule.Vulnerability)
		}
	}

	for _, m := range expiries {
		expiry, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			report("`# expires: %s` is not a YYYY-MM-DD date", m[1])
			continue
		}
		if now.After(expiry) {
			report("a suppression expired on %s. Re-triage it: confirm whether the "+
				"finding still applies, then either fix it, or renew the rule with a new "+
				"date and an updated justification. Do not simply extend the date.", m[1])
		}
	}

	return problems
}

// TestGrypeIgnoreRulesAreTriageable is what keeps the weekly scan worth reading.
//
// A suppression with no expiry outlives its reason. The CVE stops being
// reported, the package stays vulnerable, and nothing ever asks again -- the
// finding is not fixed, it is hidden, and the hiding is permanent. Grype cannot
// enforce this because it has no notion of an expiring rule, so the deadline is
// carried in a comment and enforced here.
//
// The failure is deliberately loud: an expired rule turns the build red until
// someone re-triages it. That is the whole mechanism. A quiet expiry would be
// the same as no expiry.
func TestGrypeIgnoreRulesAreTriageable(t *testing.T) {
	raw, err := os.ReadFile(grypeConfig)
	if err != nil {
		t.Fatalf("read %s: %v", grypeConfig, err)
	}
	for _, p := range checkGrypeConfig(raw, time.Now().UTC()) {
		t.Errorf("%s: %s", grypeConfig, p)
	}
}

// TestGrypeConfigCheckRejects is the half that proves the check above does
// anything. Each case is a config that must be refused, and the reason it must
// be refused is the reason the suppression mechanism is safe to have at all.
//
// Without these, the committed `ignore: []` means the rules are never once
// exercised against a rule -- so the first suppression anyone adds would be the
// first time the check runs, which is the worst moment to discover it does not.
func TestGrypeConfigCheckRejects(t *testing.T) {
	// Fixed so an expiry case cannot start or stop failing with the calendar.
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	const justified = "# justification: not reachable in this image\n# expires: 2099-01-01\n"

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "rule with no expires comment",
			body: "# justification: not reachable\nignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "`# expires:` comments",
		},
		{
			name: "rule with no justification comment",
			body: "# expires: 2099-01-01\nignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "`# justification:` comments",
		},
		{
			name: "expired rule",
			body: "# justification: not reachable\n# expires: 2026-09-03\n" +
				"ignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "expired on 2026-09-03",
		},
		{
			name: "rule naming no package suppresses the id everywhere",
			body: justified + "ignore:\n  - vulnerability: CVE-2026-1\n",
			want: "names no package",
		},
		{
			name: "rule naming no vulnerability",
			body: justified + "ignore:\n  - package:\n      name: libfoo\n",
			want: "names no vulnerability",
		},
		{
			// The regex matches the shape, so the counts agree and only the
			// parse catches it. A month of 13 is the case a shape-only check
			// would wave through.
			name: "expiry that has the shape of a date but is not one",
			body: "# justification: not reachable\n# expires: 2026-13-99\n" +
				"ignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "is not a YYYY-MM-DD date",
		},
		{
			name: "comment block left behind by a deleted rule",
			body: justified + "ignore: []\n",
			want: "`# expires:` comments",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkGrypeConfig([]byte(tc.body), now)
			if len(problems) == 0 {
				t.Fatalf("config was accepted but must be rejected:\n%s", tc.body)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("rejected for the wrong reason\n got: %s\nwant substring: %s",
					strings.Join(problems, "\n"), tc.want)
			}
		})
	}
}

// vulnScanWorkflow is the weekly image scan that consumes .grype.yaml.
const vulnScanWorkflow = "../../.github/workflows/vuln-scan-images.yml"

// TestVulnScanChecksOutBeforeScanning holds the only thing that makes every
// rule in .grype.yaml -- and every test above -- have any effect.
//
// scan-action passes neither --config nor a cwd to grype, so grype finds
// ./.grype.yaml through its default config search, relative to GITHUB_WORKSPACE.
// That works solely because the job checks the repository out first. Nothing
// else in the scan job needs the source: it scans a registry digest.
//
// So the checkout reads as removable, and removing it disarms every suppression
// without failing anything. The scan simply starts reporting findings that were
// triaged, which looks like a bad week upstream rather than a broken config --
// and the fix people reach for is another suppression that also does nothing.
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
					"grype resolves ./.grype.yaml from the workspace, so every suppression "+
					"silently stops applying", vulnScanWorkflow, jobName)
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

// TestGrypeConfigCheckAccepts guards the other direction: a correctly formed
// suppression must not be refused, or the mechanism is unusable and the next
// person deletes the check instead of the rule.
func TestGrypeConfigCheckAccepts(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "no suppressions at all",
			body: "ignore: []\n",
		},
		{
			name: "one fully triageable rule",
			body: "# justification: the vulnerable code path is not compiled in\n" +
				"# expires: 2026-12-31\n" +
				"ignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
		},
		{
			name: "two rules each with their own block",
			body: "# justification: not reachable\n# expires: 2026-12-31\n" +
				"# justification: fixed upstream, waiting on a base image bump\n# expires: 2026-11-30\n" +
				"ignore:\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n" +
				"  - vulnerability: GHSA-aaaa-bbbb-cccc\n    package:\n      name: libbar\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if problems := checkGrypeConfig([]byte(tc.body), now); len(problems) > 0 {
				t.Errorf("valid config was rejected: %s", strings.Join(problems, "\n"))
			}
		})
	}
}
