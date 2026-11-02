// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// nadReader serves a fixed NetworkAttachmentDefinition list, or fails every
// NAD List with listErr. Unless allowList is set it fails the test when a
// NAD List is made, so a case that must not look cannot pass by accident.
// Every other read goes to fallback, the reconciler's own client.
type nadReader struct {
	t         testing.TB
	nads      []unstructured.Unstructured
	listErr   error
	allowList bool
	lists     int
	fallback  client.Reader
	// pageSize, when set, serves the NADs that many at a time through
	// Limit and Continue, as the API server does.
	pageSize int
}

func (r *nadReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var lo client.ListOptions
	lo.ApplyOptions(opts)
	if l, ok := list.(*unstructured.UnstructuredList); !ok || l.GroupVersionKind().Kind != nadGVK.Kind+"List" {
		if r.fallback != nil {
			return r.fallback.List(ctx, list, opts...)
		}
	}
	r.lists++
	if !r.allowList {
		r.t.Errorf("NetworkAttachmentDefinitions were listed, but this case must not look")
	}
	if r.listErr != nil {
		return r.listErr
	}
	l, ok := list.(*unstructured.UnstructuredList)
	if !ok || l.GroupVersionKind().Kind != nadGVK.Kind+"List" {
		return fmt.Errorf("unexpected list type %T %v", list, list.GetObjectKind().GroupVersionKind())
	}
	if r.pageSize == 0 {
		l.Items = append(l.Items, r.nads...)
		return nil
	}
	start := 0
	if lo.Continue != "" {
		if _, err := fmt.Sscanf(lo.Continue, "%d", &start); err != nil {
			return err
		}
	}
	end := min(start+r.pageSize, len(r.nads))
	l.Items = append(l.Items, r.nads[start:end]...)
	if end < len(r.nads) {
		l.SetContinue(fmt.Sprint(end))
	}
	return nil
}

func (r *nadReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if r.fallback != nil {
		return r.fallback.Get(ctx, key, obj, opts...)
	}
	r.t.Errorf("unexpected Get")
	return nil
}

// nadInput is one NAD in a test case's input.yaml.
type nadInput struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
	// Config is spec.config verbatim; omitted means no spec.config at all.
	Config *string `json:"config"`
}

func (n nadInput) object() unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(nadGVK)
	u.SetNamespace(n.Namespace)
	u.SetName(n.Name)
	u.SetAnnotations(n.Annotations)
	if n.Config != nil {
		_ = unstructured.SetNestedField(u.Object, *n.Config, "spec", "config")
	}
	return u
}

func nadObjects(in []nadInput) []unstructured.Unstructured {
	out := make([]unstructured.Unstructured, 0, len(in))
	for _, n := range in {
		out = append(out, n.object())
	}
	return out
}

// listErrForbidden is the listError value for a refused NAD List.
const listErrForbidden = "forbidden"

// nadListError builds the API error a case names. Forbidden, NoMatch and
// NotFound are terminal; the rest are transient.
func nadListError(kind string) (error, error) {
	gr := schema.GroupResource{Group: nadGVK.Group, Resource: "network-attachment-definitions"}
	switch kind {
	case "":
		return nil, nil
	case listErrForbidden:
		return apierrors.NewForbidden(gr, "", errors.New(
			`User "system:serviceaccount:nvcre-system:nvcre-controller-manager" cannot list resource`)), nil
	case "noMatch":
		return &meta.NoKindMatchError{GroupKind: nadGVK.GroupKind(), SearchedVersions: []string{nadGVK.Version}}, nil
	case "notFound":
		// What a client that resolved the API gets once the CRD is removed.
		return apierrors.NewGenericServerResponse(http.StatusNotFound, "get", gr, "", "", 0, false), nil
	case "internal":
		return apierrors.NewInternalError(errors.New("etcdserver: leader changed")), nil
	case "timeout":
		return apierrors.NewTimeoutError("the server was unable to return a response in the time allotted", 1), nil
	}
	return nil, fmt.Errorf("unknown listError %q", kind)
}

// ADR-103 Decision 2 and 3: which NetworkAttachmentDefinition the OCI RDMA
// fragment attaches. The candidates are the NADs annotated
// k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics in the workload
// namespace if there are any, otherwise in every namespace. One is used;
// several whose spec.config is the same apart from its name resolve to the
// first in sorted order, naming the ones skipped; several that differ, none,
// a refused list and a missing API are terminal; any other list error is
// transient.
func TestResolveOCISecondaryNetwork(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-oci-secondary-network",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Namespace string     `json:"namespace"`
			ListError string     `json:"listError"`
			PageSize  int        `json:"pageSize"`
			NADs      []nadInput `json:"nads"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		listErr, err := nadListError(input.ListError)
		if err != nil {
			return err
		}
		reader := &nadReader{
			t: tc.T, nads: nadObjects(input.NADs), listErr: listErr, allowList: true, pageSize: input.PageSize,
		}

		sel, err := resolveOCISecondaryNetwork(context.Background(), reader, input.Namespace)
		out := struct {
			Name      string   `json:"name,omitempty"`
			Skipped   []string `json:"skipped,omitempty"`
			Notice    string   `json:"notice,omitempty"`
			Error     string   `json:"error,omitempty"`
			Terminal  bool     `json:"terminal,omitempty"`
			Forbidden bool     `json:"forbidden,omitempty"`
		}{Name: sel.Name, Skipped: sel.Skipped}
		if len(sel.Skipped) > 0 {
			out.Notice = ociNetworkSkippedMessage(sel)
		}
		if err != nil {
			out.Error = err.Error()
			if snErr, ok := errors.AsType[*secondaryNetworkError](err); ok {
				out.Terminal = true
				out.Forbidden = snErr.Forbidden
			}
		}
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(raw) + "\n"
		return nil
	})
}
