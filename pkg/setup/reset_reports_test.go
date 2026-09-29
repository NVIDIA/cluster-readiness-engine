// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Structured reset fixtures intentionally repeat Kubernetes field and kind literals.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	syaml "sigs.k8s.io/yaml"
)

type reportResetInput struct {
	HoldCertification bool   `json:"holdCertification"`
	SkipCR            bool   `json:"skipCR"`
	UnservedSource    bool   `json:"unservedSource"`
	Fault             string `json:"fault"`
}

type reportResetResult struct {
	Error        string          `json:"error,omitempty"`
	Deletes      []string        `json:"deletes"`
	HelmCommands []string        `json:"helmCommands"`
	Remaining    map[string]bool `json:"remaining"`
	Output       string          `json:"output"`
}

func TestResetPreservesReportDelivery(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "reset-report-delivery", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input reportResetInput
		if err := syaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		result, err := runReportResetCase(tc.T.(*testing.T), input)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func runReportResetCase(t *testing.T, input reportResetInput) (reportResetResult, error) {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "helm-calls")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helm"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$RESET_HELM_CALLS\"\n"), 0o755))
	t.Setenv("PATH", dir)
	t.Setenv("RESET_HELM_CALLS", calls)

	scheme := newSetupScheme(t)
	objects := reportResetObjects(input)
	for _, obj := range objects {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Group == nvcreAPIGroup {
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
		}
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := reportResetResult{Deletes: []string{}, HelmCommands: []string{}, Remaining: map[string]bool{}}
	c := interceptor.NewClient(base, reportResetInterceptors(input, cancel, &result.Deletes))
	var out bytes.Buffer
	err := runResetPhases(setupPhaseParams{
		ctx: ctx, c: c, out: &out,
		kubeconfig: "/tmp/reset-test.kubeconfig", kubeContext: "reset-context",
		skip: map[string]bool{phaseCR: input.SkipCR, phaseDeps: true},
	})
	if err != nil {
		result.Error = err.Error()
	}
	result.Output = out.String()
	if commands, err := os.ReadFile(calls); err == nil {
		result.HelmCommands = strings.Split(strings.TrimSpace(string(commands)), "\n")
	} else if !os.IsNotExist(err) {
		return result, err
	}
	for _, obj := range objects {
		copy := obj.DeepCopyObject().(client.Object)
		err := base.Get(context.Background(), client.ObjectKeyFromObject(obj), copy)
		if err != nil && !apierrors.IsNotFound(err) {
			return result, err
		}
		result.Remaining[resetObjectID(obj)] = err == nil
	}
	return result, nil
}

func reportResetObjects(input reportResetInput) []client.Object {
	var objects []client.Object
	for _, res := range []struct{ plural, kind string }{
		{"certifications", "Certification"}, {"workflows", "Workflow"},
		{"jobs", "Job"}, {"bandwidthmeasurements", "BandwidthMeasurement"},
		{"reportexportpolicies", "ReportExportPolicy"}, {"reportexports", "ReportExport"},
	} {
		objects = append(objects, &apiextv1.CustomResourceDefinition{
			APIVersion: apiextv1.SchemeGroupVersion.String(), Kind: kindCustomResourceDefinition,
			Name: res.plural + "." + nvcreAPIGroup,
			Spec: apiextv1.CustomResourceDefinitionSpec{
				Group:    nvcreAPIGroup,
				Names:    apiextv1.CustomResourceDefinitionNames{Plural: res.plural, Kind: res.kind},
				Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: !input.UnservedSource || res.kind != "Certification", Storage: true}},
			},
		})
		namespace := "gpu-validation"
		if isReportExportCRD(res.plural + "." + nvcreAPIGroup) {
			namespace = "nvcre-reports"
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": nvcreAPIGroup + "/v1alpha1", "kind": res.kind,
			"metadata": map[string]any{"name": "test-" + res.plural, "namespace": namespace},
		}}
		if input.HoldCertification && res.kind == "Certification" {
			obj.SetFinalizers([]string{"nvcre.nvidia.com/finalizer"})
		}
		objects = append(objects, obj)
	}
	return append(objects,
		&corev1.ConfigMap{APIVersion: "v1", Kind: kindConfigMap, Name: "snapshot", Namespace: "nvcre-reports"},
		&corev1.Secret{APIVersion: "v1", Kind: "Secret", Name: "webhook-auth", Namespace: "nvcre-reports"},
	)
}

func reportResetInterceptors(input reportResetInput, cancel context.CancelFunc, deletes *[]string) interceptor.Funcs {
	certDeleted, crdDeleted := false, false
	denied := func(resource, name string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: nvcreAPIGroup, Resource: resource}, name, errors.New("denied"))
	}
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if list.GetObjectKind().GroupVersionKind().Kind == "CertificationList" {
				if input.Fault == "cert-list" || input.Fault == "wait-list" && certDeleted {
					return denied("certifications", "")
				}
				if input.HoldCertification && certDeleted {
					cancel() // Deterministically end the wait with the finalizer still present.
				}
			}
			return c.List(ctx, list, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*deletes = append(*deletes, resetObjectID(obj))
			switch obj.GetObjectKind().GroupVersionKind().Kind {
			case "Certification":
				if input.Fault == "cert-delete" {
					return denied("certifications", obj.GetName())
				}
				certDeleted = true
			case kindCustomResourceDefinition:
				crdDeleted = true
				if input.Fault == "crd-stuck" {
					cancel()
					return nil // API accepted deletion but finalization has not completed.
				}
			}
			return c.Delete(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if input.Fault == "crd-wait-get" && crdDeleted && obj.GetObjectKind().GroupVersionKind().Kind == kindCustomResourceDefinition {
				return denied("customresourcedefinitions", key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
}

func resetObjectID(obj client.Object) string {
	return fmt.Sprintf("%s/%s", obj.GetObjectKind().GroupVersionKind().Kind, client.ObjectKeyFromObject(obj))
}
