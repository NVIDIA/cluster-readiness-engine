// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	syaml "sigs.k8s.io/yaml"
)

const managerRoleName = "nvcre-manager-role"

// requiredAccess is one resource the controller must be able to act on, and
// the verbs it uses.
type requiredAccess struct {
	Group    string   `json:"group"`
	Resource string   `json:"resource"`
	Verbs    []string `json:"verbs"`
}

// TestHelmManagerRoleGrantsDependencyAccess checks the rendered manager
// ClusterRole against the access each testdata case requires. envtest runs as
// an admin, so a verb missing from the role passes every integration test and
// fails only on a cluster that enforces RBAC.
func TestHelmManagerRoleGrantsDependencyAccess(t *testing.T) {
	requireHelm(t)
	chartDir := chartDir(t)
	requireChartInputs(t, chartDir)

	rendered, err := helmTemplate(chartDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := clusterRoleRules(rendered, managerRoleName)
	if err != nil {
		t.Fatal(err)
	}

	p := testutil.TestCaseParser{
		Subdir:         "manager-role-dependency-access",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Required []requiredAccess `json:"required"`
		}
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		missing := []string{}
		for _, req := range input.Required {
			for _, verb := range req.Verbs {
				if !rulesAllow(rules, req.Group, req.Resource, verb) {
					missing = append(missing, verb+" "+qualifiedResource(req.Group, req.Resource))
				}
			}
		}

		data, err := json.MarshalIndent(map[string][]string{"missing": missing}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// clusterRoleRules returns the rules of the named ClusterRole in the rendered chart.
func clusterRoleRules(rendered []byte, name string) ([]rbacv1.PolicyRule, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		var obj unstructured.Unstructured
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode helm template output: %w", err)
		}
		if obj.GetKind() != "ClusterRole" || obj.GetName() != name {
			continue
		}
		var role rbacv1.ClusterRole
		if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &role); err != nil {
			return nil, fmt.Errorf("convert ClusterRole %s: %w", name, err)
		}
		return role.Rules, nil
	}
	return nil, fmt.Errorf("rendered chart has no ClusterRole %s", name)
}

// rulesAllow reports whether any rule grants verb on every object of
// group/resource. A rule limited by resourceNames does not count: the
// controller acts on objects whose names it generates.
func rulesAllow(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			continue
		}
		if matches(rule.APIGroups, group) && matches(rule.Resources, resource) && matches(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

func matches(values []string, want string) bool {
	return slices.Contains(values, want) || slices.Contains(values, rbacv1.ResourceAll)
}

// qualifiedResource formats a resource the way kubectl does: plain for the
// core group, resource.group otherwise.
func qualifiedResource(group, resource string) string {
	if group == "" {
		return resource
	}
	return resource + "." + group
}
