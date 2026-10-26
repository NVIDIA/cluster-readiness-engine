// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	syaml "sigs.k8s.io/yaml"
)

const crdKind = "CustomResourceDefinition"

// includeCRDs makes helm template render a chart's crds/ directory, which it
// leaves out by default. Without it the subchart's CRDs never appear.
const includeCRDs = "--include-crds"

// crdView is the part of a rendered CRD the CRDs chart adds: its labels and
// annotations. The spec is compared against the subchart separately.
type crdView struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func crdsChartDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(filepath.Dir(chartDir(t)), "cluster-readiness-engine-crds")
}

func subchartCRDDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), "charts", "crds", "crds")
}

func TestCRDsChartRenders(t *testing.T) {
	requireHelm(t)
	dir := crdsChartDir(t)
	requireChartInputs(t, dir)

	p := testutil.TestCaseParser{
		Subdir:         "render-crds-chart",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Set []string `yaml:"set"`
		}
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		rendered, err := helmTemplate(dir, input.Set)
		if err != nil {
			return err
		}
		crds, err := crdsFromYAML(rendered)
		if err != nil {
			return err
		}
		views := make([]crdView, 0, len(crds))
		for _, crd := range crds {
			views = append(views, crdView{
				Name:        crd.GetName(),
				Labels:      crd.GetLabels(),
				Annotations: crd.GetAnnotations(),
			})
		}

		data, err := json.MarshalIndent(views, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestCRDsSubchartFollowsEnabled covers crds.enabled on the main chart: the
// value is the only switch tools without a skip-CRDs flag can reach (ADR-107).
func TestCRDsSubchartFollowsEnabled(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	p := testutil.TestCaseParser{
		Subdir:         "render-crds-subchart",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Set []string `yaml:"set"`
		}
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		rendered, err := helmTemplate(dir, input.Set, includeCRDs)
		if err != nil {
			return err
		}
		crds, err := crdsFromYAML(rendered)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(crds))
		for _, crd := range crds {
			names = append(names, crd.GetName())
		}

		data, err := json.MarshalIndent(names, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestCRDsChartMatchesSubchart holds the two charts to one schema. The files
// are copies make manifests writes, and the CRDs chart re-serializes them
// through fromYaml and toYaml, so both the copy and the round trip are
// checked: a drifted file or a mangled field fails here, not on a cluster.
func TestCRDsChartMatchesSubchart(t *testing.T) {
	requireHelm(t)
	dir := crdsChartDir(t)
	requireChartInputs(t, dir)
	requireChartInputs(t, chartDir(t))

	want, err := crdsFromDir(subchartCRDDir(t))
	if err != nil {
		t.Fatalf("read subchart CRDs: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("the subchart holds no CRDs; run make manifests")
	}

	filesDir := filepath.Join(dir, "files")
	if err := sameFiles(subchartCRDDir(t), filesDir); err != nil {
		t.Fatalf("%s differs from the subchart; run make manifests: %v", filesDir, err)
	}

	rendered, err := helmTemplate(dir, nil)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	got, err := crdsFromYAML(rendered)
	if err != nil {
		t.Fatalf("decode rendered CRDs: %v", err)
	}

	if g, w := crdNames(got), crdNames(want); !reflect.DeepEqual(g, w) {
		t.Fatalf("rendered CRDs = %v, want the subchart's %v", g, w)
	}
	byName := map[string]unstructured.Unstructured{}
	for _, crd := range got {
		byName[crd.GetName()] = crd
	}
	for _, w := range want {
		g := byName[w.GetName()]
		if !reflect.DeepEqual(g.Object["spec"], w.Object["spec"]) {
			t.Errorf("CRD %s: rendered spec differs from the subchart's", w.GetName())
		}
		for key, value := range w.GetAnnotations() {
			if g.GetAnnotations()[key] != value {
				t.Errorf("CRD %s: annotation %s = %q, want the subchart's %q",
					w.GetName(), key, g.GetAnnotations()[key], value)
			}
		}
	}
}

// crdsFromYAML decodes every CustomResourceDefinition in a multi-document
// stream, sorted by name.
func crdsFromYAML(data []byte) ([]unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var crds []unstructured.Unstructured
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj.Object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode YAML: %w", err)
		}
		if obj.GetKind() != crdKind {
			continue
		}
		crds = append(crds, obj)
	}
	sort.Slice(crds, func(i, j int) bool { return crds[i].GetName() < crds[j].GetName() })
	return crds, nil
}

func crdsFromDir(dir string) ([]unstructured.Unstructured, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var all []byte
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		all = append(all, []byte("\n---\n")...)
		all = append(all, data...)
	}
	return crdsFromYAML(all)
}

func crdNames(crds []unstructured.Unstructured) []string {
	names := make([]string, 0, len(crds))
	for _, crd := range crds {
		names = append(names, crd.GetName())
	}
	return names
}

// sameFiles reports the first difference between two directories' YAML files.
func sameFiles(wantDir, gotDir string) error {
	list := func(dir string) ([]string, error) {
		paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(paths))
		for _, path := range paths {
			names = append(names, filepath.Base(path))
		}
		return names, nil
	}
	want, err := list(wantDir)
	if err != nil {
		return err
	}
	got, err := list(gotDir)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("files %v, want %v", got, want)
	}
	for _, name := range want {
		w, err := os.ReadFile(filepath.Join(wantDir, name))
		if err != nil {
			return err
		}
		g, err := os.ReadFile(filepath.Join(gotDir, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(g, w) {
			return fmt.Errorf("%s differs", name)
		}
	}
	return nil
}
