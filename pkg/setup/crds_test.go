// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"
)

// TestApplyChartCRDs drives the CRD reconciliation step of the [helm] phase
// (issue #145) against a real apiserver: envtest provides real server-side
// apply and field ownership, the same fixture shape as
// TestClassifierMatchesRealSSAConflict. Each case pre-applies any
// input_existing.yaml objects as field manager "helm" to stand in for the
// ownership a previous install left behind. (A real helm install creates
// CRDs with an Update operation rather than Apply; the force-ownership
// conflict path under test is the same either way, but the applyManagers
// column in the goldens reflects this Apply-based fixture, not the exact
// managedFields a real install would leave.) The case then feeds
// input_crds.yaml through applyChartCRDs one or more times. The golden file
// holds the printed transcript plus a summary of every CRD named in the
// inputs (generation, served versions, spec schema properties, apply field
// managers), so a skipped schema update, a lost field, or a non-idempotent
// re-run shows up as a diff.
//
// The test needs KUBEBUILDER_ASSETS (as cmd/integration does); `make test`
// exports it for the unit run too. The test skips when it is unset so a
// plain `go test ./pkg/setup/` still passes without envtest binaries.
func TestApplyChartCRDs(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest CRD apply fixture")
	}

	suite := &testutil.IntegrationTestSuite{}
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	ctx := context.Background()
	c := suite.Client

	p := testutil.TestCaseParser{
		Subdir:         "apply-chart-crds",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// Rounds is how many times applyChartCRDs runs against the same
			// manifests (default 1); 2 exercises the idempotent re-run.
			Rounds int `yaml:"rounds"`
		}
		if raw := tc.Inputs["input.yaml"]; raw != "" {
			if err := sigsyaml.Unmarshal([]byte(raw), &in); err != nil {
				return err
			}
		}
		if in.Rounds == 0 {
			in.Rounds = 1
		}

		for _, doc := range splitYAMLDocuments([]byte(tc.Inputs["input_existing.yaml"])) {
			obj, err := decodeUnstructured(doc)
			if err != nil {
				return fmt.Errorf("decode existing object: %w", err)
			}
			if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
				client.FieldOwner("helm")); err != nil {
				return fmt.Errorf("pre-apply existing object: %w", err)
			}
		}

		var buf bytes.Buffer
		for round := 1; round <= in.Rounds; round++ {
			_, _ = fmt.Fprintf(&buf, "--- apply round %d ---\n", round)
			if err := applyChartCRDs(ctx, c, []byte(tc.Inputs["input_crds.yaml"]), &buf); err != nil {
				return err
			}
		}

		buf.WriteString("--- cluster state ---\n")
		for _, name := range inputCRDNames(tc.Inputs) {
			line, err := summarizeCRD(ctx, c, name)
			if err != nil {
				return err
			}
			buf.WriteString(line)
		}
		tc.Actual = buf.String()
		return nil
	})
}

// inputCRDNames collects the metadata.name of every CustomResourceDefinition
// document across the case's input files, sorted and deduplicated. Documents
// that are not CRDs (or not decodable) are skipped.
func inputCRDNames(inputs map[string]string) []string {
	seen := map[string]bool{}
	var names []string
	for _, key := range []string{"input_existing.yaml", "input_crds.yaml"} {
		for _, doc := range splitYAMLDocuments([]byte(inputs[key])) {
			obj, err := decodeUnstructured(doc)
			if err != nil || obj.GetKind() != "CustomResourceDefinition" {
				continue
			}
			if name := obj.GetName(); name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// summarizeCRD renders one deterministic line about the live CRD: generation
// (bumps only when the spec actually changed), served version names, the
// sorted spec-schema property names of the first version, and the sorted
// field managers whose operation is Apply. Update-operation managers (the
// apiserver's own status writes) are excluded because their timing is racy.
func summarizeCRD(ctx context.Context, c client.Client, name string) (string, error) {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		return "", fmt.Errorf("get CRD %s: %w", name, err)
	}

	var versions, props []string
	specVersions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for i, v := range specVersions {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := vm["name"].(string); name != "" {
			versions = append(versions, name)
		}
		if i == 0 {
			propsMap, _, _ := unstructured.NestedMap(vm,
				"schema", "openAPIV3Schema", "properties", "spec", "properties")
			for k := range propsMap {
				props = append(props, k)
			}
			sort.Strings(props)
		}
	}

	managerSet := map[string]bool{}
	for _, mf := range crd.GetManagedFields() {
		if mf.Operation == metav1.ManagedFieldsOperationApply {
			managerSet[mf.Manager] = true
		}
	}
	managers := make([]string, 0, len(managerSet))
	for m := range managerSet {
		managers = append(managers, m)
	}
	sort.Strings(managers)

	return fmt.Sprintf(
		"crd: %s generation=%d versions=[%s] specProperties=[%s] applyManagers=[%s]\n",
		name, crd.GetGeneration(), strings.Join(versions, " "),
		strings.Join(props, " "), strings.Join(managers, " ")), nil
}
