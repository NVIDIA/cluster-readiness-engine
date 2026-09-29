// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const (
	testSourceNamespace = "burn-in"
	testReportNamespace = "reports"
	actionCancel        = "cancel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testSource() *nvcrev1alpha1.Certification {
	return &nvcrev1alpha1.Certification{
		Name: "cert", Namespace: testSourceNamespace, UID: "source-uid", Generation: 1, CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)),
		Status: nvcrev1alpha1.CertificationStatus{Conditions: []metav1.Condition{{Type: "Failed", Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "CatalogRejected", Message: "the category could not be started", LastTransitionTime: metav1.NewTime(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))}}},
	}
}

func testExport() *nvcrev1alpha1.ReportExport {
	e := &nvcrev1alpha1.ReportExport{
		Name: "report", Namespace: testReportNamespace, UID: "export-uid", Generation: 1,
		Spec: nvcrev1alpha1.ReportExportSpec{
			SourceRef: nvcrev1alpha1.ReportExportSourceReference{Kind: "Certification", Name: "cert", Namespace: testSourceNamespace, UID: "source-uid"},
			PolicyRef: nvcrev1alpha1.ReportExportPolicyReference{Name: "policy", UID: "policy-uid", Generation: 1},
			ReportID:  reportID("source-uid", "policy-uid"), ClusterID: "test-cluster",
			Webhook: nvcrev1alpha1.ReportWebhook{URL: "https://receiver.example.test/reports", BearerTokenSecretRef: &corev1.SecretKeySelector{Name: "auth", Key: "token"}},
		},
	}
	defaultSpec(&e.Spec)
	return e
}

type lifecycleInput struct {
	Responses      []int    `json:"responses"`
	RetryAfter     string   `json:"retryAfter"`
	Actions        []string `json:"actions"`
	MaxAttempts    int32    `json:"maxAttempts"`
	Running        bool     `json:"running"`
	AllowHTTP      bool     `json:"allowHTTP"`
	LoseAcceptance bool     `json:"loseAcceptance"`
	URL            string   `json:"url"`
}
type lifecycleState struct {
	Action         string `json:"action"`
	Phase          string `json:"phase"`
	Reason         string `json:"reason,omitempty"`
	Result         string `json:"result,omitempty"`
	Attempts       int32  `json:"attempts"`
	CycleAttempts  int32  `json:"cycleAttempts"`
	HTTPStatus     int32  `json:"httpStatus,omitempty"`
	SnapshotReady  bool   `json:"snapshotReady"`
	HasSnapshot    bool   `json:"hasSnapshot"`
	RetryScheduled bool   `json:"retryScheduled"`
}
type requestRecord struct {
	ReportID         string `json:"reportID"`
	Digest           string `json:"digest"`
	ContentType      string `json:"contentType"`
	TokenVersion     int    `json:"tokenVersion"`
	AttemptPersisted bool   `json:"attemptPersisted"`
}

