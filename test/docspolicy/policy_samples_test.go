// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package docspolicy

import (
	"os"
	"path/filepath"
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

		match := asMap(t, spec["matchConstraints"], "spec.matchConstraints")
		ns := asMap(t, match["namespaceSelector"], "spec.matchConstraints.namespaceSelector")
		if !namespaceSelectorPinsOptIn(ns) {
			t.Error("matchConstraints.namespaceSelector must opt in via " + wantNSLabel +
				"=enforce; without it failurePolicy: Fail is cluster-wide")
		}

		globs := imageGlobs(t, spec["matchImageReferences"], "matchImageReferences")
		assertNarrowManagerGlobs(t, globs)

		identities := kyvernoIdentities(t, spec)
		assertExactReleaseIdentity(t, identities)

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
	})

	t.Run("policy-controller", func(t *testing.T) {
		doc := mustLoadYAML(t, policyControllerSample)

		if got := asString(doc["kind"]); got != "ClusterImagePolicy" {
			t.Fatalf("kind = %q, want ClusterImagePolicy", got)
		}

		spec := asMap(t, doc["spec"], "spec")
		if got := asString(spec["mode"]); got != "enforce" {
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
		if len(authorities) == 0 {
			t.Fatal("spec.authorities is empty")
		}
		auth := asMap(t, authorities[0], "authorities[0]")
		if got := asString(auth["signatureFormat"]); got != "bundle" {
			t.Errorf("signatureFormat = %q, want bundle — chart-default legacy format "+
				"cannot see NVCRE referrer signatures", got)
		}

		keyless := asMap(t, auth["keyless"], "authorities[0].keyless")
		ids := asSlice(t, keyless["identities"], "keyless.identities")
		assertExactReleaseIdentity(t, ids)

		atts := asSlice(t, auth["attestations"], "authorities[0].attestations")
		if !attestationPredicatePresent(atts, wantSignType) {
			t.Errorf("attestations must include %s (bundle-format signature)", wantSignType)
		}
		if !attestationPredicatePresent(atts, wantProvenanceType) {
			t.Errorf("attestations must include %s", wantProvenanceType)
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
	// Samples may be multi-doc; take the first non-empty document.
	for _, part := range strings.Split(string(raw), "\n---\n") {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "#") && !strings.Contains(part, "\nkind:") {
			// Still try to unmarshal; comments before kind are fine.
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
		if asString(labels[wantNSLabel]) == "enforce" {
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
		for _, v := range asStringSlice(m["values"]) {
			if v == "enforce" {
				return true
			}
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

func kyvernoIdentities(t *testing.T, spec map[string]any) []any {
	t.Helper()
	atts := asSlice(t, spec["attestors"], "spec.attestors")
	if len(atts) == 0 {
		t.Fatal("spec.attestors is empty")
	}
	attestor := asMap(t, atts[0], "attestors[0]")
	cosign := asMap(t, attestor["cosign"], "attestors[0].cosign")
	keyless := asMap(t, cosign["keyless"], "attestors[0].cosign.keyless")
	return asSlice(t, keyless["identities"], "keyless.identities")
}

func assertExactReleaseIdentity(t *testing.T, identities []any) {
	t.Helper()
	if len(identities) == 0 {
		t.Fatal("no keyless identities")
	}
	found := false
	for _, id := range identities {
		m := asMap(t, id, "identity")
		if asString(m["issuer"]) != wantIssuer {
			t.Errorf("issuer = %q, want %q", asString(m["issuer"]), wantIssuer)
		}
		subject := asString(m["subject"])
		subjectRE := asString(m["subjectRegExp"])
		if subject != "" {
			if !strings.HasPrefix(subject, wantSubject) {
				t.Errorf("subject = %q, want prefix %q", subject, wantSubject)
			}
			if strings.Contains(subject, ".+") || strings.Contains(subject, ".*") {
				t.Errorf("subject = %q looks like a regexp; use subjectRegExp: for patterns "+
					"(a regexp under subject: matches no SAN)", subject)
			}
			found = true
		}
		if subjectRE != "" {
			// Optional loosening is fine in comments; a live subjectRegExp must
			// still name attest.yml and refs/tags.
			if !strings.Contains(subjectRE, "attest\\.yml") && !strings.Contains(subjectRE, "attest.yml") {
				t.Errorf("subjectRegExp = %q does not name attest.yml", subjectRE)
			}
			if !strings.Contains(subjectRE, "refs/tags") {
				t.Errorf("subjectRegExp = %q does not anchor refs/tags", subjectRE)
			}
			found = true
		}
	}
	if !found {
		t.Error("no identity pins subject or subjectRegExp")
	}
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
