// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const updateEnvVar = "TESTUTIL_UPDATE_EXPECTED"

const (
	// SuffixJSON is the default golden-file suffix for JSON expected output.
	SuffixJSON = ".json"
	// SuffixTXT is the golden-file suffix for plain-text expected output.
	SuffixTXT = ".txt"
)

// TestCase is one golden-file test scenario (a testdata subdirectory).
type TestCase struct {
	T    testing.TB
	Name string
	// Inputs maps every non-expected file in the scenario directory to its
	// raw content string, keyed by base filename (e.g. "input_config.yaml").
	Inputs map[string]string
	// Actual is populated by the test body and compared against the expected
	// file after fn returns (or written to it when TESTUTIL_UPDATE_EXPECTED=true).
	Actual string
}

// GetObjects decodes all YAML/YML documents from the scenario's input files
// into registered k8s objects. Unrecognised documents (e.g. custom config
// structs) are returned as raw bytes in the second slice and can be ignored.
func (tc *TestCase) GetObjects(s *runtime.Scheme) ([]client.Object, []runtime.RawExtension, error) {
	dec := serializer.NewCodecFactory(s).UniversalDeserializer()

	var objs []client.Object
	var raws []runtime.RawExtension

	for name, content := range tc.Inputs {
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(content)))
		for {
			doc, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("reading %s: %w", name, err)
			}
			doc = bytes.TrimSpace(doc)
			if len(doc) == 0 || string(doc) == "---" {
				continue
			}
			obj, _, decErr := dec.Decode(doc, nil, nil)
			if decErr != nil {
				raws = append(raws, runtime.RawExtension{Raw: doc})
				continue
			}
			co, ok := obj.(client.Object)
			if !ok {
				raws = append(raws, runtime.RawExtension{Raw: doc})
				continue
			}
			objs = append(objs, co)
		}
	}
	return objs, raws, nil
}

// TestCaseParser discovers and drives golden-file tests under testdata/<Subdir>.
type TestCaseParser struct {
	// Subdir is the subdirectory of testdata/ that contains the scenario dirs.
	Subdir string
	// ExpectedSuffix is the suffix of the golden file (default: ".json").
	// The expected file is named "expected" + ExpectedSuffix.
	ExpectedSuffix string
	// NameSuffix is unused; retained for API compatibility.
	NameSuffix string
}

// TestDir discovers all first-level subdirectories of testdata/<p.Subdir>,
// builds a TestCase for each, calls fn, then either compares tc.Actual to
// the golden file or (when TESTUTIL_UPDATE_EXPECTED=true) overwrites it.
func (p *TestCaseParser) TestDir(t *testing.T, fn func(tc *TestCase) error) {
	t.Helper()

	testdataDir := filepath.Join("testdata", p.Subdir)
	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("reading testdata dir %s: %v", testdataDir, err)
	}

	update := os.Getenv(updateEnvVar) == "true"
	suffix := p.ExpectedSuffix
	if suffix == "" {
		suffix = SuffixJSON
	}
	expectedFile := "expected" + suffix

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseName := entry.Name()
		caseDir := filepath.Join(testdataDir, caseName)

		tc := &TestCase{
			Name:   caseName,
			Inputs: make(map[string]string),
		}

		var expectedContent string
		var expectedError string
		_ = filepath.WalkDir(caseDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return walkErr
			}
			base := filepath.Base(path)
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			switch base {
			case expectedFile:
				expectedContent = string(raw)
			case "error":
				// error file contains the expected error message for negative test cases
				expectedError = strings.TrimRight(string(raw), "\n\r")
			default:
				tc.Inputs[base] = string(raw)
			}
			return nil
		})

		// Match the original usage-metrics-collector naming: "subdir/caseName".
		// This keeps t.Name() stable so that hash-based names (e.g. node prefixes
		// generated from sha256(t.Name()) in the integration tests) stay identical
		// to what the golden files were written with.
		subtestName := p.Subdir + "/" + caseName + p.NameSuffix
		t.Run(subtestName, func(t *testing.T) {
			tc.T = t
			err := fn(tc)

			if expectedError != "" {
				// Negative test case: fn must return an error whose message
				// contains the expected error string (substring match, matching
				// the upstream usage-metrics-collector behaviour).
				if err == nil {
					t.Fatalf("expected error containing %q but got nil", expectedError)
				}
				if !strings.Contains(err.Error(), expectedError) {
					t.Errorf("error mismatch for test case %q\n\nwant (substr): %s\n\ngot: %s",
						caseName, expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("test case %s: %v", caseName, err)
			}

			if update {
				if err := os.WriteFile(
					filepath.Join(caseDir, expectedFile),
					[]byte(tc.Actual), 0o644,
				); err != nil {
					t.Fatalf("updating expected file: %v", err)
				}
				return
			}

			// TrimSpace on both sides (matching the original library), so
			// golden files written with a trailing newline compare equal to
			// output from json.MarshalIndent which omits it.
			if strings.TrimSpace(tc.Actual) != strings.TrimSpace(expectedContent) {
				t.Errorf("output mismatch for test case %q\n\nwant:\n%s\n\ngot:\n%s",
					caseName, expectedContent, tc.Actual)
			}
		})
	}
}
