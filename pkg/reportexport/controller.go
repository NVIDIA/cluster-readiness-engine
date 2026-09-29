// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"context"
	"errors"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleropts "sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

const (
	phaseWaiting   = "WaitingForResult"
	phaseReady     = "SnapshotReady"
	phaseRetrying  = "Retrying"
	phaseDelivered = "Delivered"
	phaseFailed    = "Failed"
	phaseCancelled = "Cancelled"
	phaseSkipped   = "Skipped"
	pollInterval   = 15 * time.Second
	snapshotWindow = 5 * time.Minute
)

// Reconciler persists final reports and retries webhook delivery independently
// of the source controller. Only ReportExports are watched; uncached source reads
// let existing snapshots remain deliverable after core CRDs are uninstalled.
type Reconciler struct {
	client.Client
	APIReader  client.Reader
	Namespace  string
	AllowHTTP  bool
	HTTPClient *http.Client
	Now        func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

type readClient struct {
	client.Client
	reader client.Reader
}

func (c readClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c readClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

func (r *Reconciler) sourceClient() client.Client {
	return readClient{Client: r.Client, reader: r.reader()}
}

// SetupWithManager watches delivery records in the manager's scoped cache.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.Add(registrationCollector{reconciler: r}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&nvcrev1alpha1.ReportExport{}).
		WithOptions(controlleropts.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

// Reconcile records an attempt before performing network I/O, then persists the
// outcome. An ambiguous attempt is retried with the same payload and identifier.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}
	export := &nvcrev1alpha1.ReportExport{}
	if err := r.reader().Get(ctx, req.NamespacedName, export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !export.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	defaultSpec(&export.Spec)
	export.Status.ReportID = export.Spec.ReportID
	if export.Spec.Cancel && export.Status.Phase != phaseCancelled {
		return r.finish(ctx, export, phaseCancelled, "CancelledByOperator", "future delivery attempts have been cancelled")
	}
	if !export.Spec.Cancel && export.Status.Phase == phaseFailed && export.Spec.RetryNonce != export.Status.ObservedRetryNonce {
		return r.restart(ctx, export)
	}
	if terminal(export.Status.Phase) {
		return r.retain(ctx, export)
	}
	if !meta.IsStatusConditionTrue(export.Status.Conditions, SnapshotReady) {
		return r.prepare(ctx, export)
	}
	return r.deliver(ctx, export)
}

func terminal(phase string) bool {
	return phase == phaseDelivered || phase == phaseFailed || phase == phaseCancelled || phase == phaseSkipped
}

func (r *Reconciler) update(ctx context.Context, export *nvcrev1alpha1.ReportExport, delay time.Duration) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}

func (r *Reconciler) finish(ctx context.Context, export *nvcrev1alpha1.ReportExport, phase, reason, message string) (ctrl.Result, error) {
	export.Status.Phase, export.Status.Reason, export.Status.Message = phase, reason, message
	export.Status.CompletionTime = timePointer(r.now())
	export.Status.NextAttemptTime = nil
	return r.update(ctx, export, time.Second)
}

func timePointer(t time.Time) *metav1.Time {
	v := metav1.NewTime(t.UTC())
	return &v
}

func (r *Reconciler) snapshotCondition(export *nvcrev1alpha1.ReportExport, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&export.Status.Conditions, metav1.Condition{
		Type: SnapshotReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: export.Generation, LastTransitionTime: metav1.NewTime(r.now()),
	})
}

func (r *Reconciler) prepare(ctx context.Context, export *nvcrev1alpha1.ReportExport) (ctrl.Result, error) {
	// Recover creation-before-status crashes without reading the source at all.
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: export.Namespace, Name: snapshotName(export)}
	if err := r.reader().Get(ctx, key, cm); err == nil {
		body, envelope, err := validateSnapshot(cm, export)
		if err != nil {
			return r.finish(ctx, export, phaseFailed, "InvalidSnapshot", "the stored snapshot failed integrity or ownership validation")
		}
		return r.recordSnapshot(ctx, export, body, envelope)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	cert := &nvcrev1alpha1.Certification{}
	if err := r.reader().Get(ctx, client.ObjectKey{
		Namespace: export.Spec.SourceRef.Namespace, Name: export.Spec.SourceRef.Name,
	}, cert); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return r.finish(ctx, export, phaseFailed, "SourceUnavailable", "the source no longer exists and no snapshot was saved")
		}
		return ctrl.Result{}, err
	}
	if cert.UID != export.Spec.SourceRef.UID {
		return r.finish(ctx, export, phaseFailed, "SourceUIDMismatch", "the source name now belongs to a different certification")
	}
	certTerminal := meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded) ||
		meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	if !certTerminal {
		if !cert.DeletionTimestamp.IsZero() {
			return r.finish(ctx, export, phaseSkipped, "SourceDeletedBeforeCompletion", "the certification was deleted before producing a final report")
		}
		return r.waitForResult(ctx, export)
	}
	body, envelope, err := r.persistSnapshot(ctx, export, cert)
	if errors.Is(err, report.ErrSourceNotFinal) {
		if !cert.DeletionTimestamp.IsZero() {
			return r.finish(ctx, export, phaseSkipped, "SourceDeletedBeforeCompletion", "the certification was deleted before its execution results became final")
		}
		return r.waitForResult(ctx, export)
	}
	// Once execution is final, pending measurement collection uses the same
	// bounded preparation budget as API read or persistence failures below.
	if errors.Is(err, errPayloadTooLarge) {
		r.snapshotCondition(export, metav1.ConditionFalse, "PayloadTooLarge", "report exceeds 8 MiB JSON or 512 KiB compressed")
		return r.finish(ctx, export, phaseFailed, "PayloadTooLarge", "report exceeds 8 MiB JSON or 512 KiB compressed; source cleanup remains blocked")
	}
	if err != nil {
		return r.snapshotFailure(ctx, export, err)
	}
	return r.recordSnapshot(ctx, export, body, envelope)
}

