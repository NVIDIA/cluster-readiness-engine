// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// ADR-103 Decision 1: detection is gated on the render, not its inputs. For
// every catalog entry and every architecture in gpu-defaults.yaml, this builds
// the first pass the way createWorkflowForCategory does (sentinel network,
// overlays applied and pruned) and records whether the sentinel survives. A
// catalog change that starts or stops attaching the OCI RDMA network on some
// entry and architecture shows up here as a golden diff, next to the
// mlnxPerNode each architecture resolves, which on its own is not the gate.
func TestOCINetworkGate(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "oci-network-gate",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Platform string `json:"platform"`
			// Architectures defaults to every architecture in gpu-defaults.yaml,
			// so a new one shows up in the golden without editing the input.
			Architectures []string `json:"architectures"`
			// NodesPerJob are tried in order until an entry builds.
			NodesPerJob        []int32                           `json:"nodesPerJob"`
			NetworkAttachments []nvcrev1alpha1.NetworkAttachment `json:"networkAttachments"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		if len(input.Architectures) == 0 {
			archs, err := gpuDefaultsArchitectures()
			if err != nil {
				return err
			}
			input.Architectures = archs
		}

		type archResult struct {
			Arch        string   `json:"arch"`
			MlnxPerNode int32    `json:"mlnxPerNode"`
			Attaches    []string `json:"attaches"`
			NoAttach    []string `json:"noAttachment"`
			BuildErrors []string `json:"buildErrors,omitempty"`
		}
		var results []archResult
		for _, arch := range input.Architectures {
			nd := catalog.GPUDefaults(arch, input.Platform)
			res := archResult{Arch: arch, MlnxPerNode: nd.MlnxPerNode, Attaches: []string{}, NoAttach: []string{}}
			for _, info := range catalog.List() {
				name := info.Domain + "/" + info.Variant
				entry := catalog.Lookup(info.Domain, info.Variant)
				category := nvcrev1alpha1.CertificateCategory{Domain: info.Domain, Variant: info.Variant}
				var (
					spec    nvcrev1alpha1.WorkflowSpec
					lastErr error
				)
				for _, n := range input.NodesPerJob {
					spec, _, lastErr = buildResolvedWorkflowSpec(entry, category, nvcrev1alpha1.TargetSpec{},
						catalog.BuildConfig{
							NodesPerJob:         n,
							GpusPerNode:         nd.GpusPerNode,
							MlnxPerNode:         nd.MlnxPerNode,
							GPUArchitecture:     arch,
							NetworkAttachments:  input.NetworkAttachments,
							OCISecondaryNetwork: catalog.OCISecondaryNetworkUnresolved,
						}, input.Platform, arch, nil)
					if lastErr == nil {
						break
					}
				}
				if lastErr != nil {
					res.BuildErrors = append(res.BuildErrors, name)
					continue
				}
				attaches, err := catalog.SpecReferencesNetwork(&spec, catalog.OCISecondaryNetworkUnresolved)
				if err != nil {
					return err
				}
				if attaches {
					res.Attaches = append(res.Attaches, name)
				} else {
					res.NoAttach = append(res.NoAttach, name)
				}
			}
			results = append(results, res)
		}
		raw, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(raw) + "\n"
		return nil
	})
}

// gpuDefaultsArchitectures lists the architectures gpu-defaults.yaml
// defines, sorted.
func gpuDefaultsArchitectures() ([]string, error) {
	raw, err := fs.ReadFile(catalog.EntriesFS(), "entries/_lib/gpu-defaults.yaml")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Defaults map[string]any `json:"defaults"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(doc.Defaults)), nil
}

var networksAnnotation = regexp.MustCompile(`"k8s\.v1\.cni\.cncf\.io/networks":"([^"]*)"`)

