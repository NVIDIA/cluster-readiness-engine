// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package docspolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Policy samples are copy-pasted into clusters. A structural test cannot prove
// live admission (that needs Kind + registry), but it can stop the samples
// rotting back into the shapes that parked issue #272: cluster-wide Fail
// without a namespace selector, signature-only checks, a regexp stuffed into
// subject:, an over-broad manager* glob, or policy-controller without
// signatureFormat: bundle.
const (
	kyvernoSample          = "../../config/samples/policy/kyverno-verify-images.yaml"
	policyControllerSample = "../../config/samples/policy/policy-controller-verify-images.yaml"
)

const (
	wantIssuer  = "https://token.actions.githubusercontent.com"
	wantSubject = "https://github.com/NVIDIA/cluster-readiness-engine" +
		"/.github/workflows/attest.yml@refs/tags/"
	wantProvenanceType = "https://slsa.dev/provenance/v1"
	wantSignType       = "https://sigstore.dev/cosign/sign/v1"
	wantNSLabel        = "kubernetes.nvcre.nvidia.com/image-admission"
	wantEnforceMode    = "enforce"
)

func TestPolicySamplePinsTheReleaseIdentity(t *testing.T) {
	t.Run("kyverno", func(t *testing.T) {
		doc := mustLoadYAML(t, kyvernoSample)

		if got := asString(doc["kind"]); got != "ImageValidatingPolicy" {
			t.Fatalf("kind = %q, want ImageValidatingPolicy — ClusterPolicy/verifyImages "+
				"cannot see new-bundle referrer signatures", got)
		}

		spec := asMap(t, doc["spec"], "spec")
		if got := asString(spec["failurePolicy"]); got != "Fail" {
			t.Errorf("failurePolicy = %q, want Fail (fail closed)", got)
		}
		// failurePolicy is webhook-failure behaviour; enforcement is validationActions.
		actions := asStringSlice(spec["validationActions"])
		if len(actions) != 1 || actions[0] != "Deny" {
			t.Errorf("validationActions = %v, want [Deny] (Audit would stop denying)", actions)
		}

		match := asMap(t, spec["matchConstraints"], "spec.matchConstraints")
		ns := asMap(t, match["namespaceSelector"], "spec.matchConstraints.namespaceSelector")
		if !namespaceSelectorPinsOptIn(ns) {
			t.Error("matchConstraints.namespaceSelector must opt in via " + wantNSLabel +
				"=enforce; without it failurePolicy: Fail is cluster-wide")
		}

		globs := imageGlobs(t, spec["matchImageReferences"], "matchImageReferences")
		assertNarrowManagerGlobs(t, globs)

		attestorName := assertKyvernoSingleTightAttestor(t, spec)

		atts := asSlice(t, spec["attestations"], "spec.attestations")
		if !attestationTypePresent(atts, wantProvenanceType) {
			t.Errorf("attestations must require %s (signature-only was parked)", wantProvenanceType)
		}

		vals := asSlice(t, spec["validations"], "spec.validations")
		joined := expressionsJoined(vals)
		if !strings.Contains(joined, "verifyImageSignatures") {
			t.Error("validations must call verifyImageSignatures")
		}
		if !strings.Contains(joined, "verifyAttestationSignatures") {
			t.Error("validations must call verifyAttestationSignatures for provenance")
		}
		if !strings.Contains(joined, "slsaProvenance") {
			t.Error("validations must reference attestations.slsaProvenance")
		}
		assertValidationsReferenceOnlyAttestor(t, joined, attestorName)
		// Per expression, not across the joined set: one validation covering
		// init/ephemeral must not paper over another that dropped back to
		// images.containers alone.
		allPositive := regexp.MustCompile(`all\(\s*\w+\s*,\s*\w+\s*>\s*0\s*\)`)
		for i, raw := range vals {
			expr := asString(asMap(t, raw, "validations[]")["expression"])
			for _, key := range []string{"initContainers", "ephemeralContainers"} {
				if !strings.Contains(expr, key) {
					t.Errorf("validations[%d] must include images.%s (containers alone is incomplete)", i, key)
				}
			}
			// `.all(e, e >= 0)` / `.exists(...)` / a dropped `.all` are all
			// vacuously true for zero verified signatures — fail-open rot.
			if !allPositive.MatchString(expr) {
				t.Errorf("validations[%d] must require all(..., e > 0); >= 0 or exists() fail open", i)
			}
		}
		// Ephemeral coverage also needs the subresource routed to this policy.
		rules := asSlice(t, match["resourceRules"], "spec.matchConstraints.resourceRules")
		if !resourceRuleCoversEphemeralContainers(rules) {
			t.Error("matchConstraints.resourceRules must cover pods/ephemeralcontainers " +
				`on ""/v1 with CREATE+UPDATE (or *), alongside pods in the same rule`)
		}
	})

	t.Run("policy-controller", func(t *testing.T) {
		doc := mustLoadYAML(t, policyControllerSample)

		if got := asString(doc["kind"]); got != "ClusterImagePolicy" {
			t.Fatalf("kind = %q, want ClusterImagePolicy", got)
		}

		spec := asMap(t, doc["spec"], "spec")
		if got := asString(spec["mode"]); got != wantEnforceMode {
			t.Errorf("mode = %q, want enforce", got)
		}

		images := asSlice(t, spec["images"], "spec.images")
		var globs []string
		for _, img := range images {
			m := asMap(t, img, "images[]")
			if g := asString(m["glob"]); g != "" {
				globs = append(globs, g)
			}
		}
		assertNarrowManagerGlobs(t, globs)

		authorities := asSlice(t, spec["authorities"], "spec.authorities")
		// policy-controller admits if *any* authority verifies. A second looser
		// authority would neuter the pin while index-0 checks still pass.
		if len(authorities) != 1 {
			t.Fatalf("spec.authorities has %d entries, want exactly 1", len(authorities))
		}
		for i, raw := range authorities {
			auth := asMap(t, raw, "authorities[]")
			if got := asString(auth["signatureFormat"]); got != "bundle" {
				t.Errorf("authorities[%d].signatureFormat = %q, want bundle — chart-default legacy format "+
					"cannot see NVCRE referrer signatures", i, got)
			}

			keyless := asMap(t, auth["keyless"], "authorities[].keyless")
			ids := asSlice(t, keyless["identities"], "keyless.identities")
			assertExactReleaseIdentity(t, ids)

			atts := asSlice(t, auth["attestations"], "authorities[].attestations")
			if !attestationPredicatePresent(atts, wantSignType) {
				t.Errorf("authorities[%d] attestations must include %s (bundle-format signature)", i, wantSignType)
			}
			if !attestationPredicatePresent(atts, wantProvenanceType) {
				t.Errorf("authorities[%d] attestations must include %s", i, wantProvenanceType)
			}
		}
	})
}

