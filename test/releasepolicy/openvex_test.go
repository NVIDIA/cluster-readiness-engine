// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// openVEXDoc is the single home for vulnerability suppressions and the impact
// analysis behind them.
const openVEXDoc = "../../.openvex.json"

// imageProductPURL is the product every statement must target.
//
// Grype derives the OCI product PURL from the registry repository BASENAME, not
// the full path and not org.opencontainers.image.title. Verified against the
// published image with grype v0.118.0 -- the version the pinned scan-action
// installs:
//
//	pkg:oci/manager                                 -> suppression applies
//	pkg:oci/cluster-readiness-engine/manager        -> no match
//	pkg:oci/nvidia/cluster-readiness-engine/manager -> no match
//
// A statement naming any other product is not an error anywhere: grype accepts
// the document, applies nothing, and the finding keeps being reported. That
// silence is the entire reason this constant is asserted rather than trusted.
const imageProductPURL = "pkg:oci/manager"

// maxStatementAge is how long a statement may go without being re-affirmed.
//
// OpenVEX has no expiry concept. A statement is true when written and stays in
// the file forever, including after the dependency is upgraded past the fix, the
// advisory is withdrawn, or the package leaves the image -- at which point it
// suppresses nothing and nobody notices, or worse, still suppresses something
// whose reachability changed. Requiring re-affirmation is what brings the claim
// back for a human to look at.
const maxStatementAge = 180 * 24 * time.Hour

// clockSkew is how far ahead of now a stamp may legitimately sit.
//
// The age check has to be bounded on BOTH sides. Rejecting only the past leaves
// `last_updated: 2099-01-01`, which satisfies every other rule here while
// producing exactly what the re-affirmation rule exists to prevent: a statement
// that never comes back. now.Sub(when) is negative for a future date, so a
// one-sided check can never fire on it -- and a distant future date reads as
// "recently affirmed" in review, so it is a softer target than an expired one.
//
// The tolerance exists only for timezone and runner-clock differences on a date
// written as midnight UTC; it is far too small to hide a defeated clock.
const clockSkew = 48 * time.Hour

// minImpactStatement is the shortest impact statement treated as substantive.
//
// Crude on purpose: the real requirement is concrete evidence -- a grep that
// returns nothing, a file path proving a feature is gated off, advisory text
// limiting the trigger to a configuration this project does not use -- and no
// test can judge that. What a test can do is make "not exploitable" fail, so the
// thin version is never the path of least resistance. Reviewers judge the rest.
const minImpactStatement = 80

// vexJustifications is the OpenVEX v0.2.0 enum. A justification outside it is
// not valid OpenVEX, and consumers other than grype may reject the document.
var vexJustifications = map[string]bool{
	"component_not_present":                             true,
	"vulnerable_code_not_present":                       true,
	"vulnerable_code_not_in_execute_path":               true,
	"vulnerable_code_cannot_be_controlled_by_adversary": true,
	"inline_mitigations_already_exist":                  true,
}

// vexStatuses is the OpenVEX v0.2.0 status enum.
var vexStatuses = map[string]bool{
	"not_affected":        true,
	"affected":            true,
	"fixed":               true,
	"under_investigation": true,
}

type vexDocument struct {
	Context    string         `json:"@context"`
	ID         string         `json:"@id"`
	Author     string         `json:"author"`
	Timestamp  string         `json:"timestamp"`
	Version    int            `json:"version"`
	Statements []vexStatement `json:"statements"`
}

type vexStatement struct {
	Vulnerability struct {
		Name string `json:"name"`
	} `json:"vulnerability"`
	Products []struct {
		ID          string `json:"@id"`
		Identifiers struct {
			PURL string `json:"purl"`
		} `json:"identifiers"`
		// Subcomponents scope a statement to one package inside the image.
		// go-vex's Product.Matches returns true for ANY subcomponent identifier
		// when this list is empty, so a statement without it suppresses its
		// advisory across the whole image -- including in a package whose
		// reachability nobody analysed.
		Subcomponents []struct {
			ID          string `json:"@id"`
			Identifiers struct {
				PURL string `json:"purl"`
			} `json:"identifiers"`
		} `json:"subcomponents"`
	} `json:"products"`
	Status          string `json:"status"`
	Justification   string `json:"justification"`
	ImpactStatement string `json:"impact_statement"`
	ActionStatement string `json:"action_statement"`
	Timestamp       string `json:"timestamp"`
	LastUpdated     string `json:"last_updated"`
}

