// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const testRunUID types.UID = "run-uid"

// TestWorkloadRunCreateAdopt pins what the reconciler does when Create reports
// the Workflow name is already taken (issue #383).
//
// Creating the Workflow and recording status.workflowRef are two separate
// writes. When the second one fails, the next reconcile finds the name taken,
// and what it does there decides whether the run recovers or is stranded with
// an empty status forever. Each case sets who holds the name and how many
// status writes fail, then drives Reconcile repeatedly.
func TestWorkloadRunCreateAdopt(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-create-adopt",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// Holder decides who owns a Workflow already carrying the run's
			// name: "none", "own", "own-terminating", or "foreign".
			Holder string `yaml:"holder"`
			// StatusWriteFailures fails that many leading status writes with a
			// conflict, modelling a write lost to a lagging cache or a
			// controller restart.
			StatusWriteFailures int `yaml:"statusWriteFailures"`
			// CacheHidesWorkflow makes the reconciler's Client answer NotFound
			// for every Workflow Get while the object store still holds it, so
			// only a read through APIReader can see the holder.
			CacheHidesWorkflow bool `yaml:"cacheHidesWorkflow"`
			// Reconciles is how many passes to drive.
			Reconciles int `yaml:"reconciles"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newWorkflowScheme(t)

		run := &nvcrev1alpha1.WorkloadRun{
			Name: testRunName, Namespace: testNS, UID: testRunUID,
			Spec: nvcrev1alpha1.WorkloadRunSpec{
				Image:    "test-image:latest",
				NumNodes: 1,
				Framework: nvcrev1alpha1.FrameworkSpec{
					Exec: &nvcrev1alpha1.ExecFramework{Command: []string{"echo", "hi"}},
				},
			},
		}

		objects := []client.Object{run}
		if holder := buildHolder(input.Holder); holder != nil {
			objects = append(objects, holder)
		}

		statusWrites := 0
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&nvcrev1alpha1.WorkloadRun{}, &nvcrev1alpha1.Workflow{}).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string,
					obj client.Object, opts ...client.SubResourceUpdateOption,
				) error {
					statusWrites++
					if statusWrites <= input.StatusWriteFailures {
						return apierrors.NewConflict(
							schema.GroupResource{
								Group:    nvcrev1alpha1.GroupVersion.Group,
								Resource: "workloadruns",
							},
							obj.GetName(), fmt.Errorf("simulated lost status write"))
					}
					return cl.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).
			Build()

		// The test always reads through c, which plays the API server. The
		// reconciler reads through cached. They are the same object store, so
		// the only difference is what the case asks cached to withhold: with
		// cacheHidesWorkflow set, every Workflow Get through it answers
		// NotFound, which is exactly what a lagging informer does after a
		// Create that has not landed in the cache yet. APIReader is then the
		// only way to see the holder, so a fixture using it fails if the adopt
		// path is changed back to r.Get.
		cached := client.Client(c)
		var apiReader client.Reader
		if input.CacheHidesWorkflow {
			cached = interceptor.NewClient(c, interceptor.Funcs{
				Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					if _, isWorkflow := obj.(*nvcrev1alpha1.Workflow); isWorkflow {
						return apierrors.NewNotFound(schema.GroupResource{
							Group:    nvcrev1alpha1.GroupVersion.Group,
							Resource: workflowResourceName,
						}, key.Name)
					}
					return inner.Get(ctx, key, obj, opts...)
				},
			})
			apiReader = c
		}

		recorder := events.NewFakeRecorder(20)
		r := &WorkloadRunReconciler{
			Client: cached, APIReader: apiReader, Scheme: scheme, Recorder: recorder,
		}
		key := client.ObjectKey{Name: testRunName, Namespace: testNS}

		type pass struct {
			RequeueAfter string `json:"requeueAfter"`
			Error        string `json:"error,omitempty"`
			WorkflowRef  string `json:"workflowRef"`
		}
		passes := []pass{}
		for i := 0; i < input.Reconciles; i++ {
			result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			current := &nvcrev1alpha1.WorkloadRun{}
			if getErr := c.Get(ctx, key, current); getErr != nil {
				return getErr
			}
			p := pass{RequeueAfter: result.RequeueAfter.String(), WorkflowRef: testNilRef}
			if err != nil {
				p.Error = err.Error()
			}
			if current.Status.WorkflowRef != nil {
				p.WorkflowRef = current.Status.WorkflowRef.Name
			}
			passes = append(passes, p)
		}

		final := &nvcrev1alpha1.WorkloadRun{}
		if err := c.Get(ctx, key, final); err != nil {
			return err
		}
		for i := range final.Status.Conditions {
			final.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		// Whether the Workflow the run is bound to is the one it owns.
		boundToOwn := false
		if final.Status.WorkflowRef != nil {
			wf := &nvcrev1alpha1.Workflow{}
			if err := c.Get(ctx, key, wf); err == nil {
				boundToOwn = metav1.IsControlledBy(wf, final)
			}
		}

		output := struct {
			Passes      []pass             `json:"passes"`
			WorkflowRef string             `json:"finalWorkflowRef"`
			BoundToOwn  bool               `json:"boundToOwnWorkflow"`
			Conditions  []metav1.Condition `json:"conditions"`
			Events      []string           `json:"events"`
		}{
			Passes: passes, WorkflowRef: testNilRef, BoundToOwn: boundToOwn,
			Conditions: final.Status.Conditions, Events: []string{},
		}
		if final.Status.WorkflowRef != nil {
			output.WorkflowRef = final.Status.WorkflowRef.Name
		}
		for len(recorder.Events) > 0 {
			output.Events = append(output.Events, <-recorder.Events)
		}
		data, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// buildHolder returns a Workflow already occupying the run's name, owned as the
// case requires, or nil when the name is free.
//
// The three owners are deliberately distinct, because the reconciler splits on
// exactly that. "own" is this run. "predecessor" is an earlier WorkloadRun of
// the same name, which cannot still exist and so is a wait. "foreign" is an
// unrelated object, which is a real collision.
func buildHolder(kind string) *nvcrev1alpha1.Workflow {
	if kind == "" || kind == "none" {
		return nil
	}
	wf := &nvcrev1alpha1.Workflow{Name: testRunName, Namespace: testNS}
	owner := metav1.OwnerReference{
		APIVersion: nvcrev1alpha1.GroupVersion.String(),
		Kind:       workloadRunKind,
		Name:       testRunName,
		UID:        testRunUID,
		Controller: new(true),
	}
	switch {
	case strings.HasPrefix(kind, "predecessor"):
		// Same kind, same name, different UID: a WorkloadRun that was deleted
		// and recreated. That owner is gone, so the collector frees the name.
		owner.UID = "a-deleted-run-uid"
	case strings.HasPrefix(kind, "foreign"):
		// Nothing to do with this run: a different kind under a different name.
		owner.APIVersion = nvcrev1alpha1.GroupVersion.String()
		owner.Kind = testKindCertification
		owner.Name = "some-other-certification"
		owner.UID = "an-unrelated-uid"
	}
	wf.OwnerReferences = []metav1.OwnerReference{owner}
	if strings.HasSuffix(kind, "terminating") {
		now := metav1.Now()
		wf.DeletionTimestamp = &now
		wf.Finalizers = []string{workflowFinalizer}
	}
	return wf
}
