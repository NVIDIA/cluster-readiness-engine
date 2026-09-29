// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/reportexport"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const reportCleanupCertificationName = "cert"

type reportCleanupInput struct {
	SnapshotReady    bool   `json:"snapshotReady"`
	Phase            string `json:"phase"`
	Cancel           bool   `json:"cancel"`
	SkipRegistration bool   `json:"skipRegistration"`
	NoMatchingPolicy bool   `json:"noMatchingPolicy"`
	DeleteExport     bool   `json:"deleteExport"`
	ForeignWorkflow  bool   `json:"foreignWorkflow"`
}

// Exercise the real registration helper and Certification deletion handler
// together. An HTTP failure must not hold source resources once the report is
// durable, while an unsaved report must protect the Workflow from deletion.
func TestCertificationReportCleanup(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "certification-report-cleanup", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input reportCleanupInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		cert, workflow, policy := reportCleanupObjects(input)
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cert, workflow, policy).
			WithStatusSubresource(&nvcrev1alpha1.ReportExport{}).
			Build()
		ctx := context.Background()
		if !input.SkipRegistration {
			if err := reportexport.EnsureRegistered(ctx, c, cert, policy.Namespace, "test-cluster"); err != nil {
				return err
			}
			if err := setReportCleanupState(ctx, c, policy.Namespace, input); err != nil {
				return err
			}
		}
		if err := c.Delete(ctx, cert); err != nil {
			return err
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(cert), cert); err != nil {
			return err
		}
		reconciler := &CertificationReconciler{
			Client: c, ReportClient: c, Scheme: scheme,
			ReportNamespace: policy.Namespace, ReportClusterID: "test-cluster",
		}
		result, err := reconciler.handleDeletion(ctx, cert)
		if err != nil {
			return err
		}
		return collectReportCleanupResult(ctx, c, tc, cert, workflow, policy.Namespace, result.RequeueAfter > 0)
	})
}

func reportCleanupObjects(input reportCleanupInput) (
	*nvcrev1alpha1.Certification, *nvcrev1alpha1.Workflow, *nvcrev1alpha1.ReportExportPolicy,
) {
	created := metav1.NewTime(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
	cert := &nvcrev1alpha1.Certification{
		Name: reportCleanupCertificationName, Namespace: string(burnIn), UID: "cert-uid", CreationTimestamp: created,
		Finalizers: []string{certificationFinalizer},
		Status: nvcrev1alpha1.CertificationStatus{
			CategoryStatuses: []nvcrev1alpha1.CertificationCategoryStatus{{
				Domain: "communication", Variant: "nccl-all-reduce", Status: "Succeeded",
				WorkflowRef: &nvcrev1alpha1.WorkflowReference{Name: labelWorkflow, Namespace: string(burnIn)},
			}},
		},
	}
	workflow := &nvcrev1alpha1.Workflow{
		Name: labelWorkflow, Namespace: cert.Namespace, UID: "workflow-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: "Certification",
			Name: cert.Name, UID: cert.UID, Controller: new(true),
		}}}
	if input.ForeignWorkflow {
		workflow.OwnerReferences[0].UID = "different-certification"
	}
	policy := &nvcrev1alpha1.ReportExportPolicy{
		Name: "results", Namespace: "nvcre-reports", UID: "policy-uid",
		CreationTimestamp: metav1.NewTime(created.Add(-time.Hour)),
		Spec: nvcrev1alpha1.ReportExportPolicySpec{
			Source:  nvcrev1alpha1.ReportExportSourceSelector{Namespaces: []string{cert.Namespace}},
			Webhook: nvcrev1alpha1.ReportWebhook{URL: "https://receiver.example.com/reports"},
		},
	}
	if input.NoMatchingPolicy {
		policy.Spec.Source.Namespaces = []string{"other-namespace"}
	}
	return cert, workflow, policy
}

func setReportCleanupState(ctx context.Context, c client.Client, namespace string, input reportCleanupInput) error {
	var exports nvcrev1alpha1.ReportExportList
	if err := c.List(ctx, &exports, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range exports.Items {
		export := &exports.Items[i]
		if input.DeleteExport {
			if err := c.Delete(ctx, export); err != nil {
				return err
			}
			continue
		}
		export.Spec.Cancel = input.Cancel
		if err := c.Update(ctx, export); err != nil {
			return err
		}
		export.Status.Phase = input.Phase
		status := metav1.ConditionFalse
		if input.SnapshotReady {
			status = metav1.ConditionTrue
		}
		export.Status.Conditions = []metav1.Condition{{
			Type: reportexport.SnapshotReady, Status: status, Reason: "TestSnapshotState",
			LastTransitionTime: metav1.NewTime(time.Date(2026, 9, 30, 10, 1, 0, 0, time.UTC)),
		}}
		if err := c.Status().Update(ctx, export); err != nil {
			return err
		}
	}
	return nil
}

func collectReportCleanupResult(ctx context.Context, c client.Client, tc *testutil.TestCase,
	cert *nvcrev1alpha1.Certification, workflow *nvcrev1alpha1.Workflow, namespace string, requeued bool,
) error {
	certErr := c.Get(ctx, client.ObjectKeyFromObject(cert), cert)
	if certErr != nil && !apierrors.IsNotFound(certErr) {
		return certErr
	}
	workflowErr := c.Get(ctx, client.ObjectKeyFromObject(workflow), workflow)
	if workflowErr != nil && !apierrors.IsNotFound(workflowErr) {
		return workflowErr
	}
	var exports nvcrev1alpha1.ReportExportList
	if err := c.List(ctx, &exports, client.InNamespace(namespace)); err != nil {
		return err
	}
	out := struct {
		CertificationExists bool `json:"certificationExists"`
		WorkflowExists      bool `json:"workflowExists"`
		Requeued            bool `json:"requeued"`
		Exports             int  `json:"exports"`
	}{certErr == nil, workflowErr == nil, requeued, len(exports.Items)}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tc.Actual = string(data) + "\n"
	return nil
}