func TestPolicySamplesAreMentionedOnVerificationPage(t *testing.T) {
	raw, err := os.ReadFile(verificationPage)
	if err != nil {
		t.Fatalf("read %s: %v", verificationPage, err)
	}
	page := string(raw)
	for _, path := range []string{
		"config/samples/policy/kyverno-verify-images.yaml",
		"config/samples/policy/policy-controller-verify-images.yaml",
	} {
		if !strings.Contains(page, path) {
			t.Errorf("%s does not link %s", verificationPage, path)
		}
	}
	if !strings.Contains(page, "signatureFormat: bundle") {
		t.Error("verification page must tell operators about signatureFormat: bundle")
	}
	if !strings.Contains(page, "ImageValidatingPolicy") {
		t.Error("verification page must name ImageValidatingPolicy, not only ClusterPolicy")
	}
	if !strings.Contains(page, "ClusterPolicy") {
		t.Error("verification page must warn that ClusterPolicy/verifyImages cannot see our format")
	}
}

func mustLoadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Samples may be multi-doc; take the first document with a kind.
	for part := range strings.SplitSeq(string(raw), "\n---\n") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(part), &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if asString(doc["kind"]) != "" {
			return doc
		}
	}
	t.Fatalf("%s has no Kubernetes document with a kind", path)
	return nil
}

func asMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		t.Fatalf("%s: want map, got %T", what, v)
	}
	return m
}

