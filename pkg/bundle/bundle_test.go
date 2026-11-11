// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Write lays out the bundle ADR-090 defines and Verify enforces its three
// reader rules. These pin both halves against each other: every case writes a
// real bundle to a real directory, then reads it back through the same reader
// a consumer would use.
//
// The golden is the manifest plus the directory listing, because the manifest
// IS the contract. Its digests are what a consumer checks, its file count is
// what catches a short write, and its component counts are what assert the
// result set is whole. Writing those into a golden means a change to any of
// them has to be stated rather than absorbed.
//
// Digests are real sha256 values over deterministic content, so they belong in
// the golden too: a change that alters a bundle's bytes without anyone
// intending it shows up here as a digest change rather than silently
// invalidating every previously written bundle.
func TestWriteBundle(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "write",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		in, err := parseWriteCase(tc)
		if err != nil {
			return err
		}

		dir := t.TempDir()
		if err := Write(dir, in.contents()); err != nil {
			return err
		}

		// Read it back through the reader rules, not by inspecting what we
		// just held in memory.
		m, err := Verify(dir)
		if err != nil {
			return fmt.Errorf("the bundle just written does not verify: %w", err)
		}

		listing, err := walkBundleFiles(dir)
		if err != nil {
			return err
		}

		out := struct {
			Manifest  *Manifest `json:"manifest"`
			OnDisk    []string  `json:"filesOnDisk"`
			RowsFirst string    `json:"firstComponentRow,omitempty"`
		}{Manifest: m, OnDisk: listing}

		// The first NDJSON line, read off disk, so the golden states what a
		// consumer actually gets rather than only how many bytes it is.
		if len(in.Components) > 0 {
			body, readErr := os.ReadFile(
				filepath.Join(dir, ComponentsDir, in.Components[0].resolved().FileName()))
			if readErr != nil {
				return readErr
			}
			if line, _, found := strings.Cut(string(body), "\n"); found {
				out.RowsFirst = line
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Verify refuses a tampered bundle. Each case writes a good bundle, breaks it
// one way, and records the refusal.
//
// The error text is the golden because it is the whole user-facing product of
// a failed verification: a consumer who gets a refusal needs to know which
// file disagreed and how, and a message that degrades to "bundle invalid"
// would pass a weaker test while being useless in the field.
func TestVerifyRefusesTamperedBundle(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "verify",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// Tamper names the single change made to a good bundle.
			Tamper string `yaml:"tamper"`
			// Target is the file the tamper applies to, where it needs one.
			Target string `yaml:"target"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		dir := t.TempDir()
		if err := Write(dir, goodContents()); err != nil {
			return err
		}
		if _, err := Verify(dir); err != nil {
			return fmt.Errorf("fixture bundle must verify before it is tampered with: %w", err)
		}

		if err := applyTamper(dir, in.Tamper, in.Target); err != nil {
			return err
		}

		_, err := Verify(dir)
		if err == nil {
			tc.Actual = "ACCEPTED (the tamper was not detected)\n"
			return nil
		}
		// Temp directory paths vary per run; the message is what is pinned.
		tc.Actual = strings.ReplaceAll(err.Error(), dir, "<bundle>") + "\n"
		return nil
	})
}

// applyTamper makes one change to a written bundle.
func applyTamper(dir, tamper, target string) error {
	path := filepath.Join(dir, filepath.FromSlash(target))
	switch tamper {
	case "remove-manifest":
		return os.Remove(filepath.Join(dir, ManifestName))

	case "edit-file-in-place":
		// The case the digest exists for. Same byte count, different content,
		// so a file-count check and a size check both still pass.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		edited := swapFirstDigit(data)
		if string(edited) == string(data) {
			return fmt.Errorf("tamper fixture %s has no digit to flip", target)
		}
		return os.WriteFile(path, edited, 0o644)

	case "truncate-file":
		return os.WriteFile(path, []byte("{"), 0o644)

	case "delete-listed-file":
		return os.Remove(path)

	case "add-unlisted-file":
		return os.WriteFile(filepath.Join(dir, "stowaway.json"), []byte("{}\n"), 0o644)

	case "wrong-file-count":
		return rewriteManifest(dir, func(m *Manifest) { m.FileCount++ })

	case "unknown-format-version":
		return rewriteManifest(dir, func(m *Manifest) { m.FormatVersion = FormatVersion + 1 })

	case "manifest-lists-itself":
		return rewriteManifest(dir, func(m *Manifest) {
			m.Files = append(m.Files, FileEntry{Path: ManifestName, SHA256: "x", Bytes: 1})
			m.FileCount = len(m.Files)
		})

	case "manifest-path-escape":
		return rewriteManifest(dir, func(m *Manifest) {
			m.Files = append(m.Files, FileEntry{
				Path: "../../etc/passwd", SHA256: strings.Repeat("0", 64), Bytes: 1,
			})
			m.FileCount = len(m.Files)
		})

	case "corrupt-manifest-json":
		return os.WriteFile(filepath.Join(dir, ManifestName), []byte("{not json"), 0o644)

	default:
		return fmt.Errorf("unknown tamper %q", tamper)
	}
}

func rewriteManifest(dir string, mutate func(*Manifest)) error {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	mutate(&m)
	out, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(out, '\n'), 0o644)
}

// swapFirstDigit changes one digit, preserving length. A same-length edit is
// the point: it is what a file count and a byte count cannot catch.
func swapFirstDigit(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i, c := range out {
		if c >= '0' && c <= '8' {
			out[i] = c + 1
			return out
		}
	}
	return out
}

// writeCase is a Write test's input.
type writeCase struct {
	Reports    []reportStub    `yaml:"reports"`
	Components []componentStub `yaml:"components"`
	// Truncations are written to truncation.json.
	Truncations []TruncationRecord `yaml:"truncations"`
}

type reportStub struct {
	Name   string `yaml:"name"`
	Status string `yaml:"status"`
}

type componentStub struct {
	Scope             string   `yaml:"scope"`
	Domain            string   `yaml:"domain"`
	Variant           string   `yaml:"variant"`
	Rows              []string `yaml:"rows"`
	ShardCount        int32    `yaml:"shardCount"`
	Final             bool     `yaml:"final"`
	TruncatedRowCount int64    `yaml:"truncatedRowCount"`
}

func (c componentStub) resolved() ResolvedComponent {
	var body strings.Builder
	for _, r := range c.Rows {
		body.WriteString(r)
		body.WriteByte('\n')
	}
	return ResolvedComponent{
		Scope: c.Scope, Domain: c.Domain, Variant: c.Variant,
		NDJSON:            []byte(body.String()),
		ShardCount:        c.ShardCount,
		RowCount:          int64(len(c.Rows)),
		Final:             c.Final,
		TruncatedRowCount: c.TruncatedRowCount,
	}
}

func (w writeCase) contents() Contents {
	c := Contents{Truncations: w.Truncations}
	for _, r := range w.Reports {
		c.Reports = append(c.Reports, &report.CertReport{Name: r.Name, Result: r.Status})
	}
	for _, comp := range w.Components {
		c.Components = append(c.Components, comp.resolved())
	}
	return c
}

func parseWriteCase(tc *testutil.TestCase) (writeCase, error) {
	var in writeCase
	err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in)
	return in, err
}

// goodContents is the fixture every verify case starts from: a small bundle
// that verifies.
func goodContents() Contents {
	return Contents{
		Reports: []*report.CertReport{{Name: "cert-a", Result: "PASSED"}},
		Components: []ResolvedComponent{{
			Scope: "cert-a", Domain: "compute", Variant: "cutlass-gemm",
			NDJSON: []byte(`{"node":"gpu-node-1","componentID":"GPU-cafe","index":0,` +
				`"name":"achievedTFLOPS","value":1460.00,"unit":"TFLOPS"}` + "\n"),
			ShardCount: 1, RowCount: 1, Final: true,
		}},
	}
}