// ADR-103 Decisions 1 to 3, end to end through one Reconcile: a category
// that attaches the OCI RDMA network gets the detected NAD and nothing else;
// one that does not never lists NADs; a terminal failure fails the
// Certification with SecondaryNetworkDetectionFailed and a Warning event and
// creates no Workflow, for the first category and for a later one; a
// transient list error requeues without touching status.
func TestCertificationOCINetworkDetection(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-oci-network",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name        string            `json:"name"`
				Product     string            `json:"product"`
				ProviderID  string            `json:"providerID"`
				Allocatable map[string]string `json:"allocatable"`
			} `json:"nodes"`
			Spec nvcrev1alpha1.CertificationSpec `json:"spec"`
			// Status, when set, starts the Certification part way through, so
			// the Reconcile creates a later category's Workflow.
			Status *nvcrev1alpha1.CertificationStatus `json:"status"`
			// ExpectLookup says whether this case may list NADs at all.
			ExpectLookup bool       `json:"expectLookup"`
			ListError    string     `json:"listError"`
			NADs         []nadInput `json:"nads"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		ctx := context.Background()

		objs := []client.Object{}
		for _, n := range input.Nodes {
			node := &corev1.Node{
				Name:   n.Name,
				Labels: map[string]string{GPUNodeLabel: "true", testGPUProductLabel: n.Product},
				Spec:   corev1.NodeSpec{ProviderID: n.ProviderID},
				Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
			}
			for res, qty := range n.Allocatable {
				node.Status.Allocatable[corev1.ResourceName(res)] = resource.MustParse(qty)
			}
			objs = append(objs, node)
		}
		// The finalizer is already there, so the one Reconcile below goes
		// straight to creating a Workflow: the first category's, or, when
		// status is set, the first Pending category's.
		cert := &nvcrev1alpha1.Certification{
			Name: "oci-cert", Namespace: testNS, Spec: input.Spec,
			Finalizers: []string{certificationFinalizer},
		}
		if input.Status != nil {
			cert.Status = *input.Status
		}
		objs = append(objs, cert)

		c := fake.NewClientBuilder().
			WithScheme(newFallbackScheme(tc.T.(*testing.T))).
			WithObjects(objs...).
			WithStatusSubresource(&nvcrev1alpha1.Certification{}).
			Build()
		listErr, err := nadListError(input.ListError)
		if err != nil {
			return err
		}
		reader := &nadReader{
			t: tc.T, nads: nadObjects(input.NADs), listErr: listErr,
			allowList: input.ExpectLookup, fallback: c,
		}
		recorder := events.NewFakeRecorder(20)
		r := &CertificationReconciler{Client: c, APIReader: reader, Scheme: c.Scheme(), Recorder: recorder}

		result, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cert)})

		type workflowOut struct {
			Name              string `json:"name"`
			Networks          string `json:"networks,omitempty"`
			NamesDefault      bool   `json:"namesDefault,omitempty"`
			NamesSentinel     bool   `json:"namesSentinel,omitempty"`
			MlnxNICsRequested bool   `json:"mlnxnicsRequested,omitempty"`
		}
		out := struct {
			Requeue    bool                `json:"requeue"`
			Error      string              `json:"error,omitempty"`
			NADLists   int                 `json:"nadLists"`
			Conditions []map[string]string `json:"conditions,omitempty"`
			Events     []string            `json:"events,omitempty"`
			Workflows  []workflowOut       `json:"workflows"`
		}{Requeue: result.RequeueAfter > 0, NADLists: reader.lists, Workflows: []workflowOut{}}
		if reconcileErr != nil {
			out.Error = reconcileErr.Error()
		}

		current := &nvcrev1alpha1.Certification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cert), current); err != nil {
			return err
		}
		for _, cond := range current.Status.Conditions {
			if cond.Status != "True" {
				continue
			}
			out.Conditions = append(out.Conditions, map[string]string{
				"type": cond.Type, "reason": cond.Reason, "message": cond.Message,
			})
		}
		slices.SortFunc(out.Conditions, func(a, b map[string]string) int { return strings.Compare(a["type"], b["type"]) })
		for drained := false; !drained; {
			select {
			case e := <-recorder.Events:
				// Only the events this ADR adds; the reconcile emits others.
				if strings.Contains(e, "SecondaryNetwork") {
					out.Events = append(out.Events, e)
				}
			default:
				drained = true
			}
		}

		workflows := &nvcrev1alpha1.WorkflowList{}
		if err := c.List(ctx, workflows, client.InNamespace(testNS)); err != nil {
			return err
		}
		for i := range workflows.Items {
			raw, err := json.Marshal(workflows.Items[i].Spec)
			if err != nil {
				return err
			}
			w := workflowOut{
				Name:              workflows.Items[i].Name,
				NamesDefault:      strings.Contains(string(raw), catalog.DefaultOCISecondaryNetwork),
				NamesSentinel:     strings.Contains(string(raw), catalog.OCISecondaryNetworkUnresolved),
				MlnxNICsRequested: strings.Contains(string(raw), catalog.OCIMlnxResourceName),
			}
			if m := networksAnnotation.FindStringSubmatch(string(raw)); m != nil {
				w.Networks = m[1]
			}
			out.Workflows = append(out.Workflows, w)
		}

		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(raw) + "\n"
		return nil
	})
}
