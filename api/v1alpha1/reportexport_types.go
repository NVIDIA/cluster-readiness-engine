// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ReportExportSourceSelector selects new Certifications for automatic delivery.
type ReportExportSourceSelector struct {
	// kind is the source kind supported by this version.
	// +kubebuilder:default=Certification
	// +kubebuilder:validation:Enum=Certification
	// +optional
	Kind string `json:"kind,omitempty"`
	// namespaces explicitly limits which namespaces may supply reports.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Namespaces []string `json:"namespaces"`
	// selector matches labels on the Certification. Empty matches all sources
	// in the selected namespaces.
	// +optional
	Selector metav1.LabelSelector `json:"selector,omitempty"`
}

// ReportWebhook describes one HTTP receiver. Credentials remain in a Secret.
type ReportWebhook struct {
	// url is the HTTP POST destination. The exporter requires HTTPS unless its
	// administrator explicitly enables plain HTTP for development.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^\s]+$`
	URL string `json:"url"`
	// bearerTokenSecretRef selects a token in the policy/export namespace.
	// +optional
	BearerTokenSecretRef *corev1.SecretKeySelector `json:"bearerTokenSecretRef,omitempty"`
	// timeout bounds an individual request.
	// +kubebuilder:default="10s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('5m')",message="timeout must be between 1s and 5m"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitzero"`
}

// ReportExportRetry defines a bounded retry cycle.
// +kubebuilder:validation:XValidation:rule="!has(self.maxBackoff) || !has(self.initialBackoff) || duration(self.maxBackoff) >= duration(self.initialBackoff)",message="maxBackoff must not be less than initialBackoff"
type ReportExportRetry struct {
	// +kubebuilder:default="10s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('1h')",message="initialBackoff must be between 1s and 1h"
	// +optional
	InitialBackoff metav1.Duration `json:"initialBackoff,omitzero"`
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('24h')",message="maxBackoff must be between 1s and 24h"
	// +optional
	MaxBackoff metav1.Duration `json:"maxBackoff,omitzero"`
	// maxAttempts includes the first HTTP attempt in each retry cycle.
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
	// maxElapsedTime starts when delivery first attempts a stored snapshot.
	// +kubebuilder:default="24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('720h')",message="maxElapsedTime must be between 1s and 30 days"
	// +optional
	MaxElapsedTime metav1.Duration `json:"maxElapsedTime,omitzero"`
}

// ReportExportRetention bounds payload retention after a delivery cycle ends.
type ReportExportRetention struct {
	// +kubebuilder:default="168h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h') && duration(self) <= duration('8760h')",message="succeeded retention must be between 1h and 365 days"
	// +optional
	Succeeded metav1.Duration `json:"succeeded,omitzero"`
	// +kubebuilder:default="720h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h') && duration(self) <= duration('8760h')",message="failed retention must be between 1h and 365 days"
	// +optional
	Failed metav1.Duration `json:"failed,omitzero"`
}

// ReportExportPolicySpec defines a persistent subscription to final reports.
type ReportExportPolicySpec struct {
	Source  ReportExportSourceSelector `json:"source"`
	Webhook ReportWebhook              `json:"webhook"`
	// +kubebuilder:default={}
	// +optional
	Retry ReportExportRetry `json:"retry,omitempty"`
	// +kubebuilder:default={}
	// +optional
	Retention ReportExportRetention `json:"retention,omitempty"`
	// suspend stops new registrations; existing deliveries continue.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// ReportExportPolicy is configured once by an administrator in the reporting
// namespace. Policy updates affect subsequently registered exports.
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=rep
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.webhook.url`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ReportExportPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              ReportExportPolicySpec `json:"spec"`
}

// ReportExportPolicyList contains persistent report subscriptions.
// +kubebuilder:object:root=true
type ReportExportPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReportExportPolicy `json:"items"`
}

// ReportExportSourceReference identifies one immutable certification run.
type ReportExportSourceReference struct {
	// +kubebuilder:default=Certification
	// +kubebuilder:validation:Enum=Certification
	// +optional
	Kind string `json:"kind,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

// ReportExportPolicyReference identifies the policy revision captured at registration.
type ReportExportPolicyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

// ReportExportSpec freezes delivery inputs while allowing explicit operator actions.
// +kubebuilder:validation:XValidation:rule="self.sourceRef == oldSelf.sourceRef && self.policyRef == oldSelf.policyRef && self.webhook == oldSelf.webhook && self.retry == oldSelf.retry && self.retention == oldSelf.retention && self.clusterID == oldSelf.clusterID && self.reportID == oldSelf.reportID",message="delivery inputs are immutable; only cancel and retryNonce may change"
type ReportExportSpec struct {
	SourceRef ReportExportSourceReference `json:"sourceRef"`
	PolicyRef ReportExportPolicyReference `json:"policyRef"`
	Webhook   ReportWebhook               `json:"webhook"`
	// +kubebuilder:default={}
	Retry ReportExportRetry `json:"retry"`
	// +kubebuilder:default={}
	Retention ReportExportRetention `json:"retention"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ClusterID string `json:"clusterID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ReportID string `json:"reportID"`
	// cancel stops future attempts without changing the source result.
	// +optional
	Cancel bool `json:"cancel,omitempty"`
	// retryNonce requests a new bounded cycle for a retained, failed delivery.
	// Changing it preserves the payload, idempotency key, and total attempts.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	RetryNonce string `json:"retryNonce,omitempty"`
}

// ReportExportStatus separates local persistence from remote acceptance.
type ReportExportStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ReportID string `json:"reportID,omitempty"`
	// +optional
	Result string `json:"result,omitempty"`
	// +optional
	SnapshotRef *corev1.LocalObjectReference `json:"snapshotRef,omitempty"`
	// +optional
	SHA256 string `json:"sha256,omitempty"`
	// +optional
	PayloadBytes int64 `json:"payloadBytes,omitempty"`
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	CycleAttempts int32 `json:"cycleAttempts,omitempty"`
	// +optional
	LastHTTPStatus int32 `json:"lastHTTPStatus,omitempty"`
	// +optional
	SnapshotStartedAt *metav1.Time `json:"snapshotStartedAt,omitempty"`
	// +optional
	CycleStartedAt *metav1.Time `json:"cycleStartedAt,omitempty"`
	// +optional
	FirstAttemptTime *metav1.Time `json:"firstAttemptTime,omitempty"`
	// +optional
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`
	// +optional
	NextAttemptTime *metav1.Time `json:"nextAttemptTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// +optional
	ObservedRetryNonce string `json:"observedRetryNonce,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReportExport records the durable delivery of one report to one policy target.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rex
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
// +kubebuilder:printcolumn:name="Result",type=string,JSONPath=`.status.result`
// +kubebuilder:printcolumn:name="Delivery",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Attempts",type=integer,JSONPath=`.status.attempts`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ReportExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              ReportExportSpec `json:"spec"`
	// +optional
	Status ReportExportStatus `json:"status,omitzero"`
}

// ReportExportList contains report delivery records.
// +kubebuilder:object:root=true
type ReportExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ReportExport `json:"items"`
}

func init() {
	Register(&ReportExportPolicy{}, &ReportExportPolicyList{}, &ReportExport{}, &ReportExportList{})
}