func asSlice(t *testing.T, v any, what string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want slice, got %T", what, v)
	}
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func namespaceSelectorPinsOptIn(ns map[string]any) bool {
	if labels, ok := ns["matchLabels"].(map[string]any); ok {
		if asString(labels[wantNSLabel]) == wantEnforceMode {
			return true
		}
	}
	exprs, _ := ns["matchExpressions"].([]any)
	for _, e := range exprs {
		m, _ := e.(map[string]any)
		if asString(m["key"]) != wantNSLabel {
			continue
		}
		if asString(m["operator"]) != "In" {
			continue
		}
		if slices.Contains(asStringSlice(m["values"]), wantEnforceMode) {
			return true
		}
	}
	return false
}

func asStringSlice(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		out = append(out, asString(x))
	}
	return out
}

func imageGlobs(t *testing.T, v any, what string) []string {
	t.Helper()
	refs := asSlice(t, v, what)
	var out []string
	for _, r := range refs {
		m := asMap(t, r, what+"[]")
		if g := asString(m["glob"]); g != "" {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no glob entries", what)
	}
	return out
}

func assertNarrowManagerGlobs(t *testing.T, globs []string) {
	t.Helper()
	const repo = "ghcr.io/nvidia/cluster-readiness-engine/manager"
	var sawRepo bool
	for _, g := range globs {
		if g == repo || strings.HasPrefix(g, repo+":") || strings.HasPrefix(g, repo+"@") {
			sawRepo = true
		}
		// The parked sample used manager* and over-matched. Reject any glob
		// that keeps a wildcard immediately after "manager" without a
		// separator — manager* / manager** — while still allowing manager:* .
		if strings.Contains(g, "manager*") && !strings.Contains(g, "manager:*") &&
			!strings.Contains(g, "manager@") {
			t.Errorf("glob %q uses manager* and can over-match manager-debug / nested paths", g)
		}
	}
	if !sawRepo {
		t.Errorf("globs %v do not pin %s", globs, repo)
	}
}

// assertKyvernoSingleTightAttestor mirrors the policy-controller len==1 pin:
// Kyverno admits if any listed attestor verifies, so a second looser attestor
// would neuter the release identity while attestors[0]-only checks stayed green.
func assertKyvernoSingleTightAttestor(t *testing.T, spec map[string]any) string {
	t.Helper()
	atts := asSlice(t, spec["attestors"], "spec.attestors")
	if len(atts) != 1 {
		t.Fatalf("spec.attestors has %d entries, want exactly 1", len(atts))
	}
	var name string
	for i, raw := range atts {
		attestor := asMap(t, raw, "attestors[]")
		name = asString(attestor["name"])
		if name == "" {
			t.Fatalf("attestors[%d].name is empty", i)
		}
		cosign := asMap(t, attestor["cosign"], "attestors[].cosign")
		keyless := asMap(t, cosign["keyless"], "attestors[].cosign.keyless")
		ids := asSlice(t, keyless["identities"], "keyless.identities")
		assertExactReleaseIdentity(t, ids)
	}
	return name
}

// attestorSelector matches dotted and bracket CEL selectors. len(attestors)==1
// is the load-bearing pin; this is defence in depth on the CEL side.
var attestorSelector = regexp.MustCompile(
	`attestors(?:\.([A-Za-z_][A-Za-z0-9_]*)|\["([^"]+)"\]|\['([^']+)'\])`)

func assertValidationsReferenceOnlyAttestor(t *testing.T, joined, wantName string) {
	t.Helper()
	wantRef := "attestors." + wantName
	if !strings.Contains(joined, wantRef) &&
		!strings.Contains(joined, `attestors["`+wantName+`"]`) &&
		!strings.Contains(joined, `attestors['`+wantName+`']`) {
		t.Errorf("validations must reference %s", wantRef)
	}
	for _, m := range attestorSelector.FindAllStringSubmatch(joined, -1) {
		got := m[1] + m[2] + m[3]
		if got != wantName {
			t.Errorf("validations reference attestors.%s; only %s is defined "+
				"(a second looser attestor in CEL would neuter the pin)", got, wantRef)
		}
	}
}

func assertExactReleaseIdentity(t *testing.T, identities []any) {
	t.Helper()
	if len(identities) == 0 {
		t.Fatal("no keyless identities")
	}
	// Every identity must be tight. policy-controller / Kyverno accept if any
	// identity matches, so a wide-open second entry next to a good one neuters
	// the pin while a "found one good identity" check would still pass.
	for i, id := range identities {
		m := asMap(t, id, "identity")
		if asString(m["issuer"]) != wantIssuer {
			t.Errorf("identities[%d].issuer = %q, want %q", i, asString(m["issuer"]), wantIssuer)
		}
		subject := asString(m["subject"])
		subjectRE := asString(m["subjectRegExp"])
		if subject == "" && subjectRE == "" {
			t.Errorf("identities[%d] pins neither subject nor subjectRegExp", i)
			continue
		}
		if subject != "" {
			if !strings.HasPrefix(subject, wantSubject) {
				t.Errorf("identities[%d].subject = %q, want prefix %q", i, subject, wantSubject)
			}
			if strings.Contains(subject, ".+") || strings.Contains(subject, ".*") {
				t.Errorf("identities[%d].subject = %q looks like a regexp; use subjectRegExp: for patterns "+
					"(a regexp under subject: matches no SAN)", i, subject)
			}
		}
		if subjectRE != "" {
			if !strings.Contains(subjectRE, "NVIDIA/cluster-readiness-engine") {
				t.Errorf("identities[%d].subjectRegExp = %q does not name NVIDIA/cluster-readiness-engine", i, subjectRE)
			}
			if !strings.Contains(subjectRE, `attest\.yml`) && !strings.Contains(subjectRE, "attest.yml") {
				t.Errorf("identities[%d].subjectRegExp = %q does not name attest.yml", i, subjectRE)
			}
			if !strings.Contains(subjectRE, "refs/tags") {
				t.Errorf("identities[%d].subjectRegExp = %q does not require refs/tags", i, subjectRE)
			}
			if !strings.HasPrefix(subjectRE, "^") || !strings.HasSuffix(subjectRE, "$") {
				t.Errorf("identities[%d].subjectRegExp = %q must be anchored with ^...$", i, subjectRE)
			}
		}
	}
}

// resourceRuleCoversEphemeralContainers requires the subresource on a core
// v1 Pod rule with CREATE+UPDATE (or *), and pods in the same rule so a
// tidy-up that relocates the subresource under apps cannot keep the assertion
// green while the API server never routes it to the policy. A broken earlier
// rule must continue so a later correct core rule still counts.
func resourceRuleCoversEphemeralContainers(rules []any) bool {
	for _, r := range rules {
		m, _ := r.(map[string]any)
		resources := asStringSlice(m["resources"])
		if !slices.Contains(resources, "pods/ephemeralcontainers") {
			continue
		}
		if !slices.Contains(resources, "pods") {
			continue
		}
		if !slices.Contains(asStringSlice(m["apiGroups"]), "") {
			continue
		}
		if !slices.Contains(asStringSlice(m["apiVersions"]), "v1") {
			continue
		}
		if !operationsCoverPodAdmission(asStringSlice(m["operations"])) {
			continue
		}
		return true
	}
	return false
}

func operationsCoverPodAdmission(ops []string) bool {
	if slices.Contains(ops, "*") {
		return true
	}
	return slices.Contains(ops, "CREATE") && slices.Contains(ops, "UPDATE")
}

func attestationTypePresent(atts []any, want string) bool {
	for _, a := range atts {
		m, _ := a.(map[string]any)
		if intoto, ok := m["intoto"].(map[string]any); ok {
			if asString(intoto["type"]) == want {
				return true
			}
		}
		if asString(m["predicateType"]) == want {
			return true
		}
	}
	return false
}

func attestationPredicatePresent(atts []any, want string) bool {
	for _, a := range atts {
		m, _ := a.(map[string]any)
		if asString(m["predicateType"]) == want {
			return true
		}
	}
	return false
}

func expressionsJoined(vals []any) string {
	var b strings.Builder
	for _, v := range vals {
		m, _ := v.(map[string]any)
		b.WriteString(asString(m["expression"]))
		b.WriteByte('\n')
	}
	return b.String()
}

// Ensure the sample files exist where the docs say they do — a rename that
// updates only one side would otherwise leave a 404 link.
func TestPolicySampleFilesExist(t *testing.T) {
	for _, p := range []string{kyvernoSample, policyControllerSample} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", filepath.Clean(p), err)
		}
	}
}
