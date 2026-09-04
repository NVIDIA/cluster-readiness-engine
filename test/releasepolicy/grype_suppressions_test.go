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

// maxExpiryHorizon bounds how far ahead a suppression may be dated.
//
// Rejecting only past dates leaves `# expires: 2099-01-01`, which satisfies
// every other rule here while producing exactly the outcome .grype.yaml says
// the expiry exists to prevent: a finding that never comes back. An expiry
// nobody will live to see is the same as no expiry, so the horizon is what
// makes the field mean anything.
const maxExpiryHorizon = 180 * 24 * time.Hour

// ruleStart matches the line that begins one ignore rule. Comment lines cannot
// match, so the documented example block in .grype.yaml is not counted.
var ruleStart = regexp.MustCompile(`^\s*-\s+\S`)

// commentLine matches any comment, used to walk the contiguous block above a
// rule.
var commentLine = regexp.MustCompile(`^\s*#`)

// checkGrypeConfig returns one message per problem found, empty if the config
// is triageable.
//
// Split out from the test that reads the real file so the rules can be run
// against configs that are deliberately wrong. With `ignore: []` -- the
// committed state, and the state this file will be in most of the time -- a
// count-based check is vacuously true: zero rules match zero comments and no
// loop executes. A test that only ever sees that input passes with every regex
// broken, which is indistinguishable from a test that works.
//
// Comments are associated with the rule they sit above rather than counted
// against the file total. .grype.yaml tells contributors that "every rule must
// carry four things"; counting cannot enforce that, and accepts a file whose
// justifications and expiries sit anywhere so long as the totals agree -- which
// makes re-triage a guess about which justification belongs to which CVE.
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

	blocks := ruleCommentBlocks(string(raw))
	if len(blocks) != len(doc.Ignore) {
		report("found %d ignore rules but %d rule lines; the file's shape is not "+
			"what this check can reason about", len(doc.Ignore), len(blocks))
		return problems
	}

	for i, rule := range doc.Ignore {
		label := fmt.Sprintf("ignore rule %d (%s)", i+1, rule.Vulnerability)

		if strings.TrimSpace(rule.Vulnerability) == "" {
			report("ignore rule %d names no vulnerability", i+1)
		}
		// A rule with no package suppresses the id everywhere, including in a
		// package that really is affected.
		if strings.TrimSpace(rule.Package.Name) == "" {
			report("%s names no package; "+
				"suppressing an id globally hides it in packages that are affected", label)
		}

		b := blocks[i]
		if b.justification == "" {
			report("%s has no `# justification:` in the comment block directly above it; "+
				"a suppression without a stated reason cannot be re-triaged by anyone else", label)
		}
		if b.expires == "" {
			report("%s has no `# expires:` in the comment block directly above it; "+
				"every suppression needs a date after which it stops applying", label)
			continue
		}

		expiry, err := time.Parse("2006-01-02", b.expires)
		if err != nil {
			report("%s: `# expires: %s` is not a YYYY-MM-DD date", label, b.expires)
			continue
		}
		if now.After(expiry) {
			report("%s expired on %s. Re-triage it: confirm whether the finding still "+
				"applies, then either fix it, or renew the rule with a new date and an "+
				"updated justification. Do not simply extend the date.", label, b.expires)
		}
		if expiry.After(now.Add(maxExpiryHorizon)) {
			report("%s expires on %s, more than %d days out. A suppression dated that far "+
				"ahead never comes back for re-triage, which is what an expiry is for.",
				label, b.expires, int(maxExpiryHorizon.Hours()/24))
		}
	}

	return problems
}

// ruleBlock is the justification and expiry found in the contiguous comment
// block immediately above one ignore rule.
type ruleBlock struct {
	justification string
	expires       string
}