// checkOpenVEX returns one message per problem, empty when the document is
// triageable.
//
// Split from the test that reads the real file so the rules can run against
// documents that are deliberately wrong. With `statements: []` -- the committed
// state, and the state this file will be in most of the time -- every per
// statement assertion is vacuously true, and a test that only ever sees that
// input passes no matter how broken it is.
//
// `now` is a parameter rather than time.Now() so age can be tested at all.
func checkOpenVEX(raw []byte, now time.Time) []string {
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	var doc vexDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return []string{fmt.Sprintf("parse: %v", err)}
	}

	if !strings.Contains(doc.Context, "openvex.dev/ns/v0.2.0") {
		report("@context is %q; the justification and status enums enforced here "+
			"are the v0.2.0 ones", doc.Context)
	}
	if doc.Version < 1 {
		report("document version is %d; bump it on every substantive edit so "+
			"downstream consumers can detect drift", doc.Version)
	}

	for i, s := range doc.Statements {
		name := strings.TrimSpace(s.Vulnerability.Name)
		label := fmt.Sprintf("statement %d (%s)", i+1, name)

		if name == "" {
			report("statement %d names no vulnerability", i+1)
			continue
		}

		// Invariant 1. The silent one.
		//
		// EVERY matching product must be scoped, not merely one of them. grype
		// applies each product entry independently, so one unscoped entry
		// suppresses the advisory image-wide no matter how carefully its
		// siblings are scoped -- and a check that accepted "at least one scoped"
		// would pass on exactly that document.
		targeted := false
		unscoped := false
		for _, p := range s.Products {
			if p.Identifiers.PURL != imageProductPURL && p.ID != imageProductPURL {
				continue
			}
			targeted = true
			if len(p.Subcomponents) == 0 {
				unscoped = true
			}
		}
		if !targeted {
			report("%s does not target %s, so grype will apply nothing and the "+
				"finding keeps being reported with no warning anywhere",
				label, imageProductPURL)
		} else if unscoped {
			report("%s has a %s product entry with no subcomponents, so it suppresses "+
				"%s across the whole image rather than in the package that was analysed. "+
				"An advisory can match more than one package -- the impact statement "+
				"would describe one and silence all of them. Name the affected package "+
				"as a subcomponent on every product entry.",
				label, imageProductPURL, name)
		}

		if !vexStatuses[s.Status] {
			report("%s has status %q, which is not an OpenVEX v0.2.0 status", label, s.Status)
		}

		switch s.Status {
		case "fixed":
			// grype's VEX ignore list is {not_affected, fixed}, so this suppresses
			// exactly like not_affected -- while OpenVEX forbids a `fixed`
			// statement from carrying a justification, impact_statement or
			// action_statement. It is therefore a suppression that cannot, by
			// spec, record why. That makes it the cheapest way to hide a finding
			// in this repository, which is the opposite of the point.
			report("%s uses status \"fixed\", which grype suppresses exactly like "+
				"not_affected but which OpenVEX forbids carrying any justification or "+
				"impact statement -- so it would hide a finding with no reviewable claim "+
				"on record. If the fix genuinely shipped, the scan reads published "+
				"digests and stops reporting it without help; if the scanner is wrong, "+
				"say so with not_affected and vulnerable_code_not_present.", label)
		case "not_affected":
			if !vexJustifications[s.Justification] {
				report("%s has justification %q, which is not in the OpenVEX v0.2.0 enum",
					label, s.Justification)
			}
			if n := len(strings.TrimSpace(s.ImpactStatement)); n < minImpactStatement {
				report("%s has a %d-character impact statement; cite the specific code "+
					"path, file or advisory language that supports the claim -- it is "+
					"published to a public registry and read by auditors", label, n)
			}
		case "affected":
			if strings.TrimSpace(s.ActionStatement) == "" {
				report("%s is status affected with no action_statement", label)
			}
		}

		// Re-affirmation. last_updated wins when both are present.
		stamp := s.LastUpdated
		if strings.TrimSpace(stamp) == "" {
			stamp = s.Timestamp
		}
		if strings.TrimSpace(stamp) == "" {
			report("%s carries neither timestamp nor last_updated, so nothing brings "+
				"it back for re-triage", label)
			continue
		}
		when, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			report("%s has an unparseable timestamp %q; use RFC3339", label, stamp)
			continue
		}
		if when.After(now.Add(clockSkew)) {
			report("%s is dated %s, in the future. now.Sub(when) is negative for a "+
				"forward-dated stamp, so the age check below can never fire on it and "+
				"the statement would never return for re-triage -- which is the whole "+
				"of what re-affirmation replaces.", label, when.Format("2006-01-02"))
			continue
		}
		if now.Sub(when) > maxStatementAge {
			report("%s was last affirmed on %s, more than %d days ago. Re-check whether "+
				"it still applies: if the dependency moved past the fix or the advisory "+
				"was withdrawn, delete it; otherwise refresh last_updated and say what "+
				"you re-verified.", label, when.Format("2006-01-02"),
				int(maxStatementAge.Hours()/24))
		}
	}

	return problems
}

