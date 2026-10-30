// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

var nadListGVK = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinitionList"}

// renderNADReader stands in for the dry-run client: it serves a fixed NAD list,
// or fails every NAD List with listErr, and counts the Lists made.
type renderNADReader struct {
	nads    []unstructured.Unstructured
	listErr error
	lists   int
}

func (r *renderNADReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*unstructured.UnstructuredList)
	if !ok || l.GroupVersionKind() != nadListGVK {
		return fmt.Errorf("unexpected list %T", list)
	}
	r.lists++
	if r.listErr != nil {
		return r.listErr
	}
	l.Items = append(l.Items, r.nads...)
	return nil
}

func (r *renderNADReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unexpected Get")
}

// ADR-103 Decision 4 through the CLI. Offline render attaches the default
// network-operator/sriov-net and prints its note only when a Workflow carries
// it. --dry-run renders with the sentinel, detects only when a resolved
// Workflow still carries it, and renders again with the detected name; a
// refused list warns and falls back to the default, since dry-run lists with
// the user's kubeconfig, and every other failure is fatal. The dry-run cases
// go through renderDryRun with the nodes they list, or with the synthetic
// render node when they list none.
func TestCertificationRenderOCINetwork(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-oci-network",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform  string `json:"platform"`
			GPUArch   string `json:"gpuArch"`
			DryRun    bool   `json:"dryRun"`
			Namespace string `json:"namespace"`
			// Nodes are the discovered target nodes a dry-run case resolves
			// against, which also decide its platform when Platform is unset.
			Nodes []struct {
				Name       string `json:"name"`
				Product    string `json:"product"`
				ProviderID string `json:"providerID"`
			} `json:"nodes"`
			// ListForbidden refuses the NAD List, as RBAC would.
			ListForbidden bool `json:"listForbidden"`
			NADs          []struct {
				Namespace   string            `json:"namespace"`
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Config      string            `json:"config"`
			} `json:"nads"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}
		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}
		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		gpuArch, err := catalog.ParseGPUArchFlag(cfg.GPUArch)
		if err != nil {
			return err
		}

		reader := &renderNADReader{}
		for _, n := range cfg.NADs {
			u := unstructured.Unstructured{Object: map[string]any{}}
			u.SetAPIVersion("k8s.cni.cncf.io/v1")
			u.SetKind("NetworkAttachmentDefinition")
			u.SetNamespace(n.Namespace)
			u.SetName(n.Name)
			u.SetAnnotations(n.Annotations)
			_ = unstructured.SetNestedField(u.Object, n.Config, "spec", "config")
			reader.nads = append(reader.nads, u)
		}
		if cfg.ListForbidden {
			reader.listErr = apierrors.NewForbidden(
				schema.GroupResource{Group: "k8s.cni.cncf.io", Resource: "network-attachment-definitions"}, "",
				errors.New(`User "alice" cannot list resource`))
		}

		var stderr bytes.Buffer
		var workflows []nvcrev1alpha1.Workflow
		var renderErr error
		if cfg.DryRun {
			nodes := []corev1.Node{syntheticRenderNode(cfg.Platform, cert.Spec.Target.NodeSelector, gpuArch)}
			if len(cfg.Nodes) > 0 {
				nodes = nil
				for _, n := range cfg.Nodes {
					nodes = append(nodes, corev1.Node{
						Name:   n.Name,
						Labels: map[string]string{gpu.ProductLabel: n.Product},
						Spec:   corev1.NodeSpec{ProviderID: n.ProviderID},
					})
				}
			}
			workflows, renderErr = renderDryRun(context.Background(), cert, nodes, cfg.Platform, gpuArch, nil, "",
				reader, cfg.Namespace, &stderr)
		} else {
			workflows, renderErr = renderOffline(cert, cfg.Platform, gpuArch, &stderr)
		}

		out := struct {
			Workflows []netAttachWorkflow `json:"workflows"`
			Stderr    string              `json:"stderr,omitempty"`
			NADLists  int                 `json:"nadLists"`
			Error     string              `json:"error,omitempty"`
		}{Workflows: []netAttachWorkflow{}, Stderr: stderr.String(), NADLists: reader.lists}
		if renderErr != nil {
			out.Error = renderErr.Error()
		} else {
			for i := range workflows {
				projected, projectErr := projectNetworkAttachments(&workflows[i])
				if projectErr != nil {
					return projectErr
				}
				out.Workflows = append(out.Workflows, projected)
			}
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