func (r *Reconciler) waitForResult(ctx context.Context, export *nvcrev1alpha1.ReportExport) (ctrl.Result, error) {
	export.Status.Phase, export.Status.Reason = phaseWaiting, "SourceNotReady"
	export.Status.Message = "waiting for the certification and its measurements to finish"
	// A genuinely running child is not a failed snapshot attempt.
	export.Status.SnapshotStartedAt = nil
	return r.update(ctx, export, pollInterval)
}

func (r *Reconciler) snapshotFailure(ctx context.Context, export *nvcrev1alpha1.ReportExport, cause error) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "unable to prepare report snapshot", "reportExport", client.ObjectKeyFromObject(export), "sourceNamespace", export.Spec.SourceRef.Namespace, "sourceName", export.Spec.SourceRef.Name)
	if export.Status.SnapshotStartedAt == nil {
		export.Status.SnapshotStartedAt = timePointer(r.now())
	}
	r.snapshotCondition(export, metav1.ConditionFalse, "SnapshotBuildFailed", "could not collect or persist a complete final report")
	if !r.now().Before(export.Status.SnapshotStartedAt.Add(snapshotWindow)) {
		return r.finish(ctx, export, phaseFailed, "SnapshotBuildFailed", "report preparation failed for five minutes; source cleanup remains blocked")
	}
	export.Status.Phase, export.Status.Reason = phaseWaiting, "SnapshotBuildFailed"
	export.Status.Message = "could not collect or persist a complete final report; retrying preparation"
	return r.update(ctx, export, pollInterval)
}

func (r *Reconciler) recordSnapshot(ctx context.Context, export *nvcrev1alpha1.ReportExport, body []byte, envelope *Envelope) (ctrl.Result, error) {
	export.Status.SnapshotRef = &corev1.LocalObjectReference{Name: snapshotName(export)}
	export.Status.SHA256, export.Status.PayloadBytes = digest(body), int64(len(body))
	export.Status.Result = envelope.Report.Result
	export.Status.Phase, export.Status.Reason = phaseReady, "SnapshotPersisted"
	export.Status.Message = "the immutable report is saved and source cleanup may proceed"
	r.snapshotCondition(export, metav1.ConditionTrue, "SnapshotPersisted", "the complete webhook request is durably saved")
	return r.update(ctx, export, time.Millisecond)
}

func (r *Reconciler) restart(ctx context.Context, export *nvcrev1alpha1.ReportExport) (ctrl.Result, error) {
	export.Status.ObservedRetryNonce = export.Spec.RetryNonce
	if export.Status.SHA256 != "" {
		if _, err := r.loadSnapshot(ctx, export); err != nil {
			if transientSnapshotRead(err) {
				return ctrl.Result{}, err
			}
			return r.finish(ctx, export, phaseFailed, "SnapshotUnavailable", "the original snapshot is unavailable and cannot be recreated for a manual retry")
		}
	}
	export.Status.CycleAttempts = 0
	export.Status.CycleStartedAt = nil
	export.Status.SnapshotStartedAt = nil
	export.Status.CompletionTime = nil
	export.Status.NextAttemptTime = nil
	export.Status.Phase, export.Status.Reason, export.Status.Message = phaseWaiting, "RetryRequested", "a new bounded delivery cycle was requested"
	return r.update(ctx, export, time.Millisecond)
}