func TestDeliveryLifecycle(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "delivery"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input lifecycleInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		ctx := context.Background()
		source, export := testSource(), testExport()
		if input.Running {
			source.Status.Conditions[0].Type = "InProgress"
			source.Finalizers = []string{"test.nvcre/finalizer"}
		}
		if input.MaxAttempts > 0 {
			export.Spec.Retry.MaxAttempts = input.MaxAttempts
		}
		if input.URL != "" {
			export.Spec.Webhook.URL = input.URL
		}
		secret := &corev1.Secret{Name: "auth", Namespace: testReportNamespace, Data: map[string][]byte{"token": []byte("test-only-one")}}
		c := fake.NewClientBuilder().WithScheme(testScheme(tc.T)).WithStatusSubresource(export, source).WithObjects(source, export, secret).Build()
		now := time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)
		var requests []requestRecord
		var envelope Envelope
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				return nil, err
			}
			current := &nvcrev1alpha1.ReportExport{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(export), current); err != nil {
				return nil, err
			}
			tokenVersion := 0
			switch req.Header.Get("Authorization") {
			case "Bearer test-only-one":
				tokenVersion = 1
			case "Bearer test-only-two":
				tokenVersion = 2
			}
			requests = append(requests, requestRecord{ReportID: req.Header.Get("Idempotency-Key"), Digest: digest(body), ContentType: req.Header.Get("Content-Type"), TokenVersion: tokenVersion, AttemptPersisted: current.Status.Attempts == int32(len(requests)+1)})
			code := 202
			if len(input.Responses) > 0 {
				code = input.Responses[min(len(requests)-1, len(input.Responses)-1)]
			}
			if code == 0 {
				return nil, errors.New("simulated ambiguous transport failure")
			}
			header := http.Header{}
			header.Set("Retry-After", input.RetryAfter)
			if code >= 300 && code < 400 {
				header.Set("Location", "https://unexpected.example.test/")
			}
			return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader("receiver response omitted")), Request: req}, nil
		})
		r := &Reconciler{Client: c, APIReader: c, Namespace: testReportNamespace, AllowHTTP: input.AllowHTTP, Now: func() time.Time { return now }, HTTPClient: &http.Client{Transport: transport}}
		if input.LoseAcceptance {
			r.Client = &statusFailureClient{Client: c, phase: phaseDelivered}
		}
		var states []lifecycleState
		key := client.ObjectKeyFromObject(export)
		for _, action := range input.Actions {
			current := &nvcrev1alpha1.ReportExport{}
			if err := c.Get(ctx, key, current); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err := runLifecycleAction(ctx, c, r, key, source, secret, current, &now, action); err != nil {
				return err
			}
			current = &nvcrev1alpha1.ReportExport{}
			if err := c.Get(ctx, key, current); err != nil {
				if apierrors.IsNotFound(err) {
					states = append(states, lifecycleState{Action: action, Phase: "Deleted"})
					continue
				}
				return err
			}
			states = append(states, lifecycleState{Action: action, Phase: current.Status.Phase, Reason: current.Status.Reason, Result: current.Status.Result, Attempts: current.Status.Attempts, CycleAttempts: current.Status.CycleAttempts, HTTPStatus: current.Status.LastHTTPStatus, SnapshotReady: meta.IsStatusConditionTrue(current.Status.Conditions, SnapshotReady), HasSnapshot: current.Status.SnapshotRef != nil, RetryScheduled: current.Status.NextAttemptTime != nil})
		}
		output := struct {
			States   []lifecycleState `json:"states"`
			Requests []requestRecord  `json:"requests"`
			Envelope *Envelope        `json:"envelope,omitempty"`
		}{States: states, Requests: requests}
		if len(requests) > 0 {
			output.Envelope = &envelope
		}
		b, err := json.MarshalIndent(output, "", "  ")
		tc.Actual = string(b) + "\n"
		return err
	})
}

func runLifecycleAction(
	ctx context.Context, c client.Client, r *Reconciler, key client.ObjectKey,
	source *nvcrev1alpha1.Certification, secret *corev1.Secret,
	current *nvcrev1alpha1.ReportExport, now *time.Time, action string,
) error {
	switch action {
	case "reconcile":
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			return err
		}
	case "reconcile-conflict":
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); !apierrors.IsConflict(err) {
			return fmt.Errorf("expected outcome persistence conflict, got %v", err)
		}
	case "advance":
		if current.Status.NextAttemptTime == nil {
			return fmt.Errorf("advance requires a persisted retry")
		}
		*now = current.Status.NextAttemptTime.Add(time.Millisecond)
	case "deadline":
		*now = now.Add(25 * time.Hour)
	case "expire":
		*now = now.Add(31 * 24 * time.Hour)
	case "delete-source":
		if err := c.Delete(ctx, source); err != nil {
			return err
		}
	case "rotate-token":
		s := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(secret), s); err != nil {
			return err
		}
		s.Data["token"] = []byte("test-only-two")
		if err := c.Update(ctx, s); err != nil {
			return err
		}
	case "manual-retry":
		current.Spec.RetryNonce = "request-1"
		if err := c.Update(ctx, current); err != nil {
			return err
		}
	case actionCancel:
		current.Spec.Cancel = true
		if err := c.Update(ctx, current); err != nil {
			return err
		}
	case "lose-status":
		current.Status = nvcrev1alpha1.ReportExportStatus{}
		if err := c.Status().Update(ctx, current); err != nil {
			return err
		}
	case "delete-snapshot":
		if current.Status.SnapshotRef == nil {
			return fmt.Errorf("no snapshot to remove")
		}
		if err := c.Delete(ctx, &corev1.ConfigMap{Namespace: current.Namespace, Name: current.Status.SnapshotRef.Name}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown action %s", action)
	}
	return nil
}

