// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
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
		if err := syaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		// The golden is the residue, so an input that declares nothing produces
		// the same {"missing": []} as an input every verb satisfies. Without
		// these checks the case would pass with no assertions after a typo in
		// the top-level key or a dropped verbs list, which is the drift it
		// exists to catch.
		if len(input.Required) == 0 {
			return fmt.Errorf("input.yaml declares no required access")
		}

		// checked is recorded alongside missing so the golden describes what the
		// case asserted, not only what it failed to find. A golden that carried
		// the residue alone could be regenerated over a narrowed role and still
		// read as an ordinary update, because {"missing": []} is also what an
		// assertion that never ran produces.
		granted := []string{}
		missing := []string{}
		for _, req := range input.Required {
			if len(req.Verbs) == 0 {
				return fmt.Errorf("input.yaml entry %s declares no verbs",
					qualifiedResource(req.Group, req.Resource))
			}
			for _, verb := range req.Verbs {
				access := verb + " " + qualifiedResource(req.Group, req.Resource)
				if rulesAllow(rules, req.Group, req.Resource, verb) {
					granted = append(granted, access)
				} else {
					missing = append(missing, access)
				}
			}
		}

		data, err := json.MarshalIndent(map[string][]string{
			"granted": granted,
			"missing": missing,
		}, "", "  ")
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

// dependencyKindToResource maps each kind a Workflow can create as a dependency
// to the API group and resource its RBAC rule names. A kind absent here is
// reported by TestManagerRoleCoversEveryDependencyKind rather than skipped, so
// adding a dependency kind to the catalog forces a decision about its grant.
var dependencyKindToResource = map[string]requiredAccess{
	"ConfigMap":             {Group: "", Resource: "configmaps"},
	"PersistentVolumeClaim": {Group: "", Resource: "persistentvolumeclaims"},
	"TrainingRuntime":       {Group: "trainer.kubeflow.org", Resource: "trainingruntimes"},
	"ResourceClaimTemplate": {Group: "resource.k8s.io", Resource: "resourceclaimtemplates"},
	"ComputeDomain":         {Group: "resource.nvidia.com", Resource: "computedomains"},
}

// dependencyLifecycleVerbs are the verbs the Workflow controller uses on every
// dependency it owns: create (createDependencies / createJobDependencies), get
// (getDependencyObject, setDependencyOwners), update (setDependencyOwners
// stamps the Job owner on job-scoped dependencies) and delete (handleDeletion
// and scoped-dependency cleanup).
var dependencyLifecycleVerbs = []string{"create", "get", "update", "delete"}

// TestManagerRoleCoversEveryDependencyKind derives the dependency kinds from
// the catalog itself and checks the rendered role grants the lifecycle verbs on
// each. The sibling testdata case pins the mapping that is known today; this
// one fails when the catalog grows a kind nobody granted, which is the drift
// that left PersistentVolumeClaims without update until #463. envtest runs as
// an admin, so neither gap shows up in an integration test.
func TestManagerRoleCoversEveryDependencyKind(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	rendered, err := helmTemplate(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := clusterRoleRules(rendered, managerRoleName)
	if err != nil {
		t.Fatal(err)
	}

	kinds, err := catalogDependencyKinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) == 0 {
		t.Fatal("found no dependency kinds in the catalog; the scan is broken, not the role")
	}

	for _, kind := range kinds {
		access, known := dependencyKindToResource[kind]
		if !known {
			t.Errorf("catalog declares dependency kind %s, which maps to no RBAC resource here; "+
				"add it to dependencyKindToResource and grant it in the chart's manager role", kind)
			continue
		}
		for _, verb := range dependencyLifecycleVerbs {
			if !rulesAllow(rules, access.Group, access.Resource, verb) {
				t.Errorf("manager role does not grant %s on %s, needed for dependency kind %s",
					verb, qualifiedResource(access.Group, access.Resource), kind)
			}
		}
	}
}

// catalogDependencyKinds returns the sorted set of kinds the catalog and the
// platform overrides declare as Workflow dependencies.
//
// The sources are Go templates, so they cannot be parsed as YAML: a fragment
// under _lib/deps/ is a bare list of objects, and an entry's dependencies may
// sit inside a {{- if }} guard. The scan therefore walks lines, tracking
// whether it is inside a dependencies: block, and skips template directives so
// a guard at column zero does not read as the end of the block.
func catalogDependencyKinds() ([]string, error) {
	seen := map[string]struct{}{}
	sources := []struct {
		fsys fs.FS
		root string
	}{
		{catalog.EntriesFS(), "entries"},
		{platform.OverridesFS(), "overrides"},
	}
	for _, src := range sources {
		err := fs.WalkDir(src.fsys, src.root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return walkErr
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			raw, err := fs.ReadFile(src.fsys, path)
			if err != nil {
				return err
			}
			for _, kind := range dependencyKindsIn(string(raw), strings.Contains(path, "_lib/deps/")) {
				seen[kind] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

var (
	dependenciesKeyRE = regexp.MustCompile(`^(\s*)dependencies:\s*$`)
	kindRE            = regexp.MustCompile(`^\s*(?:-\s+)?kind:\s*(\S+)`)
)

// dependencyKindsIn returns the dependency kinds declared in one file.
// bareList is true for _lib/deps/ fragments, whose whole content is a
// dependency list with no dependencies: key above it.
func dependencyKindsIn(content string, bareList bool) []string {
	var kinds []string
	inDeps := bareList
	depIndent := -1
	for line := range strings.Lines(content) {
		line = strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(line)
		// Blank lines, comments and template actions carry no indentation
		// meaning: a {{- if }} guard sits at column zero inside a block.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "{{") {
			continue
		}
		if m := dependenciesKeyRE.FindStringSubmatch(line); m != nil {
			inDeps, depIndent = true, len(m[1])
			continue
		}
		if depIndent >= 0 {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent <= depIndent && !strings.HasPrefix(trimmed, "-") {
				inDeps, depIndent = bareList, -1
			}
		}
		if m := kindRE.FindStringSubmatch(line); m != nil && inDeps {
			kinds = append(kinds, m[1])
		}
	}
	return kinds
}