// ruleCommentBlocks returns one entry per ignore rule, in file order, carrying
// whatever the contiguous comment block directly above that rule declared.
//
// "Directly above" is the whole point: a block separated from its rule by
// another rule, or parked above the `ignore:` key, documents nothing a reader
// can act on.
func ruleCommentBlocks(body string) []ruleBlock {
	lines := strings.Split(body, "\n")
	var out []ruleBlock

	for i, line := range lines {
		if !ruleStart.MatchString(line) {
			continue
		}
		var b ruleBlock
		for j := i - 1; j >= 0 && commentLine.MatchString(lines[j]); j-- {
			if m := justificationComment.FindStringSubmatch(lines[j]); m != nil && b.justification == "" {
				b.justification = strings.TrimSpace(m[1])
			}
			if m := expiryComment.FindStringSubmatch(lines[j]); m != nil && b.expires == "" {
				b.expires = m[1]
			}
		}
		out = append(out, b)
	}
	return out
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

	// A well-formed block, directly above its rule, well inside the horizon.
	const ok = "ignore:\n  # justification: not reachable in this image\n" +
		"  # expires: 2026-12-31\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n"

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "rule with no expires comment",
			body: "ignore:\n  # justification: not reachable\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "no `# expires:`",
		},
		{
			name: "rule with no justification comment",
			body: "ignore:\n  # expires: 2026-12-31\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "no `# justification:`",
		},
		{
			name: "expired rule",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2026-09-03\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "expired on 2026-09-03",
		},
		{
			// The rule this file exists to prevent. Every other assertion is
			// satisfied; only the horizon catches it.
			name: "expiry so far out the finding never returns",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2099-01-01\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "more than 180 days out",
		},
		{
			name: "rule naming no package suppresses the id everywhere",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2026-12-31\n" +
				"  - vulnerability: CVE-2026-1\n",
			want: "names no package",
		},
		{
			name: "rule naming no vulnerability",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2026-12-31\n" +
				"  - package:\n      name: libfoo\n",
			want: "names no vulnerability",
		},
		{
			// The regex matches the shape, so only the parse catches it. A month
			// of 13 is the case a shape-only check would wave through.
			name: "expiry that has the shape of a date but is not one",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2026-13-99\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
			want: "is not a YYYY-MM-DD date",
		},
		{
			// Both blocks parked above the `ignore:` key rather than above the
			// rules they justify. Totals agree, so a counting check accepts this
			// -- and nobody can tell which justification belongs to which CVE.
			name: "comment blocks not attached to the rules they describe",
			body: "# justification: not reachable\n# expires: 2026-12-31\n" +
				"# justification: fixed upstream\n# expires: 2026-11-30\n" +
				"ignore:\n  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n" +
				"  - vulnerability: CVE-2026-2\n    package:\n      name: libbar\n",
			want: "no `# justification:` in the comment block directly above it",
		},
		{
			// The second rule borrows nothing: the block above it belongs to the
			// first rule, and there is no contiguous comment run of its own.
			name: "second rule with no block of its own",
			body: ok + "  - vulnerability: CVE-2026-2\n    package:\n      name: libbar\n",
			want: "no `# justification:`",
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
			body: "ignore:\n" +
				"  # justification: the vulnerable code path is not compiled in\n" +
				"  # expires: 2026-12-31\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
		},
		{
			// Each block sits directly above the rule it describes, which is the
			// layout .grype.yaml documents and the only one a re-triager can
			// read. The earlier version of this case put both blocks above the
			// `ignore:` key and still called itself "each with their own block".
			name: "two rules each with their own block",
			body: "ignore:\n" +
				"  # justification: not reachable\n  # expires: 2026-12-31\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n" +
				"  # justification: fixed upstream, waiting on a base image bump\n" +
				"  # expires: 2026-11-30\n" +
				"  - vulnerability: GHSA-aaaa-bbbb-cccc\n    package:\n      name: libbar\n",
		},
		{
			name: "expiry exactly at the horizon",
			body: "ignore:\n  # justification: not reachable\n  # expires: 2027-03-03\n" +
				"  - vulnerability: CVE-2026-1\n    package:\n      name: libfoo\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if problems := checkGrypeConfig([]byte(tc.body), now); len(problems) > 0 {
				t.Errorf("valid config was rejected: %s", strings.Join(problems, "\n"))
			}
		})
	}
}
