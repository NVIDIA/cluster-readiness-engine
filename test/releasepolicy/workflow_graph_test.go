// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const workflowDir = "../../.github/workflows"

// job is the subset of a workflow job these tests reason about.
type job struct {
	Needs   stringOrSlice     `json:"needs"`
	Uses    string            `json:"uses"`
	Outputs map[string]string `json:"outputs"`
	// Raw carries every remaining key, including `with:` on a reusable-workflow
	// call. Without it, marshalling a job back to YAML to scan for references
	// silently drops the very block those references live in -- which is how the
	// first version of this test passed against a defect it was written to catch.
	Raw map[string]any `json:"-"`
}

// wf is the subset of a workflow file these tests reason about. `on` is not
// modelled: YAML parses a bare `on:` key as the boolean true, and nothing here
// needs the trigger block.
type wf struct {
	Jobs map[string]job `json:"jobs"`
	// CallOutputs are the outputs a reusable workflow declares in its own
	// `workflow_call` block. Populated by hand rather than by a struct tag: YAML
	// 1.1 reads a bare `on:` key as the boolean true, so `json:"on"` never
	// matches and the block silently reads as empty.
	CallOutputs map[string]any
}

// stringOrSlice accepts both `needs: build` and `needs: [a, b]`.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalJSON(b []byte) error {
	var one string
	if err := yaml.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	var many []string
	if err := yaml.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

var needsRef = regexp.MustCompile(`needs\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)`)

func loadWorkflows(t *testing.T) map[string]wf {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no workflows found under %s", workflowDir)
	}

	out := map[string]wf{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var w wf
		if err := yaml.Unmarshal(raw, &w); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}

		// Re-parse untyped so every job key is available for the reference scan.
		var generic struct {
			Jobs map[string]map[string]any `json:"jobs"`
		}
		if err := yaml.Unmarshal(raw, &generic); err != nil {
			t.Fatalf("parse %s untyped: %v", p, err)
		}
		for jobName, body := range generic.Jobs {
			j := w.Jobs[jobName]
			j.Raw = body
			w.Jobs[jobName] = j
		}

		w.CallOutputs = workflowCallOutputs(raw, t)
		out[filepath.Base(p)] = w
	}
	return out
}

// TestJobOutputReferencesResolve is the check that a real defect got past.
//
// A `needs.<job>.outputs.<field>` reference is not validated by actionlint or by
// YAML parsing. When the release workflow's tag resolution was consolidated into
// a single job, a downstream `subject_tag: ${{ needs.helm-publish.outputs.tag }}`
// was left pointing at an output that no longer existed. It expands to the empty
// string, which meant the chart signing job would have failed on every release —
// after the image and chart were already public.
//
// Checking that the producing job is listed in `needs:` is not enough; the
// specific field has to still exist.
func TestJobOutputReferencesResolve(t *testing.T) {
	workflows := loadWorkflows(t)

	for name, w := range workflows {
		for jobName, j := range w.Jobs {
			body, err := yaml.Marshal(j.Raw)
			if err != nil {
				t.Fatalf("%s: re-marshal job %s: %v", name, jobName, err)
			}

			for _, m := range needsRef.FindAllStringSubmatch(string(body), -1) {
				producer, field := m[1], m[2]

				if !slices.Contains(j.Needs, producer) {
					t.Errorf("%s: job %q reads needs.%s.outputs.%s but does not declare %q in needs",
						name, jobName, producer, field, producer)
					continue
				}

				pj, ok := w.Jobs[producer]
				if !ok {
					t.Errorf("%s: job %q depends on %q, which does not exist", name, jobName, producer)
					continue
				}

				declared := pj.Outputs
				// A job that calls a reusable workflow declares its outputs in
				// the called file, not here.
				if pj.Uses != "" {
					called, ok := workflows[filepath.Base(pj.Uses)]
					if !ok {
						// Not a local workflow; nothing to resolve against.
						continue
					}
					declared = map[string]string{}
					for k := range called.CallOutputs {
						declared[k] = ""
					}
				}

				if _, ok := declared[field]; !ok {
					t.Errorf("%s: job %q reads needs.%s.outputs.%s, which %q does not declare (declares: %v)",
						name, jobName, producer, field, producer, sortedKeys(declared))
				}
			}
		}
	}
}

