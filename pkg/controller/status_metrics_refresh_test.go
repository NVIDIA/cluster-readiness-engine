// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type refreshStatusInput struct {
	Kind          string `yaml:"kind"`
	ConditionType string `yaml:"conditionType"`
	Certification string `yaml:"certification"`
}

type refreshStatusResult struct {
	Failed     float64 `json:"failed"`
	InProgress float64 `json:"in_progress"`
	Succeeded  float64 `json:"succeeded"`
	Recorded   bool    `json:"recorded"`
}

func TestRefreshCertificationStatusMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "refresh-certification-status-metrics",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input refreshStatusInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ns, name := testNS, "cert-refresh-"+tc.Name
		cert := &nvcrev1alpha1.Certification{
			Name: name, Namespace: ns,
		}
		if input.ConditionType != "" {
			meta.SetStatusCondition(&cert.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "Persisted",
			})
		}

		baseline := promtest.CollectAndCount(certificationStatusGauge)
		refreshCertificationStatusMetrics(cert)
		defer cleanupCertificationMetrics(ns, name)

		result := refreshStatusResult{
			Recorded:   promtest.CollectAndCount(certificationStatusGauge) > baseline,
			InProgress: promtest.ToFloat64(certificationStatusGauge.WithLabelValues(ns, name, testMetricStatusInProgress)),
			Succeeded:  promtest.ToFloat64(certificationStatusGauge.WithLabelValues(ns, name, testMetricStatusSucceeded)),
			Failed:     promtest.ToFloat64(certificationStatusGauge.WithLabelValues(ns, name, testMetricStatusFailed)),
		}
		if !result.Recorded {
			// ToFloat64 creates series; strip them so this case only reports
			// that refresh was a no-op on an object with no exclusive condition.
			cleanupCertificationMetrics(ns, name)
			result.InProgress, result.Succeeded, result.Failed = 0, 0, 0
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestRefreshWorkflowStatusMetrics(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "refresh-workflow-status-metrics",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input refreshStatusInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ns, name := testNS, "wf-refresh-"+tc.Name
		wf := &nvcrev1alpha1.Workflow{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelCertification: input.Certification},
		}
		if input.ConditionType != "" {
			meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "Persisted",
			})
		}

		baseline := promtest.CollectAndCount(workflowStatusGauge)
		refreshWorkflowStatusMetrics(wf)
		defer cleanupWorkflowStatusMetrics(ns, name)

		result := refreshStatusResult{
			Recorded:   promtest.CollectAndCount(workflowStatusGauge) > baseline,
			InProgress: promtest.ToFloat64(workflowStatusGauge.WithLabelValues(ns, name, input.Certification, testMetricStatusInProgress)),
			Succeeded:  promtest.ToFloat64(workflowStatusGauge.WithLabelValues(ns, name, input.Certification, testMetricStatusSucceeded)),
			Failed:     promtest.ToFloat64(workflowStatusGauge.WithLabelValues(ns, name, input.Certification, testMetricStatusFailed)),
		}
		if !result.Recorded {
			cleanupWorkflowStatusMetrics(ns, name)
			result.InProgress, result.Succeeded, result.Failed = 0, 0, 0
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// Terminal objects return before setExclusiveCondition. After a controller
// restart the in-memory gauges are empty; Reconcile must republish from the
// persisted condition without writing status.
func TestReconcileRefreshesStatusGaugesWithoutStatusMutation(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "reconcile-refresh-status-metrics",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input refreshStatusInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newWorkflowScheme(tc.T.(*testing.T))
		name := "refresh-" + tc.Name

		var obj client.Object
		var beforeConditions []metav1.Condition
		switch input.Kind {
		case testKindCertification:
			cert := &nvcrev1alpha1.Certification{
				Name:       name,
				Namespace:  testNS,
				Finalizers: []string{certificationFinalizer},
			}
			meta.SetStatusCondition(&cert.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "AlreadyTerminal",
			})
			beforeConditions = append([]metav1.Condition(nil), cert.Status.Conditions...)
			obj = cert
		default:
			wf := &nvcrev1alpha1.Workflow{
				Name:       name,
				Namespace:  testNS,
				Finalizers: []string{workflowFinalizer},
				Labels:     map[string]string{labelCertification: input.Certification},
			}
			meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "AlreadyTerminal",
			})
			beforeConditions = append([]metav1.Condition(nil), wf.Status.Conditions...)
			obj = wf
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(obj).WithStatusSubresource(obj).Build()

		req := ctrl.Request{Name: name, Namespace: testNS}
		var err error
		switch input.Kind {
		case testKindCertification:
			_, err = (&CertificationReconciler{Client: c, Scheme: scheme}).Reconcile(ctx, req)
			defer cleanupCertificationMetrics(testNS, name)
		default:
			_, err = (&WorkflowReconciler{Client: c, Scheme: scheme}).Reconcile(ctx, req)
			defer cleanupWorkflowStatusMetrics(testNS, name)
		}
		if err != nil {
			return err
		}

		stored := obj.DeepCopyObject().(client.Object)
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
			return err
		}

		var after []metav1.Condition
		var gauges map[string]float64
		switch input.Kind {
		case testKindCertification:
			after = stored.(*nvcrev1alpha1.Certification).Status.Conditions
			gauges = exclusiveStatusGaugeValues(func(status string) float64 {
				return promtest.ToFloat64(certificationStatusGauge.WithLabelValues(testNS, name, status))
			})
		default:
			after = stored.(*nvcrev1alpha1.Workflow).Status.Conditions
			gauges = exclusiveStatusGaugeValues(func(status string) float64 {
				return promtest.ToFloat64(workflowStatusGauge.WithLabelValues(testNS, name, input.Certification, status))
			})
		}

		data, err := json.MarshalIndent(struct {
			Failed          float64 `json:"failed"`
			InProgress      float64 `json:"in_progress"`
			Succeeded       float64 `json:"succeeded"`
			StatusUnchanged bool    `json:"statusUnchanged"`
		}{
			Failed:          gauges[testMetricStatusFailed],
			InProgress:      gauges[testMetricStatusInProgress],
			Succeeded:       gauges[testMetricStatusSucceeded],
			StatusUnchanged: conditionsEqual(beforeConditions, after),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// After our finalizer is gone, another finalizer can keep a deleting object
// alive. Reconcile must not republish gauges that handleDeletion already
// cleaned, or the series leak until process restart.
func TestReconcileSkipsStatusGaugeRefreshWhileDeleting(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "reconcile-skip-refresh-while-deleting",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input refreshStatusInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newWorkflowScheme(tc.T.(*testing.T))
		name := "skip-refresh-" + tc.Name
		now := metav1.NewTime(time.Now())
		// Foreign finalizer keeps the object alive after our finalizer is gone,
		// which is the leak window CodeRabbit flagged.
		const holdFinalizer = "nvcre.nvidia.com/test-hold"

		var obj client.Object
		switch input.Kind {
		case testKindCertification:
			cert := &nvcrev1alpha1.Certification{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         testNS,
					DeletionTimestamp: &now,
					Finalizers:        []string{holdFinalizer},
				},
			}
			meta.SetStatusCondition(&cert.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "AlreadyTerminal",
			})
			obj = cert
		default:
			wf := &nvcrev1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{
					Name:              name,
					Namespace:         testNS,
					DeletionTimestamp: &now,
					Finalizers:        []string{holdFinalizer},
					Labels:            map[string]string{labelCertification: input.Certification},
				},
			}
			meta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
				Type:   input.ConditionType,
				Status: metav1.ConditionTrue,
				Reason: "AlreadyTerminal",
			})
			obj = wf
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(obj).WithStatusSubresource(obj).Build()

		req := ctrl.Request{Name: name, Namespace: testNS}
		var (
			err      error
			baseline int
		)
		switch input.Kind {
		case testKindCertification:
			baseline = promtest.CollectAndCount(certificationStatusGauge)
			_, err = (&CertificationReconciler{Client: c, Scheme: scheme}).Reconcile(ctx, req)
			defer cleanupCertificationMetrics(testNS, name)
		default:
			baseline = promtest.CollectAndCount(workflowStatusGauge)
			_, err = (&WorkflowReconciler{Client: c, Scheme: scheme}).Reconcile(ctx, req)
			defer cleanupWorkflowStatusMetrics(testNS, name)
		}
		if err != nil {
			return err
		}

		var (
			recorded bool
			gauges   map[string]float64
		)
		switch input.Kind {
		case testKindCertification:
			recorded = promtest.CollectAndCount(certificationStatusGauge) > baseline
			if recorded {
				gauges = exclusiveStatusGaugeValues(func(status string) float64 {
					return promtest.ToFloat64(certificationStatusGauge.WithLabelValues(testNS, name, status))
				})
			} else {
				cleanupCertificationMetrics(testNS, name)
				gauges = map[string]float64{}
			}
		default:
			recorded = promtest.CollectAndCount(workflowStatusGauge) > baseline
			if recorded {
				gauges = exclusiveStatusGaugeValues(func(status string) float64 {
					return promtest.ToFloat64(workflowStatusGauge.WithLabelValues(testNS, name, input.Certification, status))
				})
			} else {
				cleanupWorkflowStatusMetrics(testNS, name)
				gauges = map[string]float64{}
			}
		}

		data, err := json.MarshalIndent(struct {
			Failed     float64 `json:"failed"`
			InProgress float64 `json:"in_progress"`
			Succeeded  float64 `json:"succeeded"`
			Recorded   bool    `json:"recorded"`
		}{
			Failed:     gauges[testMetricStatusFailed],
			InProgress: gauges[testMetricStatusInProgress],
			Succeeded:  gauges[testMetricStatusSucceeded],
			Recorded:   recorded,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Status != b[i].Status || a[i].Reason != b[i].Reason {
			return false
		}
	}
	return true
}
