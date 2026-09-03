// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
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

	var doc struct {
		Ignore []struct {
			Vulnerability string `json:"vulnerability"`
			Package       struct {
				Name string `json:"name"`
			} `json:"package"`
		} `json:"ignore"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", grypeConfig, err)
	}

	body := string(raw)
	expiries := expiryComment.FindAllStringSubmatch(body, -1)
	justifications := justificationComment.FindAllStringSubmatch(body, -1)

	// One of each per rule. Counting rather than associating keeps this simple,
	// and a mismatch is exactly the case worth failing on: a rule added without
	// its comment block, or a comment block left behind by a deleted rule.
	if len(expiries) != len(doc.Ignore) {
		t.Errorf("%s has %d ignore rules but %d `# expires:` comments; "+
			"every suppression needs a date after which it stops applying",
			grypeConfig, len(doc.Ignore), len(expiries))
	}
	if len(justifications) != len(doc.Ignore) {
		t.Errorf("%s has %d ignore rules but %d `# justification:` comments; "+
			"a suppression without a stated reason cannot be re-triaged by anyone else",
			grypeConfig, len(doc.Ignore), len(justifications))
	}

	for i, rule := range doc.Ignore {
		if strings.TrimSpace(rule.Vulnerability) == "" {
			t.Errorf("%s: ignore rule %d names no vulnerability", grypeConfig, i+1)
		}
		// A rule with no package suppresses the id everywhere, including in a
		// package that really is affected.
		if strings.TrimSpace(rule.Package.Name) == "" {
			t.Errorf("%s: ignore rule %d (%s) names no package; "+
				"suppressing an id globally hides it in packages that are affected",
				grypeConfig, i+1, rule.Vulnerability)
		}
	}

	today := time.Now().UTC()
	for _, m := range expiries {
		expiry, err := time.Parse("2006-01-02", m[1])
		if err != nil {
			t.Errorf("%s: `# expires: %s` is not a YYYY-MM-DD date", grypeConfig, m[1])
			continue
		}
		if today.After(expiry) {
			t.Errorf("%s: a suppression expired on %s. Re-triage it: confirm whether the "+
				"finding still applies, then either fix it, or renew the rule with a new "+
				"date and an updated justification. Do not simply extend the date.",
				grypeConfig, m[1])
		}
	}
}
