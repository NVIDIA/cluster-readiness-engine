// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// testCertName is the Certification name used by the lookup fixtures.
const testCertName = "cert"

// testCertUID lets the fixtures build a Workflow that IsControlledBy resolves
// to this Certification, which the adopt branch turns on.
const testCertUID types.UID = "cert-uid"

// A manager-backed reconciler must never confirm a cache miss against the same
// cache, so SetupWithManager supplies the uncached reader when the caller
// leaves it unset (issue #384).
func TestCertificationSetupDefaultsAPIReader(t *testing.T) {
	mgr, err := ctrl.NewManager(&rest.Config{Host: testFakeAPIServerHost}, ctrl.Options{
		Scheme:     newFallbackScheme(t),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	require.NoError(t, err)
	r := &CertificationReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
	require.Same(t, mgr.GetAPIReader(), r.APIReader)
}

// TestCertificationWorkflowLookup pins what the Certification tier does when the
// Workflow named by an InProgress category is missing from the cache (issue
// #384). Burning the category is terminal for it, and the reconciler then
// starts the next category's Workflow alongside a burn-in that is still
// running, so the cache must not be the only witness.
//
// The reconciler's Client plays the informer cache and APIReader plays the API
// server, so each case sets exactly what each side can see. The last case
// leaves both in agreement and fails the status write instead, pinning that the
// category's own outcome survives a conflict refetch.
func TestCertificationWorkflowLookup(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-workflow-lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			// InCache and InAPI place the first category's Workflow in the cache
			// and on the API server respectively.
			InCache bool `yaml:"inCache"`
			InAPI   bool `yaml:"inAPI"`
			// Succeeded gives that Workflow a terminal Succeeded condition.
			Succeeded bool `yaml:"succeeded"`
			// APIError, when set, fails every live read with this message.
			APIError string `yaml:"apiError"`
			// AdoptHolder makes Create report the *second* category's Workflow
			// name already taken, and places the holder this Certification
			// already owns on the API server ("api-only") or on both sides
			// ("both"). "foreign-terminating" instead puts a holder owned by a
			// different Certification there, already being deleted. Empty
			// leaves that name free.
			AdoptHolder string `yaml:"adoptHolder"`
			// AdoptReadError fails the live read of the second category's
			// Workflow with this message and leaves every other live read
			// alone. That read is the one the adopt path makes after Create
			// reports the name taken, so this models a 5xx or a timeout
			// arriving at exactly that point.
			AdoptReadError string `yaml:"adoptReadError"`
			// StatusWriteFailures fails that many leading status writes with a
			// conflict, modelling a write lost to a lagging cache.
			StatusWriteFailures int `yaml:"statusWriteFailures"`
			// Reconciles is how many passes to drive.
			Reconciles int `yaml:"reconciles"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newFallbackScheme(t)
		categories := twoCatalogCategories(t)
		const firstWorkflowName = "cert-workflow-0"

		certification := &nvcrev1alpha1.Certification{
			Name: testCertName, Namespace: testNS, UID: testCertUID,
			Finalizers: []string{certificationFinalizer},
			Spec: nvcrev1alpha1.CertificationSpec{
				Target: nvcrev1alpha1.TargetSpec{
					NodeSelector: map[string]string{GPUNodeLabel: present},
				},
				Categories: categories,
			},
			Status: nvcrev1alpha1.CertificationStatus{
				Conditions: []metav1.Condition{{
					Type: nvcrev1alpha1.CertificationInProgress, Status: metav1.ConditionTrue,
					Reason: ReasonWorkflowCreated, Message: "Created Workflow for category 1 of 2",
				}},
				CategoryStatuses: []nvcrev1alpha1.CertificationCategoryStatus{
					{
						Domain: categories[0].Domain, Variant: categories[0].Variant,
						Status: categoryStatusInProgress,
						WorkflowRef: &nvcrev1alpha1.WorkflowReference{
							Name: firstWorkflowName, Namespace: testNS,
						},
					},
					{
						Domain: categories[1].Domain, Variant: categories[1].Variant,
						Status: categoryStatusPending,
					},
				},
			},
		}
		node := &corev1.Node{
			Name: testNodeA,
			Labels: map[string]string{
				GPUNodeLabel:        present,
				testGPUProductLabel: testGPUProductH100,
			},
			Spec: corev1.NodeSpec{ProviderID: testProviderIDAWS},
		}
		newWorkflow := func() *nvcrev1alpha1.Workflow {
			wf := &nvcrev1alpha1.Workflow{Name: firstWorkflowName, Namespace: testNS}
			if input.Succeeded {
				wf.Status.Conditions = []metav1.Condition{{
					Type: nvcrev1alpha1.WorkflowSucceeded, Status: metav1.ConditionTrue,
					Reason: ReasonJobCompleted, Message: "All iterations completed successfully",
				}}
			}
			return wf
		}

		// The Workflow the second category will try to create, and the holder
		// already occupying that name when the case asks for one.
		secondWorkflowName := (&CertificationReconciler{}).getWorkflowName(certification, categories[1])
		newHolder := func() *nvcrev1alpha1.Workflow {
			wf := &nvcrev1alpha1.Workflow{
				Name: secondWorkflowName, Namespace: testNS,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: nvcrev1alpha1.GroupVersion.String(),
					Kind:       testKindCertification,
					Name:       testCertName,
					UID:        testCertUID,
					Controller: new(true),
				}},
			}
			if input.AdoptHolder == "foreign-terminating" {
				wf.OwnerReferences[0].UID = "a-different-certification-uid"
				now := metav1.Now()
				wf.DeletionTimestamp = &now
				wf.Finalizers = []string{workflowFinalizer}
			}
			return wf
		}

		cacheObjects := []client.Object{certification, node}
		if input.InCache {
			cacheObjects = append(cacheObjects, newWorkflow())
		}
		if input.AdoptHolder == "both" {
			cacheObjects = append(cacheObjects, newHolder())
		}
		statusWrites := 0
		cache := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cacheObjects...).
			WithStatusSubresource(&nvcrev1alpha1.Certification{}, &nvcrev1alpha1.Workflow{}).
			WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object,
					opts ...client.CreateOption,
				) error {
					// Model the API server rejecting the duplicate create even
					// when this client's own store has no such object, which is
					// exactly the lag the confirming read exists for.
					if input.AdoptHolder != "" && obj.GetName() == secondWorkflowName {
						return apierrors.NewAlreadyExists(
							schema.GroupResource{
								Group:    nvcrev1alpha1.GroupVersion.Group,
								Resource: workflowResourceName,
							}, obj.GetName())
					}
					return cl.Create(ctx, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string,
					obj client.Object, opts ...client.SubResourceUpdateOption,
				) error {
					statusWrites++
					if statusWrites <= input.StatusWriteFailures {
						return apierrors.NewConflict(
							schema.GroupResource{
								Group:    nvcrev1alpha1.GroupVersion.Group,
								Resource: testCertificationsResource,
							},
							obj.GetName(), errors.New("simulated lost status write"))
					}
					return cl.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).
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
					if input.AdoptReadError != "" && key.Name == secondWorkflowName {
						return errors.New(input.AdoptReadError)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
		apiObjects := []client.Object{}
		if input.InAPI {
			apiObjects = append(apiObjects, newWorkflow())
		}
		if input.AdoptHolder != "" {
			apiObjects = append(apiObjects, newHolder())
		}
		if len(apiObjects) > 0 {
			apiBuilder = apiBuilder.WithObjects(apiObjects...).
				WithStatusSubresource(&nvcrev1alpha1.Workflow{})
		}

		recorder := events.NewFakeRecorder(20)
		r := &CertificationReconciler{
			Client: cache, APIReader: apiBuilder.Build(), Scheme: scheme, Recorder: recorder,
		}
		key := client.ObjectKeyFromObject(certification)

		type category struct {
			Status      string `json:"status"`
			WorkflowRef string `json:"workflowRef"`
		}
		type pass struct {
			RequeueAfter string     `json:"requeueAfter"`
			Error        string     `json:"error,omitempty"`
			Categories   []category `json:"categories"`
			Workflows    int        `json:"workflowsInCluster"`
		}
		passes := []pass{}
		for range input.Reconciles {
			result, reconcileErr := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			current := &nvcrev1alpha1.Certification{}
			if err := cache.Get(ctx, key, current); err != nil {
				return err
			}
			workflows := &nvcrev1alpha1.WorkflowList{}
			if err := cache.List(ctx, workflows, client.InNamespace(testNS)); err != nil {
				return err
			}
			p := pass{
				RequeueAfter: result.RequeueAfter.String(),
				Workflows:    len(workflows.Items),
				Categories:   []category{},
			}
			if reconcileErr != nil {
				p.Error = reconcileErr.Error()
			}
			for _, cs := range current.Status.CategoryStatuses {
				row := category{Status: cs.Status, WorkflowRef: testNilRef}
				if cs.WorkflowRef != nil {
					row.WorkflowRef = cs.WorkflowRef.Name
				}
				p.Categories = append(p.Categories, row)
			}
			passes = append(passes, p)
		}

		final := &nvcrev1alpha1.Certification{}
		if err := cache.Get(ctx, key, final); err != nil {
			return err
		}
		for i := range final.Status.Conditions {
			final.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		output := struct {
			Passes     []pass             `json:"passes"`
			APIReads   int                `json:"apiReads"`
			Conditions []metav1.Condition `json:"conditions"`
			Events     []string           `json:"events"`
		}{Passes: passes, APIReads: apiReads, Conditions: final.Status.Conditions, Events: []string{}}
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

// twoCatalogCategories returns two distinct real catalog entries, so the first
// category can stall while the second still has a Workflow to build. They are
// named rather than taken off the front of catalog.List(), because the golden
// files carry the rendered category names and the generated Workflow name.
func twoCatalogCategories(t *testing.T) []nvcrev1alpha1.CertificateCategory {
	t.Helper()
	out := []nvcrev1alpha1.CertificateCategory{
		{Domain: testDomainCommunication, Variant: "nccl-all-gather"},
		{Domain: testDomainCommunication, Variant: testVariantNCCLAllReduce},
	}
	for _, c := range out {
		require.NotNil(t, catalog.Lookup(c.Domain, c.Variant),
			"catalog entry %s/%s must be registered via blank import", c.Domain, c.Variant)
	}
	return out
}
