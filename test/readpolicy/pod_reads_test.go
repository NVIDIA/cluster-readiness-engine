// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package readpolicy checks that the controller reads Pods only through
// pkg/podread (ADR-102, decision 4).
//
// The manager's cache holds only Pods with the NVCRE job label, so whether a
// Pod read may go to the cache depends on which pods it needs; pkg/podread is
// where that is decided. Every Pod list needs a corev1.PodList to read into,
// so a package the manager links that names that type outside pkg/podread is
// reading Pods on its own. The cache guard catches such a read at run time
// only if a test exercises it; this catches it in any file.
package readpolicy

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath = "github.com/NVIDIA/cluster-readiness-engine"
	corev1Path = "k8s.io/api/core/v1"
	podreadPkg = modulePath + "/pkg/podread"
)

// podListUses returns the positions in src where the corev1 PodList type is
// named, whatever the core/v1 import is called.
func podListUses(fset *token.FileSet, filename string, src []byte) ([]string, error) {
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	alias := ""
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != corev1Path {
			continue
		}
		alias = "v1"
		if imp.Name != nil {
			alias = imp.Name.Name
		}
	}
	if alias == "" || alias == "_" {
		return nil, nil
	}
	var uses []string
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PodList" {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == alias {
			uses = append(uses, fset.Position(sel.Pos()).String())
		}
		return true
	})
	return uses, nil
}

// TestPodListDetector keeps the detector honest: a policy that cannot see a
// Pod list would pass on any code.
func TestPodListDetector(t *testing.T) {
	tests := []struct {
		name string
		src  string
		hits int
	}{
		{"corev1 alias, composite literal", `package p
import corev1 "k8s.io/api/core/v1"
func f() { _ = &corev1.PodList{} }`, 1},
		{"default import name, var declaration", `package p
import "k8s.io/api/core/v1"
func f() { var pods v1.PodList; _ = pods }`, 1},
		{"other alias", `package p
import core "k8s.io/api/core/v1"
func f(p *core.PodList) {}`, 1},
		{"a Pod alone is not a list", `package p
import corev1 "k8s.io/api/core/v1"
func f() { _ = corev1.Pod{} }`, 0},
		{"another package's PodList", `package p
import corev1 "example.com/other/v1"
func f() { _ = corev1.PodList{} }`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uses, err := podListUses(token.NewFileSet(), "p.go", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			if len(uses) != tt.hits {
				t.Fatalf("got %d PodList uses %v, want %d", len(uses), uses, tt.hits)
			}
		})
	}
}

// managerPackageDirs returns the directory of every package in this module
// that the manager binary links, keyed by import path.
func managerPackageDirs(t *testing.T) map[string]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f",
		`{{if .Module}}{{if eq .Module.Path "`+modulePath+`"}}{{.ImportPath}} {{.Dir}}{{end}}{{end}}`,
		"./cmd/manager")
	cmd.Dir = filepath.Join("..", "..")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/manager: %v\n%s", err, stderr.String())
	}
	dirs := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if path, dir, ok := strings.Cut(line, " "); ok {
			dirs[path] = dir
		}
	}
	if _, ok := dirs[podreadPkg]; !ok {
		t.Fatalf("go list found no %s among the manager's packages; the listing is wrong", podreadPkg)
	}
	return dirs
}

// TestControllerReadsPodsOnlyThroughPodread fails when a non-test file in a
// package the manager links, other than pkg/podread, names corev1.PodList.
func TestControllerReadsPodsOnlyThroughPodread(t *testing.T) {
	dirs := managerPackageDirs(t)
	fset := token.NewFileSet()
	var violations []string
	for path, dir := range dirs {
		if path == podreadPkg {
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			uses, err := podListUses(fset, f, src)
			if err != nil {
				t.Fatal(err)
			}
			violations = append(violations, uses...)
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("Pods are read outside pkg/podread (ADR-102); add a podread method that picks the cache or "+
			"the API server, and call it instead:\n%s", strings.Join(violations, "\n"))
	}
}