func TestSnapshotReadFailureKeepsReady(t *testing.T) {
	ctx := context.Background()
	source, export := testSource(), testExport()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(export).WithObjects(source, export).Build()
	r := &Reconciler{Client: c, Namespace: testReportNamespace}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	r.APIReader = snapshotUnavailableReader{Reader: c}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected transient API read error")
	}
	if err := c.Get(ctx, req.NamespacedName, export); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(export.Status.Conditions, SnapshotReady) {
		t.Fatal("transient API failure revoked durable readiness")
	}
}

type snapshotUnavailableReader struct{ client.Reader }

func (r snapshotUnavailableReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return apierrors.NewServiceUnavailable("simulated API outage")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestSnapshotIdentityRejectsReplacement(t *testing.T) {
	ctx := context.Background()
	source, export := testSource(), testExport()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(export).WithObjects(source, export).Build()
	r := &Reconciler{Client: c, Namespace: testReportNamespace}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: testReportNamespace, Name: snapshotName(export)}, cm); err != nil {
		t.Fatal(err)
	}
	cm.OwnerReferences[0].UID = "wrong-owner"
	if _, _, err := validateSnapshot(cm, export); err == nil {
		t.Fatal("adopted another export's snapshot")
	}
}

// statusFailureClient simulates a crash-equivalent failure after the receiver
// accepts the request but before the successful outcome reaches the API server.
type statusFailureClient struct {
	client.Client
	phase  string
	failed bool
}

func (c *statusFailureClient) Status() client.SubResourceWriter {
	return statusFailureWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type statusFailureWriter struct {
	client.SubResourceWriter
	parent *statusFailureClient
}

func (w statusFailureWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if export, ok := obj.(*nvcrev1alpha1.ReportExport); ok && export.Status.Phase == w.parent.phase && !w.parent.failed {
		w.parent.failed = true
		return apierrors.NewConflict(nvcrev1alpha1.GroupVersion.WithResource("reportexports").GroupResource(), obj.GetName(), errors.New("simulated status conflict"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestAttemptPersistencePrecedesHTTP(t *testing.T) {
	ctx := context.Background()
	source, export := testSource(), testExport()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(export).WithObjects(source, export).Build()
	r := &Reconciler{Client: c, Namespace: testReportNamespace}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.Client = &statusFailureClient{Client: c, phase: phaseRetrying}
	r.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected HTTP") })}
	if _, err := r.Reconcile(ctx, request); !apierrors.IsConflict(err) {
		t.Fatalf("expected status conflict, got %v", err)
	}
	if calls != 0 {
		t.Fatal("sent an unrecorded HTTP attempt")
	}
}

func TestCancelledSnapshotRetentionAfterUnrecordedCreate(t *testing.T) {
	ctx := context.Background()
	source, export := testSource(), testExport()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(export).WithObjects(source, export).Build()
	now := time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)
	r := &Reconciler{Client: c, Namespace: testReportNamespace, Now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(export)}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, export); err != nil {
		t.Fatal(err)
	}
	export.Status = nvcrev1alpha1.ReportExportStatus{}
	if err := c.Status().Update(ctx, export); err != nil {
		t.Fatal(err)
	}
	export.Spec.Cancel = true
	if err := c.Update(ctx, export); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * 24 * time.Hour)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: testReportNamespace, Name: snapshotName(export)}, cm); !apierrors.IsNotFound(err) {
		t.Fatalf("unrecorded snapshot was retained: %v", err)
	}
}
