// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type conditionOut struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// A logProfileRef that does not resolve was swallowed: logged, then requeued,
// with no condition. The measurement then went terminal as
// Complete=True/JobSucceeded with zero results, which reads as a successful
// measurement rather than a skipped one.
func TestUnresolvedLogProfile(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "unresolved-log-profile",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Measurement     string `yaml:"measurement"`
			JobCondition    string `yaml:"jobCondition"`
			LogProfileRef   string `yaml:"logProfileRef"`
			ExistingResults int    `yaml:"existingResults"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := burninv1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		job := &burninv1alpha1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
			Status: burninv1alpha1.JobStatus{Conditions: []metav1.Condition{{
				Type: in.JobCondition, Status: metav1.ConditionTrue,
				Reason: "Test", LastTransitionTime: metav1.Now(),
			}}},
		}
		key := types.NamespacedName{Name: "m", Namespace: "ns"}
		req := ctrl.Request{NamespacedName: key}

		var conds []metav1.Condition
		var results int

		switch in.Measurement {
		case "bandwidth":
			m := &burninv1alpha1.BandwidthMeasurement{
				ObjectMeta: metav1.ObjectMeta{
					Name: "m", Namespace: "ns",
					Finalizers: []string{bandwidthMeasurementFinalizer},
				},
				Spec: burninv1alpha1.BandwidthMeasurementSpec{
					JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
					LogProfileRef: in.LogProfileRef,
				},
			}
			for i := 0; i < in.ExistingResults; i++ {
				m.Status.Results = append(m.Status.Results,
					burninv1alpha1.BandwidthResult{SizeBytes: 1024, AlgBW: "10", BusBW: "20"})
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(job, m).WithStatusSubresource(job, m).Build()
			r := &BandwidthMeasurementReconciler{Client: c, Scheme: scheme, RequeueInterval: time.Second}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				return err
			}
			got := &burninv1alpha1.BandwidthMeasurement{}
			if err := c.Get(context.Background(), key, got); err != nil {
				return err
			}
			conds, results = got.Status.Conditions, len(got.Status.Results)
		default:
			m := &burninv1alpha1.GoodputMeasurement{
				ObjectMeta: metav1.ObjectMeta{
					Name: "m", Namespace: "ns",
					Finalizers: []string{goodputMeasurementFinalizer},
				},
				Spec: burninv1alpha1.GoodputMeasurementSpec{
					JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
					LogProfileRef: in.LogProfileRef,
				},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(job, m).WithStatusSubresource(job, m).Build()
			r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				return err
			}
			got := &burninv1alpha1.GoodputMeasurement{}
			if err := c.Get(context.Background(), key, got); err != nil {
				return err
			}
			conds = got.Status.Conditions
		}

		out := struct {
			Conditions  []conditionOut `json:"conditions"`
			ResultCount int            `json:"resultCount"`
		}{ResultCount: results}
		for _, c := range conds {
			out.Conditions = append(out.Conditions, conditionOut{
				Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			})
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
