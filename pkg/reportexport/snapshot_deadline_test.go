// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type preparationState struct {
	Step          string       `json:"step"`
	Phase         string       `json:"phase"`
	Reason        string       `json:"reason"`
	Result        string       `json:"result,omitempty"`
	StartedAt     *metav1.Time `json:"startedAt,omitempty"`
	SnapshotReady bool         `json:"snapshotReady"`
	CleanupReady  bool         `json:"cleanupReady"`
}

func TestSnapshotPreparationDeadline(t *testing.T) {
	parser := &testutil.TestCaseParser{Subdir: "snapshot-deadline"}
	parser.TestDir(t, func(tc *testutil.TestCase) error {
		ctx := context.Background()
		scheme := testScheme(tc.T)
		objects, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}
		var source *nvcrev1alpha1.Certification
		for _, object := range objects {
			if cert, ok := object.(*nvcrev1alpha1.Certification); ok {
				source = cert
			}
		}
		if source == nil {
			return errors.New("fixture requires a certification")
		}
		reference := testExport()
		policy := &nvcrev1alpha1.ReportExportPolicy{
			Name: reference.Spec.PolicyRef.Name, Namespace: testReportNamespace, UID: reference.Spec.PolicyRef.UID, Generation: 1,
			Spec: nvcrev1alpha1.ReportExportPolicySpec{
				Source:  nvcrev1alpha1.ReportExportSourceSelector{Namespaces: []string{source.Namespace}},
				Webhook: nvcrev1alpha1.ReportWebhook{URL: "https://receiver.example.test/reports"},
			},
		}
		objects = append(objects, policy)
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&nvcrev1alpha1.ReportExport{}, &nvcrev1alpha1.BandwidthMeasurement{}).WithObjects(objects...).Build()
		if err := EnsureRegistered(ctx, c, source, testReportNamespace, reference.Spec.ClusterID); err != nil {
			return err
		}
		exports := &nvcrev1alpha1.ReportExportList{}
		if err := c.List(ctx, exports, client.InNamespace(testReportNamespace)); err != nil {
			return err
		}
		if len(exports.Items) != 1 {
			return errors.New("expected one registered export")
		}
		export := exports.Items[0].DeepCopy()
		export.UID = reference.UID
		if err := c.Update(ctx, export); err != nil {
			return err
		}
		now := time.Date(2026, 9, 30, 10, 2, 0, 0, time.UTC)
		key := client.ObjectKeyFromObject(export)
		var states []preparationState
		reconcile := func(step string) error {
			// Recreate the reconciler every time: all deadline state must survive a restart.
			r := &Reconciler{Client: c, APIReader: c, Namespace: testReportNamespace, Now: func() time.Time { return now }}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				return err
			}
			current := &nvcrev1alpha1.ReportExport{}
			if err := c.Get(ctx, key, current); err != nil {
				return err
			}
			ready, err := ReadyForCleanup(ctx, c, source, testReportNamespace)
			if err != nil {
				return err
			}
			states = append(states, preparationState{Step: step, Phase: current.Status.Phase, Reason: current.Status.Reason, Result: current.Status.Result, StartedAt: current.Status.SnapshotStartedAt, SnapshotReady: meta.IsStatusConditionTrue(current.Status.Conditions, SnapshotReady), CleanupReady: ready})
			return nil
		}
		if err := reconcile("measurement-pending"); err != nil {
			return err
		}
		now = now.Add(6 * time.Minute)
		if err := reconcile("deadline-after-restart"); err != nil {
			return err
		}
		if err := c.Get(ctx, key, export); err != nil {
			return err
		}
		export.Spec.RetryNonce = "retry-1"
		if err := c.Update(ctx, export); err != nil {
			return err
		}
		if err := reconcile("manual-retry"); err != nil {
			return err
		}
		if err := reconcile("new-preparation-window"); err != nil {
			return err
		}
		now = now.Add(4 * time.Minute)
		if err := reconcile("within-new-window"); err != nil {
			return err
		}
		if err := completeTestMeasurement(ctx, c, source.Namespace, now); err != nil {
			return err
		}
		if err := reconcile("measurement-completed"); err != nil {
			return err
		}
		data, err := json.MarshalIndent(states, "", "  ")
		tc.Actual = string(data) + "\n"
		return err
	})
}

func completeTestMeasurement(ctx context.Context, c client.Client, namespace string, now time.Time) error {
	measurement := &nvcrev1alpha1.BandwidthMeasurement{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "bw"}, measurement); err != nil {
		return err
	}
	measurement.Status.Conditions = []metav1.Condition{{Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue, ObservedGeneration: measurement.Generation, Reason: "Completed", LastTransitionTime: metav1.NewTime(now)}}
	return c.Status().Update(ctx, measurement)
}