// TestOpenVEXStatementsAreTriageable is what keeps the suppression file honest.
//
// A VEX statement that does not apply is invisible. Grype accepts the document,
// silently matches nothing, and the only signal is that the CVE keeps appearing
// -- which reads as "still vulnerable", not as "your suppression is broken".
func TestOpenVEXStatementsAreTriageable(t *testing.T) {
	raw, err := os.ReadFile(openVEXDoc)
	if err != nil {
		t.Fatalf("read %s: %v", openVEXDoc, err)
	}
	for _, p := range checkOpenVEX(raw, time.Now().UTC()) {
		t.Errorf("%s: %s", openVEXDoc, p)
	}
}

// TestOpenVEXProductMatchesTheScannedImage binds the constant above to the image
// the workflow actually scans.
//
// imageProductPURL is derived from the basename of `env.IMAGE`, but nothing
// otherwise relates the two. Rename the published repository -- for 1.0, for a
// namespace move, or because a second controller image replaces this one -- and
// grype derives a different product PURL, so every statement matches nothing and
// all suppressions stop applying at once.
//
// The guard would invert rather than fire: TestOpenVEXStatementsAreTriageable
// would keep passing while enforcing the stale value, and would then REJECT a
// statement someone corrected to the new basename.
func TestOpenVEXProductMatchesTheScannedImage(t *testing.T) {
	raw, err := os.ReadFile(vulnScanWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", vulnScanWorkflow, err)
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", vulnScanWorkflow, err)
	}

	image := doc.Env["IMAGE"]
	if image == "" {
		t.Fatalf("%s declares no env.IMAGE; this test can no longer confirm that %s "+
			"matches what is scanned", vulnScanWorkflow, imageProductPURL)
	}

	// Grype derives the OCI product PURL from the registry repository basename.
	want := "pkg:oci/" + path.Base(image)
	if imageProductPURL != want {
		t.Errorf("the scan targets %s, for which grype derives %q, but statements are "+
			"required to name %q. Every statement in %s currently matches nothing.",
			image, want, imageProductPURL, openVEXDoc)
	}
}

// vexDoc builds a document around one statement body.
func vexDoc(statement string) string {
	return `{"@context":"https://openvex.dev/ns/v0.2.0","@id":"x","author":"a",` +
		`"timestamp":"2026-09-01T00:00:00Z","version":1,"statements":[` + statement + `]}`
}

// goodProducts targets the image and scopes the claim to one package inside it.
const goodProducts = `"products":[{"@id":"pkg:oci/manager","identifiers":{"purl":"pkg:oci/manager"},` +
	`"subcomponents":[{"@id":"pkg:golang/google.golang.org/grpc@v1.83.0",` +
	`"identifiers":{"purl":"pkg:golang/google.golang.org/grpc@v1.83.0"}}]}]`

// unscopedProducts targets the image but analyses nothing in particular, so it
// silences the advisory in every package the image carries.
const unscopedProducts = `"products":[{"@id":"pkg:oci/manager","identifiers":{"purl":"pkg:oci/manager"}}]`

const goodImpact = `"impact_statement":"The vulnerable Decompress path is reached only from ` +
	`the archive/zip reader, which this controller never constructs; grep -rn for the symbol ` +
	`across pkg/ and cmd/ returns zero hits."`

