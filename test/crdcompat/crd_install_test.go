// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crdcompat checks that every CRD the chart ships installs on every
// Kubernetes minor NVCRE supports (ADR-105).
//
// The API server estimates the cost of each CEL validation rule when a CRD is
// created and refuses the whole CRD when a rule is over budget, and the
// estimator differs between minors. Every release before this test existed
// shipped a Certification CRD that 1.29 through 1.33 refused, while the
// documented minimum said 1.29, because envtest only ever ran the version
// go.mod implies.
// `make test-crd-compat` runs TestCRDsInstall once per minor from
// MIN_K8S_VERSION up; `make test` runs it once, against that default version.
package crdcompat

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	crdDir       = "../../helm/cluster-readiness-engine/crds"
	makefile     = "../../Makefile"
	installGuide = "../../docs/getting-started/install.md"

	// k8sVersionEnv is set by `make test-crd-compat` to the minor it pointed
	// KUBEBUILDER_ASSETS at. The test checks the server really is that minor,
	// so a wrong asset path cannot quietly test the default version eight
	// times and pass.
	k8sVersionEnv = "NVCRE_CRD_COMPAT_K8S_VERSION"
)

// TestCRDsInstall creates every chart CRD on a bare envtest API server and
// fails with the server's own message for each one it refuses.
func TestCRDsInstall(t *testing.T) {
	want := os.Getenv(k8sVersionEnv)
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if want != "" {
			t.Fatalf("%s=%s but KUBEBUILDER_ASSETS is not set", k8sVersionEnv, want)
		}
		t.Skip("KUBEBUILDER_ASSETS not set; skipping the envtest CRD install check")
	}

	crds := readChartCRDs(t)

	suite := &testutil.IntegrationTestSuite{}
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	got := serverMinor(t, suite.Config)
	if want != "" && got != want {
		t.Fatalf("API server is Kubernetes %s, want %s: KUBEBUILDER_ASSETS points at the wrong envtest binaries", got, want)
	}
	t.Logf("installing %d CRDs on Kubernetes %s", len(crds), got)

	for _, crd := range crds {
		t.Run(crd.GetName(), func(t *testing.T) {
			if err := suite.Client.Create(context.Background(), crd); err != nil {
				t.Errorf("Kubernetes %s refuses CRD %s: %v", got, crd.GetName(), err)
			}
		})
	}
}

// TestDocumentedFloorIsTested keeps the minimum the install guide states equal
// to the minimum `make test-crd-compat` starts from, so the guide cannot claim
// a version nothing installs the CRDs on.
func TestDocumentedFloorIsTested(t *testing.T) {
	tested := soleMatch(t, makefile, regexp.MustCompile(`(?m)^MIN_K8S_VERSION \?= (\d+\.\d+)$`))
	documented := soleMatch(t, installGuide, regexp.MustCompile(`(?m)^- Kubernetes (\d+\.\d+)\+ `))
	if documented != tested {
		t.Errorf("%s states Kubernetes %s+, but MIN_K8S_VERSION in %s is %s; change both together",
			installGuide, documented, makefile, tested)
	}
}

// readChartCRDs decodes every CustomResourceDefinition document in the chart's
// crds directory, failing if it finds none.
func readChartCRDs(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crds []*unstructured.Unstructured
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- fixed repository path
		if err != nil {
			t.Fatal(err)
		}
		reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			doc, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			obj := &unstructured.Unstructured{}
			if err := sigsyaml.Unmarshal(doc, &obj.Object); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if obj.GetKind() == "CustomResourceDefinition" {
				crds = append(crds, obj)
			}
		}
	}
	if len(crds) == 0 {
		t.Fatalf("no CustomResourceDefinitions found in %s", crdDir)
	}
	return crds
}

// serverMinor returns the API server's version as "major.minor".
func serverMinor(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	info, err := dc.ServerVersion()
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^v(\d+\.\d+)\.`).FindStringSubmatch(info.GitVersion)
	if m == nil {
		t.Fatalf("cannot parse API server version %q", info.GitVersion)
	}
	return m[1]
}

// soleMatch returns the first capture group of the one place re matches in the
// file at path, failing if it matches nowhere or more than once.
func soleMatch(t *testing.T, path string, re *regexp.Regexp) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed repository path
	if err != nil {
		t.Fatal(err)
	}
	matches := re.FindAllSubmatch(raw, -1)
	if len(matches) != 1 {
		t.Fatalf("%s: want exactly one match for %s, found %d", path, re, len(matches))
	}
	return string(matches[0][1])
}
