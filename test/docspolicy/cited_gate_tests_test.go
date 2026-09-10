// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package docspolicy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Pages that cite releasepolicy / docspolicy gate tests by name. Renaming a
// Test* without updating these pages would leave the prose asserting
// enforcement by a test that no longer exists; nothing else catches that.
var gateTestCitePages = []string{
	"../../SECURITY.md",
	"../../docs/operations/verifying-artifacts.md",
}

// testIdent matches a Go test function identifier cited in prose.
var testIdent = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]+\b`)

// TestCitedGateTestsExist fails when SECURITY.md or verifying-artifacts.md
// names a Test* that is not defined under test/releasepolicy/ or
// test/docspolicy/. Keeps the gate-test enumeration honest without requiring
// the list to be duplicated verbatim in both pages.
func TestCitedGateTestsExist(t *testing.T) {
	defined := definedTests(t, []string{"../releasepolicy", "."})

	for _, page := range gateTestCitePages {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		seen := map[string]bool{}
		for _, m := range testIdent.FindAllString(string(raw), -1) {
			if seen[m] {
				continue
			}
			seen[m] = true
			if !defined[m] {
				t.Errorf("%s cites %s, but no such test exists under test/releasepolicy or test/docspolicy",
					filepath.Base(page), m)
			}
		}
		if len(seen) == 0 && filepath.Base(page) == "SECURITY.md" {
			t.Errorf("%s cites no Test* gate names; the Build Level paragraph must name the tests behind the claim",
				filepath.Base(page))
		}
	}
}

func definedTests(t *testing.T, dirs []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Name == nil {
					continue
				}
				name := fn.Name.Name
				if strings.HasPrefix(name, "Test") {
					out[name] = true
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Test* functions found under the scanned packages")
	}
	return out
}