// TestNoExpressionInterpolationInRunBlocks keeps the release path free of
// `${{ }}` inside shell bodies. A git tag may contain `"`, `$( )`, backticks and
// `|`, so an interpolated value is executed rather than read. Values must cross
// into a shell through `env:`.
func TestNoExpressionInterpolationInRunBlocks(t *testing.T) {
	releasePath := map[string]bool{
		"release.yml":     true,
		"publish.yml":     true,
		"attest.yml":      true,
		"build-image.yml": true,
	}

	paths, err := filepath.Glob(filepath.Join(workflowDir, "*.yml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}

	var steps struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `json:"name"`
				Run  string `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}

	for _, p := range paths {
		base := filepath.Base(p)
		if !releasePath[base] {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		steps.Jobs = nil
		if err := yaml.Unmarshal(raw, &steps); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for jobName, j := range steps.Jobs {
			for _, s := range j.Steps {
				if strings.Contains(s.Run, "${{") {
					t.Errorf("%s: job %q step %q interpolates ${{ }} inside a run block; "+
						"pass the value through env: instead so it is data, not code",
						base, jobName, s.Name)
				}
			}
		}
	}
}

// workflowCallOutputs reads on.workflow_call.outputs, tolerating YAML 1.1
// turning the bare `on:` key into the boolean true.
func workflowCallOutputs(raw []byte, t *testing.T) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse workflow triggers: %v", err)
	}
	for _, key := range []string{"on", "true"} {
		trig, ok := doc[key].(map[string]any)
		if !ok {
			continue
		}
		call, ok := trig["workflow_call"].(map[string]any)
		if !ok {
			continue
		}
		if outs, ok := call["outputs"].(map[string]any); ok {
			return outs
		}
	}
	return nil
}

// TestVerificationUsesExactIdentity pins two habits that would each weaken
// verification silently.
//
// `--certificate-identity-regexp` is how the pre-epic SECURITY.md command
// accepted a main-branch build as a release: a pattern naming no workflow and no
// ref answers a weaker question than it appears to, and the weakening is
// invisible to whoever copies it. The command we publish is as much a part of
// the release path as the workflow that signs, so the published instructions are
// covered too — see TestPublishedVerifyCommandsAreExact.
//
// A bare `gh attestation verify` reads GitHub's Attestations API rather than the
// registry. `cosign attest` writes only to the registry, so the bare form would
// verify a copy that does not exist here — and where both copies do exist they
// can diverge, so it would pass on an artifact whose registry attestation is
// gone while a consumer using cosign gets nothing.
func TestVerificationUsesExactIdentity(t *testing.T) {
	releasePath := []string{"release.yml", "publish.yml", "attest.yml", "build-image.yml", "attest-selftest.yml"}

	for _, base := range releasePath {
		raw, err := os.ReadFile(filepath.Join(workflowDir, base))
		if err != nil {
			t.Fatalf("read %s: %v", base, err)
		}
		body := string(raw)

		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			// Comments may name these forms in order to warn against them.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(line, "certificate-identity-regexp") {
				t.Errorf("%s:%d uses --certificate-identity-regexp; use the exact "+
					"--certificate-identity, which names the workflow and the ref", base, i+1)
			}
			// Scoped to the line, not the file. Searching `body` would let one
			// compliant usage anywhere -- including inside a comment -- disarm
			// the check for every other occurrence in that file, so the test
			// would go green on exactly the silent weakening it exists to catch.
			if strings.Contains(line, "gh attestation verify") && !strings.Contains(line, "--bundle-from-oci") {
				t.Errorf("%s:%d uses a bare `gh attestation verify`; add --bundle-from-oci so it "+
					"reads the registry copy rather than the GitHub Attestations API", base, i+1)
			}
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestPublishedVerifyCommandsAreExact covers the surface the original defect
// actually shipped on: the instructions users copy.
//
// ADR-074 records that the pre-epic SECURITY.md command told users to pass
// `--certificate-identity-regexp`, which accepts a `main` build as a release.
// Fixing the workflows does not fix that; the published command is its own
// contract, and a weakened one converts an unverified install into a falsely
// verified one.
//
// Prose is allowed to name the flag — SECURITY.md warns against it by name, and
// a test that forbade the warning would be worse than no test. Only fenced
// command blocks are checked.
func TestPublishedVerifyCommandsAreExact(t *testing.T) {
	docs := []string{"../../SECURITY.md", "../../README.md", "../../RELEASE.md"}

	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}

		inFence := false
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence && strings.Contains(line, "certificate-identity-regexp") {
				t.Errorf("%s:%d publishes a command using --certificate-identity-regexp; "+
					"an identity naming no workflow and no ref also accepts a branch build, "+
					"so a user following it accepts a development image as a release",
					filepath.Base(path), i+1)
			}
		}
	}
}
