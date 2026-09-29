// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/reportexport"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestReportExportLifecycle uses the API server's defaults, immutable ConfigMaps,
// status subresources and resource versions throughout registration and delivery.
// Removing the source between attempts proves the durable snapshot is sufficient.
func TestReportExportLifecycle(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{nvcreCRDDirectory}
	suite.Environment.ErrorIfCRDPathMissing = true
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)
	parser := &testutil.TestCaseParser{Subdir: "report-export", ExpectedSuffix: testutil.SuffixJSON}
	parser.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Retry         bool `json:"retry"`
			DeleteRunning bool `json:"deleteRunning"`
		}
		require.NoError(tc.T, yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input))
		ctx := context.Background()
		c := suite.Client
		ns := &corev1.Namespace{GenerateName: "report-export-"}
		require.NoError(tc.T, c.Create(ctx, ns))
		// envtest has no namespace or garbage-collection controller. Every case uses
		// an isolated namespace; tearing down the API server removes all fixtures.
		now := time.Now().UTC().Truncate(time.Second)
		var bodies [][]byte
		var keys, tokens []string
		var requestsMu sync.Mutex
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			requestsMu.Lock()
			defer requestsMu.Unlock()
			body, err := io.ReadAll(req.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodies = append(bodies, body)
			keys = append(keys, req.Header.Get("Idempotency-Key"))
			tokens = append(tokens, req.Header.Get("Authorization"))
			if input.Retry && len(bodies) == 1 {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}))
		defer server.Close()
		secret := &corev1.Secret{
			Name: "receiver", Namespace: ns.Name,
			Data: map[string][]byte{"token": []byte("first-test-token")},
		}
		require.NoError(tc.T, c.Create(ctx, secret))
		policy := &nvcrev1alpha1.ReportExportPolicy{
			Name: "webhook", Namespace: ns.Name,
			Spec: nvcrev1alpha1.ReportExportPolicySpec{
				Source: nvcrev1alpha1.ReportExportSourceSelector{Namespaces: []string{ns.Name}},
				Webhook: nvcrev1alpha1.ReportWebhook{
					URL:                  server.URL,
					BearerTokenSecretRef: &corev1.SecretKeySelector{Name: secret.Name, Key: "token"},
				},
			},
		}
		require.NoError(tc.T, c.Create(ctx, policy))
		cert := &nvcrev1alpha1.Certification{
			Name: "run", Namespace: ns.Name, Finalizers: []string{"integration.nvcre.nvidia.com/hold"},
			Spec: nvcrev1alpha1.CertificationSpec{
				Categories: []nvcrev1alpha1.CertificateCategory{{Domain: "communication", Variant: "nccl-all-reduce"}},
			},
		}
		require.NoError(tc.T, c.Create(ctx, cert))
		require.NoError(tc.T, reportexport.EnsureRegistered(ctx, c, cert, ns.Name, "integration-cluster"))
		var exports nvcrev1alpha1.ReportExportList
		require.NoError(tc.T, c.List(ctx, &exports, client.InNamespace(ns.Name)))
		require.Len(tc.T, exports.Items, 1)
		export := exports.Items[0].DeepCopy()
		// Updating/deleting a policy does not mutate the registered delivery.
		policy.Spec.Webhook.URL = "https://changed.example.test/unused"
		require.NoError(tc.T, c.Update(ctx, policy))
		require.NoError(tc.T, c.Delete(ctx, policy))
		require.NoError(tc.T, reportexport.EnsureRegistered(ctx, c, cert, ns.Name, "integration-cluster"))
		readyBefore, err := reportexport.ReadyForCleanup(ctx, c, cert, ns.Name)
		require.NoError(tc.T, err)
		if input.DeleteRunning {
			require.NoError(tc.T, c.Delete(ctx, cert))
		} else {
			cert.Status.Conditions = []metav1.Condition{{
				Type: nvcrev1alpha1.CertificationFailed, Status: metav1.ConditionTrue,
				Reason: "InvalidConfiguration", Message: "The certification was rejected before workload creation",
				ObservedGeneration: cert.Generation, LastTransitionTime: metav1.NewTime(now),
			}}
			require.NoError(tc.T, c.Status().Update(ctx, cert))
		}
		reconciler := &reportexport.Reconciler{
			Client: c, APIReader: c, Namespace: ns.Name, AllowHTTP: true,
			HTTPClient: server.Client(), Now: func() time.Time { return now },
		}
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}
		_, err = reconciler.Reconcile(ctx, req)
		require.NoError(tc.T, err)
		require.NoError(tc.T, c.Get(ctx, req.NamespacedName, export))
		readyAfter, err := reportexport.ReadyForCleanup(ctx, c, cert, ns.Name)
		require.NoError(tc.T, err)
		saved := meta.IsStatusConditionTrue(export.Status.Conditions, reportexport.SnapshotReady)
		phaseAfterSnapshot := export.Status.Phase
		snapshotImmutable := false
		if export.Status.SnapshotRef != nil {
			cm := &corev1.ConfigMap{}
			require.NoError(tc.T, c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: export.Status.SnapshotRef.Name}, cm))
			snapshotImmutable = cm.Immutable != nil && *cm.Immutable && metav1.GetControllerOf(cm).UID == export.UID
		}
		// A new reconciler can deliver without the Certification and its descendants.
		require.NoError(tc.T, c.Get(ctx, client.ObjectKeyFromObject(cert), cert))
		cert.Finalizers = nil
		require.NoError(tc.T, c.Update(ctx, cert))
		if !input.DeleteRunning {
			require.NoError(tc.T, c.Delete(ctx, cert))
		}
		reconciler = &reportexport.Reconciler{
			Client: c, APIReader: c, Namespace: ns.Name, AllowHTTP: true,
			HTTPClient: server.Client(), Now: func() time.Time { return now },
		}
		_, err = reconciler.Reconcile(ctx, req)
		require.NoError(tc.T, err)
		require.NoError(tc.T, c.Get(ctx, req.NamespacedName, export))
		firstDeliveryPhase := export.Status.Phase
		if input.Retry {
			require.NotNil(tc.T, export.Status.NextAttemptTime)
			require.GreaterOrEqual(tc.T, export.Status.NextAttemptTime.Sub(now), time.Minute)
			now = export.Status.NextAttemptTime.Add(time.Second)
			secret.Data["token"] = []byte("rotated-test-token")
			require.NoError(tc.T, c.Update(ctx, secret))
			_, err = reconciler.Reconcile(ctx, req)
			require.NoError(tc.T, err)
			require.NoError(tc.T, c.Get(ctx, req.NamespacedName, export))
		}
		requestsMu.Lock()
		defer requestsMu.Unlock()
		result := struct {
			ReadyBefore        bool   `json:"readyBeforeSnapshot"`
			ReadyAfter         bool   `json:"readyAfterSnapshot"`
			SnapshotReady      bool   `json:"snapshotReady"`
			SnapshotImmutable  bool   `json:"snapshotImmutable"`
			SnapshotPhase      string `json:"snapshotPhase"`
			FirstDeliveryPhase string `json:"firstDeliveryPhase"`
			FinalPhase         string `json:"finalPhase"`
			Reason             string `json:"reason"`
			Attempts           int32  `json:"attempts"`
			Requests           int    `json:"requests"`
			SameBody           bool   `json:"sameBody"`
			StableIdentity     bool   `json:"stableIdentity"`
			RotatedToken       bool   `json:"rotatedToken"`
			ReportResult       string `json:"reportResult,omitempty"`
			SourceReason       string `json:"sourceReason,omitempty"`
		}{
			ReadyBefore: readyBefore, ReadyAfter: readyAfter, SnapshotReady: saved, SnapshotImmutable: snapshotImmutable,
			SnapshotPhase: phaseAfterSnapshot, FirstDeliveryPhase: firstDeliveryPhase, FinalPhase: export.Status.Phase,
			Reason: export.Status.Reason, Attempts: export.Status.Attempts, Requests: len(bodies),
			SameBody: true, StableIdentity: true,
		}
		for i := range bodies {
			result.SameBody = result.SameBody && bytes.Equal(bodies[0], bodies[i])
			result.StableIdentity = result.StableIdentity && keys[i] == export.Spec.ReportID
		}
		if len(tokens) == 2 {
			result.RotatedToken = tokens[0] != tokens[1] && tokens[1] == "Bearer rotated-test-token"
		}
		if len(bodies) > 0 {
			var envelope reportexport.Envelope
			require.NoError(tc.T, json.Unmarshal(bodies[0], &envelope))
			require.Equal(tc.T, "integration-cluster", envelope.Source.ClusterID)
			require.Equal(tc.T, export.Spec.ReportID, envelope.ReportID)
			result.ReportResult, result.SourceReason = envelope.Report.Result, envelope.Source.Reason
		}
		actual, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(actual) + "\n"
		return nil
	})
}
