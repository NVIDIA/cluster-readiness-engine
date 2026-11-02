// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestCertificationWorkflowRerun pins what the Certification tier does when it
// runs Workflow creation again for a category whose Workflow already exists
// (issue #566). That happens when the status write after a successful Create
// fails, when the next reconcile reads a cached Certification that predates
// that write, or when the controller restarts in between.
//
// Every terminal check that runs before the Create (node discovery, GPU
// architecture, capacity, OCI RDMA network detection) can fail on that second
// pass if the cluster changed in between, and before this fix it marked the
// Certification Failed while its Workflow kept running. A Workflow this
// Certification controls is adopted before any of those checks run. A holder
// it does not control, or no Workflow at all, takes the path it always did.
func TestCertificationWorkflowRerun(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-workflow-rerun",
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
			// ExistingWorkflow is the index of the category whose Workflow is
			// already on the API server, or -1 for none.
			ExistingWorkflow int `json:"existingWorkflow"`
			// ForeignHolder makes that Workflow belong to another
			// Certification.
			ForeignHolder bool `json:"foreignHolder"`
			// ExpectLookup says whether this case may list NADs at all.
			ExpectLookup bool       `json:"expectLookup"`
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
		// Created well past nodeDiscoveryTimeout, so a node set that has gone
		// away is terminal rather than something to wait for.
		cert := &nvcrev1alpha1.Certification{
			Name: testCertName, Namespace: testNS, UID: testCertUID,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * nodeDiscoveryTimeout)),
			Finalizers:        []string{certificationFinalizer},
			Spec:              input.Spec,
		}
		if input.Status != nil {
			cert.Status = *input.Status
		}
		objs = append(objs, cert)

		if i := input.ExistingWorkflow; i >= 0 {
			owner := metav1.OwnerReference{
				APIVersion: nvcrev1alpha1.GroupVersion.String(),
				Kind:       testKindCertification,
				Name:       testCertName,
				UID:        testCertUID,
				Controller: new(true),
			}
			if input.ForeignHolder {
				owner.UID = "a-different-certification-uid"
			}
			objs = append(objs, &nvcrev1alpha1.Workflow{
				Name:            WorkflowName(testCertName, input.Spec.Categories[i]),
				Namespace:       testNS,
				OwnerReferences: []metav1.OwnerReference{owner},
			})
		}

		c := fake.NewClientBuilder().
			WithScheme(newFallbackScheme(tc.T.(*testing.T))).
			WithObjects(objs...).
			WithStatusSubresource(&nvcrev1alpha1.Certification{}, &nvcrev1alpha1.Workflow{}).
			Build()
		reader := &nadReader{
			t: tc.T, nads: nadObjects(input.NADs),
			allowList: input.ExpectLookup, fallback: c,
		}
		recorder := events.NewFakeRecorder(20)
		r := &CertificationReconciler{Client: c, APIReader: reader, Scheme: c.Scheme(), Recorder: recorder}

		result, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cert)})

		type category struct {
			Status      string `json:"status"`
			WorkflowRef string `json:"workflowRef"`
		}
		out := struct {
			Requeue    bool                `json:"requeue"`
			Error      string              `json:"error,omitempty"`
			Conditions []map[string]string `json:"conditions,omitempty"`
			Categories []category          `json:"categories"`
			Workflows  []string            `json:"workflows"`
		}{Requeue: result.RequeueAfter > 0, Categories: []category{}, Workflows: []string{}}
		if reconcileErr != nil {
			out.Error = reconcileErr.Error()
		}

		current := &nvcrev1alpha1.Certification{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cert), current); err != nil {
			return err
		}
		for _, cond := range current.Status.Conditions {
			if cond.Status != metav1.ConditionTrue {
				continue
			}
			out.Conditions = append(out.Conditions, map[string]string{
				"type": cond.Type, "reason": cond.Reason, "message": cond.Message,
			})
		}
		slices.SortFunc(out.Conditions, func(a, b map[string]string) int { return strings.Compare(a["type"], b["type"]) })
		for _, cs := range current.Status.CategoryStatuses {
			row := category{Status: cs.Status, WorkflowRef: testNilRef}
			if cs.WorkflowRef != nil {
				row.WorkflowRef = cs.WorkflowRef.Name
			}
			out.Categories = append(out.Categories, row)
		}

		workflows := &nvcrev1alpha1.WorkflowList{}
		if err := c.List(ctx, workflows, client.InNamespace(testNS)); err != nil {
			return err
		}
		for i := range workflows.Items {
			out.Workflows = append(out.Workflows, workflows.Items[i].Name)
		}

		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(raw) + "\n"
		return nil
	})
}
