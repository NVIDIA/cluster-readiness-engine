// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	sigsyaml "sigs.k8s.io/yaml"
)

// nvcreCRDNames are the seven CRDs the chart carries today. A case's chart
// defaults to them.
var nvcreCRDNames = []string{
	"bandwidthmeasurements.nvcre.nvidia.com",
	"certifications.nvcre.nvidia.com",
	"goodputmeasurements.nvcre.nvidia.com",
	"jobs.nvcre.nvidia.com",
	"logprofiles.nvcre.nvidia.com",
	"workflows.nvcre.nvidia.com",
	"workloadruns.nvcre.nvidia.com",
}

// crdOwnershipCase describes one cluster. owner is "namespace/release" and
// sets the label and both annotations Helm writes; annotations alone model
// incomplete metadata.
type crdOwnershipCase struct {
	CRDsDisabled bool `json:"crdsDisabled"`
	CRDs         []struct {
		Name        string            `json:"name"`
		Group       string            `json:"group"`
		Owner       string            `json:"owner"`
		Annotations map[string]string `json:"annotations"`
	} `json:"crds"`
	// Releases maps "namespace/release" to its helm status; a release not
	// listed reads as not installed.
	Releases map[string]string `json:"releases"`
	// StatusError makes every status read fail.
	StatusError string `json:"statusError"`
	// ChartCRDs are the CRDs of the chart being installed; omitted means
	// nvcreCRDNames.
	ChartCRDs []string `json:"chartCRDs"`
}

func TestPlanCRDReconcile(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "crd-ownership", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in crdOwnershipCase
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		var objects []client.Object
		for _, crd := range in.CRDs {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion("apiextensions.k8s.io/v1")
			obj.SetKind(kindCustomResourceDefinition)
			obj.SetName(crd.Name)
			group := crd.Group
			if group == "" {
				group = nvcreAPIGroup
			}
			if err := unstructured.SetNestedField(obj.Object, group, "spec", "group"); err != nil {
				return err
			}
			annotations := map[string]string{}
			maps.Copy(annotations, crd.Annotations)
			if crd.Owner != "" {
				namespace, release, _ := strings.Cut(crd.Owner, "/")
				obj.SetLabels(map[string]string{helmManagedByLabel: helmManagedByValue})
				annotations["meta.helm.sh/release-name"] = release
				annotations["meta.helm.sh/release-namespace"] = namespace
			}
			if len(annotations) > 0 {
				obj.SetAnnotations(annotations)
			}
			objects = append(objects, obj)
		}
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(objects...).Build()

		var statusCalls []string
		status := func(release, namespace string) (string, error) {
			key := namespace + "/" + release
			statusCalls = append(statusCalls, key)
			if in.StatusError != "" {
				return helmStateUnknown, errors.New(in.StatusError)
			}
			if state, ok := in.Releases[key]; ok {
				return state, nil
			}
			return helmStateNotInstalled, nil
		}

		chartCRDs := in.ChartCRDs
		if chartCRDs == nil {
			chartCRDs = nvcreCRDNames
		}

		// Both stages, as setup init runs them: the plan before the deps
		// phase, then the check against the fetched chart in the helm phase.
		var out bytes.Buffer
		plan, err := planCRDReconcile(context.Background(), c, in.CRDsDisabled, status, &out)
		if err == nil {
			plan = confirmReleaseOwned(plan, chartCRDs, &out)
		}
		result := struct {
			Action      string   `json:"action,omitempty"`
			Owner       string   `json:"owner,omitempty"`
			SkipCRDs    bool     `json:"skipCRDs"`
			StatusCalls []string `json:"statusCalls,omitempty"`
			Output      []string `json:"output,omitempty"`
			Error       string   `json:"error,omitempty"`
		}{StatusCalls: statusCalls}
		if out.Len() > 0 {
			result.Output = strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		}
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Action = map[crdAction]string{
				crdActionApply:        "apply",
				crdActionReleaseOwned: "release-owned",
				crdActionLeave:        "leave",
			}[plan.action]
			if plan.owner.valid() {
				result.Owner = plan.owner.namespace + "/" + plan.owner.release
			}
			result.SkipCRDs = plan.skipCRDs()
		}

		actual, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(actual) + "\n"
		return nil
	})
}