func (r *Reconciler) deliver(ctx context.Context, export *nvcrev1alpha1.ReportExport) (ctrl.Result, error) {
	now := r.now()
	if export.Status.CycleStartedAt != nil && !now.Before(export.Status.CycleStartedAt.Add(export.Spec.Retry.MaxElapsedTime.Duration)) {
		return r.finish(ctx, export, phaseFailed, "RetryDeadlineExceeded", "the delivery retry deadline has elapsed")
	}
	if export.Status.CycleAttempts >= export.Spec.Retry.MaxAttempts {
		return r.finish(ctx, export, phaseFailed, "RetryAttemptsExceeded", "the delivery attempt limit has been reached")
	}
	if next := export.Status.NextAttemptTime; next != nil && now.Before(next.Time) {
		return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
	}
	body, err := r.loadSnapshot(ctx, export)
	if err != nil {
		if transientSnapshotRead(err) {
			return ctrl.Result{}, err
		}
		r.snapshotCondition(export, metav1.ConditionFalse, "SnapshotUnavailable", "the saved report could not be loaded or verified")
		return r.finish(ctx, export, phaseFailed, "SnapshotUnavailable", "the saved report could not be loaded or verified")
	}
	if export.Status.CycleStartedAt == nil {
		export.Status.CycleStartedAt = timePointer(now)
	}
	if export.Status.FirstAttemptTime == nil {
		export.Status.FirstAttemptTime = timePointer(now)
	}
	export.Status.Attempts++
	export.Status.CycleAttempts++
	export.Status.LastAttemptTime = timePointer(now)
	backoff := retryDelay(export.Spec, export.Status.CycleAttempts)
	export.Status.NextAttemptTime = timePointer(now.Add(export.Spec.Webhook.Timeout.Duration + backoff))
	export.Status.Phase, export.Status.Reason = phaseRetrying, "AttemptStarted"
	export.Status.Message = "delivery attempt recorded; waiting for receiver acceptance"
	// Persist the attempt before sending. A restart after this point cannot reset
	// the retry budget, although it may need to retry an ambiguously accepted POST.
	if err := r.Status().Update(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	response := r.send(ctx, export, body)
	export.Status.LastHTTPStatus = int32(response.StatusCode)
	if response.Accepted {
		return r.finish(ctx, export, phaseDelivered, response.Reason, response.Message)
	}
	if !response.Retryable {
		return r.finish(ctx, export, phaseFailed, response.Reason, response.Message)
	}
	if export.Status.CycleAttempts >= export.Spec.Retry.MaxAttempts {
		return r.finish(ctx, export, phaseFailed, "RetryAttemptsExceeded", "the delivery attempt limit has been reached")
	}
	next := r.now().Add(max(backoff, response.RetryAfter))
	deadline := export.Status.CycleStartedAt.Add(export.Spec.Retry.MaxElapsedTime.Duration)
	if next.After(deadline) {
		next = deadline
	}
	export.Status.NextAttemptTime = timePointer(next)
	export.Status.Reason, export.Status.Message = response.Reason, response.Message
	return r.update(ctx, export, max(time.Millisecond, next.Sub(r.now())))
}

func (r *Reconciler) retain(ctx context.Context, export *nvcrev1alpha1.ReportExport) (ctrl.Result, error) {
	if export.Status.CompletionTime == nil {
		return r.finish(ctx, export, export.Status.Phase, export.Status.Reason, export.Status.Message)
	}
	retention := export.Spec.Retention.Failed.Duration
	if export.Status.Phase == phaseDelivered {
		retention = export.Spec.Retention.Succeeded.Duration
	}
	expires := export.Status.CompletionTime.Add(retention)
	if r.now().Before(expires) {
		return ctrl.Result{RequeueAfter: expires.Sub(r.now())}, nil
	}
	// A snapshot may exist even when a crash prevented its reference from being
	// recorded. Retention therefore checks the deterministic name and ownership.
	cm := &corev1.ConfigMap{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: snapshotName(export)}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil {
		owner := metav1.GetControllerOf(cm)
		if owner == nil || owner.UID != export.UID || owner.Kind != kindReportExport {
			return ctrl.Result{}, errors.New("refusing to remove a snapshot owned by another resource")
		}
		if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if export.Status.SnapshotRef != nil {
		export.Status.SnapshotRef = nil
		// Preserve SnapshotReady as evidence that controlled source cleanup was safe.
		if err := r.Status().Update(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}
	cert := &nvcrev1alpha1.Certification{}
	err = r.reader().Get(ctx, client.ObjectKey{Namespace: export.Spec.SourceRef.Namespace, Name: export.Spec.SourceRef.Name}, cert)
	if err == nil && cert.UID == export.Spec.SourceRef.UID {
		return ctrl.Result{RequeueAfter: 24 * time.Hour}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return ctrl.Result{}, err
	}
	if err := r.Delete(ctx, export); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func transientSnapshotRead(err error) bool {
	var readErr snapshotReadError
	return errors.As(err, &readErr) && !apierrors.IsNotFound(readErr.error)
}