// TestOpenVEXCheckRejects proves the check above does something. Without it the
// committed empty document means the rules never run against a statement, and
// the first suppression anyone writes is the first time they execute.
func TestOpenVEXCheckRejects(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// The silent one: everything else is correct, grype applies nothing.
			name: "statement targeting the full image path instead of the basename",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` +
				`"products":[{"@id":"pkg:oci/cluster-readiness-engine/manager",` +
				`"identifiers":{"purl":"pkg:oci/cluster-readiness-engine/manager"}}],` +
				`"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "does not target pkg:oci/manager",
		},
		{
			name: "justification outside the v0.2.0 enum",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"not_exploitable",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "not in the OpenVEX v0.2.0 enum",
		},
		{
			name: "boilerplate impact statement",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				`"impact_statement":"not exploitable","timestamp":"2026-09-01T00:00:00Z"}`),
			want: "impact statement",
		},
		{
			name: "statement gone stale",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-01-01T00:00:00Z"}`),
			want: "more than 180 days ago",
		},
		{
			name: "statement with no timestamp at all",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `}`),
			want: "neither timestamp nor last_updated",
		},
		{
			name: "statement naming no vulnerability",
			body: vexDoc(`{"vulnerability":{"name":""},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "names no vulnerability",
		},
		{
			name: "status outside the enum",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"mitigated","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "not an OpenVEX v0.2.0 status",
		},
		{
			name: "affected with no action statement",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"affected","timestamp":"2026-09-01T00:00:00Z"}`),
			want: "no action_statement",
		},
		{
			// grype suppresses `fixed` exactly like `not_affected`, and OpenVEX
			// forbids evidence fields on it -- a suppression that cannot record
			// why. Previously accepted by this suite, which sanctioned it.
			name: "fixed status suppresses with no reviewable claim",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"fixed","timestamp":"2026-09-01T00:00:00Z"}`),
			want: `status "fixed"`,
		},
		{
			// The one-sided age check can never fire on a forward-dated stamp,
			// so this defeats re-affirmation permanently.
			name: "statement dated far in the future never returns for re-triage",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z",` +
				`"last_updated":"2099-01-01T00:00:00Z"}`),
			want: "in the future",
		},
		{
			// The likelier form: an off-by-one-year typo when refreshing a date.
			name: "re-affirmation date mistyped a year ahead",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"last_updated":"2027-09-04T00:00:00Z"}`),
			want: "in the future",
		},
		{
			name: "statement that suppresses the advisory image-wide",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + unscopedProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "no subcomponents",
		},
		{
			// One scoped entry does not redeem an unscoped sibling: grype applies
			// each product independently, so the unscoped one still covers the
			// whole image. A check accepting "at least one scoped" passes here.
			name: "statement mixing a scoped and an unscoped product entry",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},"products":[` +
				`{"@id":"pkg:oci/manager","identifiers":{"purl":"pkg:oci/manager"},` +
				`"subcomponents":[{"@id":"pkg:golang/x@v1","identifiers":{"purl":"pkg:golang/x@v1"}}]},` +
				`{"@id":"pkg:oci/manager","identifiers":{"purl":"pkg:oci/manager"}}],` +
				`"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
			want: "no subcomponents",
		},
		{
			name: "wrong openvex context version",
			body: `{"@context":"https://openvex.dev/ns/v0.1.0","@id":"x","author":"a",` +
				`"timestamp":"2026-09-01T00:00:00Z","version":1,"statements":[]}`,
			want: "@context",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := checkOpenVEX([]byte(tc.body), now)
			if len(problems) == 0 {
				t.Fatalf("document was accepted but must be rejected:\n%s", tc.body)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Errorf("rejected for the wrong reason\n got: %s\nwant substring: %s",
					strings.Join(problems, "\n"), tc.want)
			}
		})
	}
}

// TestOpenVEXCheckAccepts guards the other direction: a correctly formed
// statement must not be refused, or the mechanism is unusable and the next
// person deletes the check instead of writing the statement properly.
func TestOpenVEXCheckAccepts(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "the committed empty document",
			body: `{"@context":"https://openvex.dev/ns/v0.2.0","@id":"x","author":"a",` +
				`"timestamp":"2026-09-01T00:00:00Z","version":1,"statements":[]}`,
		},
		{
			name: "one fully triageable statement",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-vp52-pcj8-j9qc"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-01T00:00:00Z"}`),
		},
		{
			// last_updated is what re-affirmation refreshes, so a stale timestamp
			// with a fresh last_updated must pass.
			name: "old statement re-affirmed via last_updated",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2025-01-01T00:00:00Z",` +
				`"last_updated":"2026-08-01T00:00:00Z"}`),
		},
		{
			// Neither status suppresses anything in grype, so neither needs the
			// evidence a not_affected statement does.
			name: "under_investigation needs no justification",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"under_investigation","timestamp":"2026-09-01T00:00:00Z"}`),
		},
		{
			name: "affected with an action statement",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"affected","action_statement":"Upgrade to 1.83.1 in the next release.",` +
				`"timestamp":"2026-09-01T00:00:00Z"}`),
		},
		{
			// Inside the clock-skew tolerance, which exists for a date written as
			// midnight UTC from a machine ahead of it.
			name: "stamp a few hours ahead of now",
			body: vexDoc(`{"vulnerability":{"name":"GHSA-x"},` + goodProducts +
				`,"status":"not_affected","justification":"vulnerable_code_not_in_execute_path",` +
				goodImpact + `,"timestamp":"2026-09-04T12:00:00Z"}`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if problems := checkOpenVEX([]byte(tc.body), now); len(problems) > 0 {
				t.Errorf("valid document was rejected: %s", strings.Join(problems, "\n"))
			}
		})
	}
}
