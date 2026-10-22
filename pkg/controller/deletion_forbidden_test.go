// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Namespace states for the deletion fixtures.
const (
	nsTerminating = "terminating"
	nsActive      = "active"
	nsGone        = "gone"
	nsUnreadable  = "unreadable"
)

// TestDeletionForbiddenInNamespace covers ADR-092 decision 5: a child read or
// delete that is forbidden must not wedge a finalizer when the namespace is
// terminating, because the RoleBinding that granted it may have gone with the
// namespace. When the namespace is not terminating, or cannot be read, the
// finalizer stays and the reconcile retries rather than guess.
func TestDeletionForbiddenInNamespace(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "deletion-forbidden-namespace", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Owner is the finalizer under test: workflow-finalizer or
			// job-finalizer.
			Owner string `json:"owner"`
			// Forbid is the child call that is forbidden: get or delete.
			Forbid string `json:"forbid"`
			// Namespace is terminating, active, gone or unreadable.
			Namespace string `json:"namespace"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		ctx := context.Background()
		scheme := newJobLookupScheme(t)
		if err := trainerv1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "children"}, "child",
			errors.New("injected: no RoleBinding in this namespace"))
		isChild := func(obj client.Object) bool {
			switch obj.(type) {
			case *unstructured.Unstructured, *trainerv1alpha1.TrainJob:
				return true
			}
			return false
		}
		childFuncs := interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if input.Forbid == "get" && isChild(obj) {
					return forbidden
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if input.Forbid == "delete" && isChild(obj) {
					return forbidden
				}
				return c.Delete(ctx, obj, opts...)
			},
		}

		var nsObjects []client.Object
		now := metav1.Now()
		switch input.Namespace {
		case nsTerminating:
			nsObjects = append(nsObjects, &corev1.Namespace{
				Name: testNS, DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
			})
		case nsActive, nsUnreadable:
			nsObjects = append(nsObjects, &corev1.Namespace{Name: testNS})
		case nsGone:
		default:
			return fmt.Errorf("unknown namespace state %q", input.Namespace)
		}
		apiFuncs := interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.Namespace); ok && input.Namespace == nsUnreadable {
					return errors.New("injected: namespace read failed")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}
		api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nsObjects...).
			WithInterceptorFuncs(apiFuncs).Build()

		var (
			requeueAfter  string
			reconcileErr  error
			finalizerHeld bool
		)
		switch input.Owner {
		case "workflow-finalizer":
			workflow := &nvcrev1alpha1.Workflow{
				Name: testWorkflowName, Namespace: testNS, UID: "wf-uid-teardown",
				Finalizers: []string{workflowFinalizer}, DeletionTimestamp: &now,
				Status: nvcrev1alpha1.WorkflowStatus{
					DependencyRefs: []nvcrev1alpha1.DependencyResourceRef{{
						APIVersion: "v1",
						Kind:       kindConfigMap,
						Name:       "hostfile",
						Namespace:  testNS,
					}},
				},
			}
			dep := &unstructured.Unstructured{}
			dep.SetAPIVersion("v1")
			dep.SetKind(kindConfigMap)
			dep.SetName("hostfile")
			dep.SetNamespace(testNS)
			dep.SetAnnotations(map[string]string{annotationWorkflowUID: string(workflow.UID)})
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workflow, dep).
				WithInterceptorFuncs(childFuncs).Build()
			r := &WorkflowReconciler{Client: c, APIReader: api, Scheme: scheme}
			res, err := r.handleDeletion(ctx, workflow)
			requeueAfter, reconcileErr = res.RequeueAfter.String(), err
			finalizerHeld = stillHeld(ctx, c, &nvcrev1alpha1.Workflow{}, testWorkflowName, workflowFinalizer)
		case "job-finalizer":
			job := newLookupJob(true)
			job.Finalizers = []string{jobFinalizer}
			job.DeletionTimestamp = &now
			trainJob := &trainerv1alpha1.TrainJob{Name: testJobWorkloadName, Namespace: testNS}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, trainJob).
				WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(client.Object) []string { return nil }).
				WithInterceptorFuncs(childFuncs).Build()
			r := &JobReconciler{Client: c, APIReader: api, Scheme: scheme}
			res, err := r.handleDeletion(ctx, job)
			requeueAfter, reconcileErr = res.RequeueAfter.String(), err
			finalizerHeld = stillHeld(ctx, c, &nvcrev1alpha1.Job{}, testJobName, jobFinalizer)
		default:
			return fmt.Errorf("unknown owner %q", input.Owner)
		}

		output := struct {
			RequeueAfter  string `json:"requeueAfter"`
			Error         string `json:"error,omitempty"`
			FinalizerHeld bool   `json:"finalizerStillHeld"`
		}{RequeueAfter: requeueAfter, FinalizerHeld: finalizerHeld}
		if reconcileErr != nil {
			output.Error = reconcileErr.Error()
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// stillHeld reports whether the named object still carries finalizer. The
// fake client deletes an object whose last finalizer is removed while it has
// a deletionTimestamp, so a missing object counts as released.
func stillHeld(ctx context.Context, c client.Client, obj client.Object, name, finalizer string) bool {
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: name}, obj); err != nil {
		return false
	}
	return slices.Contains(obj.GetFinalizers(), finalizer)
}
