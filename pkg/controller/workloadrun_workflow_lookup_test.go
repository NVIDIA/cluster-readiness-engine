// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A manager-backed reconciler must never confirm a cache miss against the
// same cache, so SetupWithManager supplies the uncached reader when the
// caller leaves it unset (issue #352).
func TestWorkloadRunSetupDefaultsAPIReader(t *testing.T) {
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:0"}, ctrl.Options{
		Scheme:     newWorkflowScheme(t),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	require.NoError(t, err)
	r := &WorkloadRunReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	require.Same(t, mgr.GetAPIReader(), r.APIReader)
}

// TestWorkloadRunWorkflowLookup pins how a WorkloadRun with a workflowRef
// treats a Workflow missing from the cache (issue #352). The reconciler's
// Client plays the informer cache and APIReader plays the API server, so
// each case sets exactly what each side can see.
func TestWorkloadRunWorkflowLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workloadrun-workflow-lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// InCache and InAPI place a Succeeded Workflow in the cache and on
			// the API server respectively.
			InCache bool `yaml:"inCache"`
			InAPI   bool `yaml:"inAPI"`
			// APIError, when set, fails every live read with this message.
			APIError string `yaml:"apiError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newWorkflowScheme(t)
		run := &nvcrev1alpha1.WorkloadRun{
			Name: testRunName, Namespace: testNS,
			Status: nvcrev1alpha1.WorkloadRunStatus{
				WorkflowRef: &nvcrev1alpha1.WorkflowReference{Name: testRunName, Namespace: testNS},
			},
		}
		(&WorkloadRunReconciler{}).setWorkloadRunCondition(run,
			nvcrev1alpha1.WorkloadRunInProgress, ReasonWorkflowCreated, "Workflow run created")
		newWorkflow := func() *nvcrev1alpha1.Workflow {
			return &nvcrev1alpha1.Workflow{
				Name: testRunName, Namespace: testNS,
				Status: nvcrev1alpha1.WorkflowStatus{Conditions: []metav1.Condition{{
					Type: nvcrev1alpha1.WorkflowSucceeded, Status: metav1.ConditionTrue,
					Reason: ReasonJobCompleted, Message: "All iterations completed successfully",
				}}},
			}
		}

		cacheObjects := []client.Object{run}
		if input.InCache {
			cacheObjects = append(cacheObjects, newWorkflow())
		}
		cache := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cacheObjects...).
			WithStatusSubresource(&nvcrev1alpha1.WorkloadRun{}, &nvcrev1alpha1.Workflow{}).
			Build()

		apiReads := 0
		apiBuilder := fake.NewClientBuilder().WithScheme(scheme).
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption,
				) error {
					apiReads++
					if input.APIError != "" {
						return errors.New(input.APIError)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
		if input.InAPI {
			apiBuilder = apiBuilder.WithObjects(newWorkflow()).
				WithStatusSubresource(&nvcrev1alpha1.Workflow{})
		}

		recorder := events.NewFakeRecorder(10)
		r := &WorkloadRunReconciler{
			Client: cache, APIReader: apiBuilder.Build(), Scheme: scheme, Recorder: recorder,
		}
		result, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})

		current := &nvcrev1alpha1.WorkloadRun{}
		if err := cache.Get(ctx, client.ObjectKeyFromObject(run), current); err != nil {
			return err
		}
		for i := range current.Status.Conditions {
			current.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		output := struct {
			RequeueAfter string             `json:"requeueAfter"`
			Error        string             `json:"error,omitempty"`
			APIReads     int                `json:"apiReads"`
			Conditions   []metav1.Condition `json:"conditions"`
			Events       []string           `json:"events"`
		}{
			RequeueAfter: result.RequeueAfter.String(),
			APIReads:     apiReads,
			Conditions:   current.Status.Conditions,
			Events:       []string{},
		}
		if reconcileErr != nil {
			output.Error = reconcileErr.Error()
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
