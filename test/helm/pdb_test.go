// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"io"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestHelmTemplateControllerPDB(t *testing.T) {
	requireHelm(t)
	const enablePDB = "pdb.enabled=true"
	dir := chartDir(t)
	requireChartInputs(t, dir)
	for _, tc := range []struct {
		name string
		set  []string
		want *intstr.IntOrString
	}{
		{name: "disabled by default"},
		{name: "explicitly disabled", set: []string{"pdb.enabled=false"}},
		{
			name: "singleton protection",
			set:  []string{enablePDB},
			want: new(intstr.FromInt32(1)),
		},
		{
			name: "two replicas",
			set:  []string{enablePDB, "manager.replicas=2"},
			want: new(intstr.FromInt32(1)),
		},
		{
			name: "integer override",
			set:  []string{enablePDB, "pdb.minAvailable=2", "nameOverride=custom"},
			want: new(intstr.FromInt32(2)),
		},
		{
			name: "zero permits maintenance",
			set:  []string{enablePDB, "pdb.minAvailable=0"},
			want: new(intstr.FromInt32(0)),
		},
		{
			name: "percentage",
			set:  []string{enablePDB, "pdb.minAvailable=50%"},
			want: new(intstr.FromString("50%")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered, err := helmTemplate(dir, tc.set)
			if err != nil {
				t.Fatal(err)
			}
			var pdbs []policyv1.PodDisruptionBudget
			var deployment appsv1.Deployment
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
			for {
				var obj unstructured.Unstructured
				if err := decoder.Decode(&obj); err != nil {
					if err == io.EOF {
						break
					}
					t.Fatal(err)
				}
				switch obj.GetKind() {
				case "PodDisruptionBudget":
					var pdb policyv1.PodDisruptionBudget
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &pdb); err != nil {
						t.Fatal(err)
					}
					if obj.GetAPIVersion() != "policy/v1" {
						t.Fatalf("unexpected API version: %s", obj.GetAPIVersion())
					}
					pdbs = append(pdbs, pdb)
				case deploymentKind:
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.want == nil {
				if len(pdbs) != 0 {
					t.Fatal("disabled PDB was rendered")
				}
				return
			}
			if len(pdbs) != 1 {
				t.Fatalf("got %d PDBs, want 1", len(pdbs))
			}
			pdb := pdbs[0]
			if !reflect.DeepEqual(pdb.Spec.MinAvailable, tc.want) {
				t.Fatalf("minAvailable = %v, want %v", pdb.Spec.MinAvailable, tc.want)
			}
			if pdb.Spec.MaxUnavailable != nil {
				t.Fatal("maxUnavailable must be unset")
			}
			if deployment.Name == "" || pdb.Name != deployment.Name || pdb.Namespace != deployment.Namespace {
				t.Fatal("PDB identity does not match manager Deployment")
			}
			if pdb.Spec.Selector == nil || len(pdb.Spec.Selector.MatchLabels) == 0 ||
				!reflect.DeepEqual(pdb.Spec.Selector, deployment.Spec.Selector) {
				t.Fatal("PDB selector does not match manager Deployment")
			}
		})
	}
}

func TestHelmTemplateDefaultControllerAntiAffinity(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	rendered, err := helmTemplate(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	deployment := decodeManagerDeployment(t, rendered)
	affinity := deployment.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.PodAntiAffinity == nil {
		t.Fatal("default pod anti-affinity was not rendered")
	}
	preferred := affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) != 1 {
		t.Fatalf("got %d preferred pod anti-affinity terms, want 1", len(preferred))
	}
	term := preferred[0]
	if term.Weight != 100 {
		t.Fatalf("anti-affinity weight = %d, want 100", term.Weight)
	}
	if term.PodAffinityTerm.TopologyKey != "kubernetes.io/hostname" {
		t.Fatalf("anti-affinity topology key = %q, want kubernetes.io/hostname",
			term.PodAffinityTerm.TopologyKey)
	}
	if !reflect.DeepEqual(term.PodAffinityTerm.LabelSelector, deployment.Spec.Selector) {
		t.Fatal("anti-affinity selector does not match manager Deployment")
	}
}

func TestHelmTemplateControllerAffinityOverrideReplacesDefault(t *testing.T) {
	requireHelm(t)
	dir := chartDir(t)
	requireChartInputs(t, dir)

	const requiredNodeAffinity = "manager.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution."
	rendered, err := helmTemplate(dir, []string{
		requiredNodeAffinity + "nodeSelectorTerms[0].matchExpressions[0].key=node-role.kubernetes.io/control-plane",
		requiredNodeAffinity + "nodeSelectorTerms[0].matchExpressions[0].operator=Exists",
	})
	if err != nil {
		t.Fatal(err)
	}

	deployment := decodeManagerDeployment(t, rendered)
	affinity := deployment.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil {
		t.Fatal("custom node affinity was not rendered")
	}
	if affinity.PodAntiAffinity != nil {
		t.Fatal("default pod anti-affinity was retained with a custom affinity override")
	}
}

func decodeManagerDeployment(t *testing.T, rendered []byte) appsv1.Deployment {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		if obj.GetKind() != deploymentKind {
			continue
		}

		var deployment appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment); err != nil {
			t.Fatal(err)
		}
		return deployment
	}
	t.Fatal("rendered chart has no manager Deployment")
	return appsv1.Deployment{}
}
