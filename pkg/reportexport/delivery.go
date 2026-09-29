// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

type deliveryResult struct {
	StatusCode int
	Accepted   bool
	Retryable  bool
	Reason     string
	Message    string
	RetryAfter time.Duration
}

func (r *Reconciler) send(ctx context.Context, export *nvcrev1alpha1.ReportExport, body []byte) deliveryResult {
	target, err := url.Parse(export.Spec.Webhook.URL)
	if err != nil || target.Host == "" || target.User != nil || target.Fragment != "" ||
		(target.Scheme != "https" && (!r.AllowHTTP || target.Scheme != "http")) {
		return deliveryResult{Reason: "InvalidDestination", Message: "webhook requires an HTTPS URL without user information or a fragment"}
	}
	timeout := export.Spec.Webhook.Timeout.Duration
	if export.Status.CycleStartedAt != nil {
		timeout = min(timeout, export.Status.CycleStartedAt.Add(export.Spec.Retry.MaxElapsedTime.Duration).Sub(r.now()))
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return deliveryResult{Reason: "InvalidDestination", Message: "could not construct webhook request"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", export.Spec.ReportID)
	if ref := export.Spec.Webhook.BearerTokenSecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := r.reader().Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: ref.Name}, secret); err != nil {
			return deliveryResult{Retryable: true, Reason: "CredentialsUnavailable", Message: "could not read the configured authentication Secret"}
		}
		token, ok := secret.Data[ref.Key]
		if !ok || len(token) == 0 || strings.ContainsAny(string(token), "\r\n") {
			return deliveryResult{Reason: "InvalidCredentials", Message: "authentication Secret has no valid token at the configured key"}
		}
		request.Header.Set("Authorization", "Bearer "+string(token))
	}
	httpClient := http.Client{}
	if r.HTTPClient != nil {
		httpClient = *r.HTTPClient
	}
	// Never send credentials or report contents to a redirected destination.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(request)
	if err != nil {
		// Transport errors can contain the URL and headers; keep status sanitized.
		return deliveryResult{Retryable: true, Reason: "TransportError", Message: "webhook request failed before acceptance could be confirmed"}
	}
	defer func() { _ = response.Body.Close() }()
	// Read only a bounded diagnostic body and never persist or log its contents.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	result := classifyResponse(response.StatusCode)
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		result.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), r.now())
	}
	return result
}

func classifyResponse(code int) deliveryResult {
	result := deliveryResult{StatusCode: code, Message: fmt.Sprintf("receiver returned HTTP %d", code)}
	switch {
	case code >= 200 && code < 300:
		result.Accepted, result.Reason = true, "ReceiverAccepted"
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		result.Retryable, result.Reason = true, "ReceiverUnavailable"
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		result.Reason = "AuthenticationFailed"
	case code >= 300 && code < 400:
		result.Reason = "RedirectRejected"
	default:
		result.Reason = "ReceiverRejected"
	}
	return result
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		// A response cannot extend the 24-hour default window without bound.
		return time.Duration(min(seconds, int64(365*24*60*60))) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func retryDelay(spec nvcrev1alpha1.ReportExportSpec, attempts int32) time.Duration {
	delay := spec.Retry.InitialBackoff.Duration
	for i := int32(1); i < attempts && delay < spec.Retry.MaxBackoff.Duration; i++ {
		delay = min(delay*2, spec.Retry.MaxBackoff.Duration)
	}
	delay = min(delay, spec.Retry.MaxBackoff.Duration)
	// Downward jitter stays inside the configured maximum.
	return time.Duration(float64(delay) * (0.8 + rand.Float64()*0.2)) //nolint:gosec // Retry jitter is not security-sensitive.
}
