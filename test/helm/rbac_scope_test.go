// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	syaml "sigs.k8s.io/yaml"
)

// runRoleBindingView is one rendered binding of nvcre-run-role.
type runRoleBindingView struct {
	Kind      string           `json:"kind"`
	Name      string           `json:"name"`
	Namespace string           `json:"namespace,omitempty"`
	RoleRef   rbacv1.RoleRef   `json:"roleRef"`
	Subjects  []rbacv1.Subject `json:"subjects"`
}

// managerEnvView is a manager container env var set from the downward API.
type managerEnvView struct {
	Name      string `json:"name"`
	FieldPath string `json:"fieldPath"`
}

// TestHelmTemplateRendersRunRoleBindings pins where each rbac.scope grants
// nvcre-run-role (ADR-092): cluster-wide, once per listed namespace, or
// nowhere. It also records the downward-API env the controller reads its own
// ServiceAccount from, since that is the subject a waiting Workflow names.
func TestHelmTemplateRendersRunRoleBindings(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	p := testutil.TestCaseParser{Subdir: "render-rbac", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Set []string `json:"set"`
		}
		if err := syaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		rendered, err := helmTemplate(dir, input.Set)
		if err != nil {
			return err
		}

		bindings := []runRoleBindingView{}
		var env []managerEnvView
		dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
		for {
			var obj unstructured.Unstructured
			if err := dec.Decode(&obj); err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("decode helm template output: %w", err)
			}
			switch obj.GetKind() {
			case "ClusterRoleBinding":
				var b rbacv1.ClusterRoleBinding
				if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &b); err != nil {
					return err
				}
				if b.RoleRef.Name == runRoleName {
					bindings = append(bindings, runRoleBindingView{
						Kind: "ClusterRoleBinding", Name: b.Name, RoleRef: b.RoleRef, Subjects: b.Subjects,
					})
				}
			case "RoleBinding":
				var b rbacv1.RoleBinding
				if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &b); err != nil {
					return err
				}
				if b.RoleRef.Name == runRoleName {
					bindings = append(bindings, runRoleBindingView{
						Kind: "RoleBinding", Name: b.Name, Namespace: b.Namespace, RoleRef: b.RoleRef, Subjects: b.Subjects,
					})
				}
			case deploymentKind:
				var dep appsv1.Deployment
				if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &dep); err != nil {
					return err
				}
				env = downwardEnv(dep.Spec.Template.Spec.Containers)
			}
		}

		data, err := json.MarshalIndent(map[string]any{
			"runRoleBindings": bindings,
			"managerEnv":      env,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func downwardEnv(containers []corev1.Container) []managerEnvView {
	env := []managerEnvView{}
	for _, c := range containers {
		if c.Name != managerContainer {
			continue
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
				env = append(env, managerEnvView{Name: e.Name, FieldPath: e.ValueFrom.FieldRef.FieldPath})
			}
		}
	}
	return env
}

// TestHelmRoleSplitDiff diffs the controller's cluster-mode grant, the union
// of nvcre-manager-role and nvcre-run-role, against the rule set shipped before
// ADR-092 split the role. The golden lists exactly the differences ADR-092 and
// later ADRs name (ADR-103 adds get and list on NetworkAttachmentDefinitions),
// so rbac.scope: cluster, this release's default, cannot gain or lose a
// permission nobody decided on. It also lists each role's own grants, so a
// rule that moves between the two roles shows up as a golden change even when
// the union is unchanged.
func TestHelmRoleSplitDiff(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	rendered, err := helmTemplate(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := clusterRoleRules(rendered, managerRoleName)
	if err != nil {
		t.Fatal(err)
	}
	run, err := clusterRoleRules(rendered, runRoleName)
	if err != nil {
		t.Fatal(err)
	}

	p := testutil.TestCaseParser{Subdir: "role-split", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var previous rbacv1.ClusterRole
		if err := syaml.UnmarshalStrict([]byte(tc.Inputs["input_previous_role.yaml"]), &previous); err != nil {
			return err
		}
		if len(previous.Rules) == 0 {
			return fmt.Errorf("input_previous_role.yaml declares no rules")
		}

		before := expandRules(previous.Rules)
		after := expandRules(append(slices.Clone(manager), run...))
		data, err := json.MarshalIndent(map[string][]string{
			"added":       sortedDifference(after, before),
			"removed":     sortedDifference(before, after),
			"managerRole": slices.Sorted(maps.Keys(expandRules(manager))),
			"runRole":     slices.Sorted(maps.Keys(expandRules(run))),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// expandRules flattens rules into one "verb resource[.group]" entry per
// granted triple. Wildcards are kept literally; neither role uses them.
func expandRules(rules []rbacv1.PolicyRule) map[string]struct{} {
	out := map[string]struct{}{}
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					out[verb+" "+qualifiedResource(group, resource)] = struct{}{}
				}
			}
		}
	}
	return out
}

// sortedDifference returns the entries of a that b lacks, sorted by resource
// and then verb so a resource's changes read together.
func sortedDifference(a, b map[string]struct{}) []string {
	diff := []string{}
	for k := range a {
		if _, ok := b[k]; !ok {
			diff = append(diff, k)
		}
	}
	slices.SortFunc(diff, func(x, y string) int {
		xv, xr, _ := strings.Cut(x, " ")
		yv, yr, _ := strings.Cut(y, " ")
		if c := strings.Compare(xr, yr); c != 0 {
			return c
		}
		return strings.Compare(xv, yv)
	})
	return diff
}
