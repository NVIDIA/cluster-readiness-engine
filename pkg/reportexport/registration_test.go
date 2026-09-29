// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type registrationInput struct {
	Mode    string   `json:"mode"`
	Actions []string `json:"actions"`
}
type registrationState struct {
	Action        string             `json:"action"`
	Error         string             `json:"error,omitempty"`
	CleanupReady  bool               `json:"cleanupReady"`
	Exports       []registeredExport `json:"exports"`
	Registrations int                `json:"registrations"`
}
type registeredExport struct {
	Name             string    `json:"name"`
	URL              string    `json:"url"`
	SourceUID        types.UID `json:"sourceUID"`
	PolicyUID        types.UID `json:"policyUID"`
	PolicyGeneration int64     `json:"policyGeneration"`
}

func TestRegistration(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "registration"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input registrationInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		ctx := context.Background()
		cert := testSource()
		namespace := testReportNamespace
		policy := &nvcrev1alpha1.ReportExportPolicy{
			Name: "policy", Namespace: namespace, UID: "policy-uid", Generation: 1, CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)),
			Spec: nvcrev1alpha1.ReportExportPolicySpec{Source: nvcrev1alpha1.ReportExportSourceSelector{Kind: "Certification", Namespaces: []string{testSourceNamespace}}, Webhook: nvcrev1alpha1.ReportWebhook{URL: "https://receiver.example.test/original"}},
		}
		switch input.Mode {
		case "disabled":
			namespace = ""
		case "future-policy":
			policy.CreationTimestamp = metav1.NewTime(cert.CreationTimestamp.Add(time.Hour))
		case "selector-miss":
			policy.Spec.Source.Selector.MatchLabels = map[string]string{"report": "enabled"}
		case "suspended":
			policy.Spec.Suspend = true
		case "wrong-source-namespace":
			policy.Spec.Source.Namespaces = []string{"other"}
		case "forged-empty":
			cert.Annotations = map[string]string{BindingsAnnotation: `{"namespace":"reports"}`}
		case "forged-namespace":
			cert.Annotations = map[string]string{BindingsAnnotation: `{"namespace":"other","name":"x"}`}
		case "forged-spec":
			cert.Annotations = map[string]string{BindingsAnnotation: `[{"namespace":"reports","name":"forged","spec":{"webhook":{"url":"https://untrusted.example/"}}}]`}
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(tc.T)).WithStatusSubresource(&nvcrev1alpha1.ReportExport{}).WithObjects(cert, policy).Build()
		var states []registrationState
		for _, action := range input.Actions {
			var actionErr error
			switch action {
			case "register":
				actionErr = EnsureRegistered(ctx, c, cert, namespace, "test-cluster")
			case "change-policy":
				current := &nvcrev1alpha1.ReportExportPolicy{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
					return err
				}
				current.Spec.Webhook.URL = "https://receiver.example.test/changed"
				current.Generation = 2
				actionErr = c.Update(ctx, current)
			case "delete-policy":
				actionErr = c.Delete(ctx, policy)
			case "lose-annotation":
				before := cert.DeepCopy()
				delete(cert.Annotations, BindingsAnnotation)
				actionErr = c.Patch(ctx, cert, client.MergeFrom(before))
			case "disable":
				namespace = ""
			case actionCancel, "snapshot-ready":
				var exports nvcrev1alpha1.ReportExportList
				if err := c.List(ctx, &exports); err != nil {
					return err
				}
				for i := range exports.Items {
					e := &exports.Items[i]
					if action == actionCancel {
						e.Spec.Cancel = true
						actionErr = c.Update(ctx, e)
					} else {
						e.Status.Conditions = []metav1.Condition{{Type: SnapshotReady, Status: metav1.ConditionTrue, Reason: "SnapshotPersisted", LastTransitionTime: metav1.Now()}}
						actionErr = c.Status().Update(ctx, e)
					}
					if actionErr != nil {
						return actionErr
					}
				}
			case "delete-source":
				actionErr = c.Delete(ctx, cert)
			case "collect":
				actionErr = (&Reconciler{Client: c, Namespace: testReportNamespace}).collectRegistrations(ctx)
			default:
				return fmt.Errorf("unknown action %s", action)
			}
			state := registrationState{Action: action}
			if actionErr != nil {
				state.Error = actionErr.Error()
			}
			ready, err := ReadyForCleanup(ctx, c, cert, namespace)
			state.CleanupReady = ready
			if err != nil && state.Error == "" {
				state.Error = err.Error()
			}
			var exports nvcrev1alpha1.ReportExportList
			if err := c.List(ctx, &exports); err != nil {
				return err
			}
			for _, e := range exports.Items {
				state.Exports = append(state.Exports, registeredExport{Name: e.Name, URL: e.Spec.Webhook.URL, SourceUID: e.Spec.SourceRef.UID, PolicyUID: e.Spec.PolicyRef.UID, PolicyGeneration: e.Spec.PolicyRef.Generation})
			}
			var cms corev1.ConfigMapList
			if err := c.List(ctx, &cms, client.MatchingLabels{registrationLabel: "true"}); err != nil {
				return err
			}
			state.Registrations = len(cms.Items)
			states = append(states, state)
		}
		b, err := json.MarshalIndent(states, "", "  ")
		tc.Actual = string(b) + "\n"
		return err
	})
}
