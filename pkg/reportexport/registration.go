// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package reportexport persists and delivers final certification reports.
package reportexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	// BindingsAnnotation freezes the selected policies before child workloads start.
	BindingsAnnotation = "nvcre.nvidia.com/report-export-bindings"
	// SourceUIDLabel supports looking up deliveries for one source incarnation.
	SourceUIDLabel = "nvcre.nvidia.com/report-source-uid"
	// SnapshotReady is independent of delivery success and gates source cleanup.
	SnapshotReady = "SnapshotReady"
	// SchemaVersion versions the webhook envelope separately from Kubernetes APIs.
	SchemaVersion     = "v1"
	registrationLabel = "nvcre.nvidia.com/report-registration"
)

type binding struct {
	Namespace string                         `json:"namespace"`
	Name      string                         `json:"name"`
	Spec      nvcrev1alpha1.ReportExportSpec `json:"spec"`
}

type registrationHint struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name,omitempty"`
}

// EnsureRegistered freezes matching policies and creates their delivery records.
// Frozen specifications live in an immutable ConfigMap in the trusted reporting
// namespace. User-writable source annotations contain only a reference, never
// instructions for which privileged Secret to send to which destination.
func EnsureRegistered(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification, namespace, clusterID string) error {
	_, found := cert.Annotations[BindingsAnnotation]
	if namespace == "" && !found {
		return nil
	}
	var bound []binding
	var err error
	if !found {
		bound, err = freezeBindings(ctx, c, cert, namespace, clusterID)
		if err != nil {
			return err
		}
		hint := registrationHint{Namespace: namespace, Name: registrationName(string(cert.UID))}
		data, marshalErr := json.Marshal(hint)
		if marshalErr != nil {
			return marshalErr
		}
		before := cert.DeepCopy()
		if cert.Annotations == nil {
			cert.Annotations = map[string]string{}
		}
		cert.Annotations[BindingsAnnotation] = string(data)
		if err := c.Patch(ctx, cert, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("freeze report export bindings: %w", err)
		}
	} else {
		bound, err = readBindings(ctx, c, cert, namespace)
		if err != nil {
			return err
		}
	}
	for _, b := range bound {
		export := &nvcrev1alpha1.ReportExport{
			Namespace: b.Namespace, Name: b.Name,
			Labels: map[string]string{SourceUIDLabel: string(cert.UID)},
			Spec:   b.Spec,
		}
		if err := c.Create(ctx, export); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("register report export: %w", err)
			}
			existing := &nvcrev1alpha1.ReportExport{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(export), existing); err != nil {
				return err
			}
			if existing.Spec.SourceRef.UID != cert.UID || existing.Spec.ReportID != b.Spec.ReportID ||
				existing.Spec.PolicyRef.UID != b.Spec.PolicyRef.UID {
				return fmt.Errorf("report export %s/%s has a different identity", b.Namespace, b.Name)
			}
		}
	}
	return nil
}

func registrationName(sourceUID string) string {
	sum := sha256.Sum256([]byte(sourceUID))
	return "registration-" + hex.EncodeToString(sum[:20])
}

func freezeBindings(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification, namespace, clusterID string) ([]binding, error) {
	key := client.ObjectKey{Namespace: namespace, Name: registrationName(string(cert.UID))}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, key, cm); err == nil {
		return decodeBindings(cm, cert, namespace)
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	bound, err := selectBindings(ctx, c, cert, namespace, clusterID)
	if err != nil {
		return nil, err
	}
	if len(bound) > 32 {
		return nil, fmt.Errorf("certification matches more than 32 report policies")
	}
	data, err := json.Marshal(bound)
	if err != nil {
		return nil, err
	}
	if len(data) > 64*1024 {
		return nil, fmt.Errorf("report registration exceeds 64 KiB")
	}
	source, err := json.Marshal(nvcrev1alpha1.ReportExportSourceReference{
		Kind: "Certification", Namespace: cert.Namespace, Name: cert.Name, UID: cert.UID,
	})
	if err != nil {
		return nil, err
	}
	cm = &corev1.ConfigMap{
		Namespace: namespace, Name: key.Name, Labels: map[string]string{registrationLabel: "true"},
		Immutable: new(true), Data: map[string]string{"bindings.json": string(data), "source.json": string(source)},
	}
	if err := c.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := c.Get(ctx, key, cm); err != nil {
			return nil, err
		}
		return decodeBindings(cm, cert, namespace)
	}
	return bound, nil
}

func decodeBindings(cm *corev1.ConfigMap, cert *nvcrev1alpha1.Certification, namespace string) ([]binding, error) {
	if cm.Namespace != namespace || cm.Name != registrationName(string(cert.UID)) || cm.Immutable == nil || !*cm.Immutable {
		return nil, fmt.Errorf("report registration ownership or immutability is invalid")
	}
	var source nvcrev1alpha1.ReportExportSourceReference
	if json.Unmarshal([]byte(cm.Data["source.json"]), &source) != nil || source.UID != cert.UID ||
		source.Name != cert.Name || source.Namespace != cert.Namespace {
		return nil, fmt.Errorf("report registration source identity is invalid")
	}
	raw := cm.Data["bindings.json"]
	if len(raw) > 64*1024 {
		return nil, fmt.Errorf("report registration exceeds 64 KiB")
	}
	var bound []binding
	if err := json.Unmarshal([]byte(raw), &bound); err != nil {
		return nil, fmt.Errorf("decode report registration: %w", err)
	}
	if len(bound) > 32 {
		return nil, fmt.Errorf("report registration exceeds 32 policies")
	}
	for _, b := range bound {
		id := reportID(string(cert.UID), string(b.Spec.PolicyRef.UID))
		if b.Namespace != namespace || b.Spec.SourceRef.UID != cert.UID ||
			b.Spec.SourceRef.Name != cert.Name || b.Spec.SourceRef.Namespace != cert.Namespace ||
			b.Spec.ReportID != id || b.Name != "report-"+id[:40] {
			return nil, fmt.Errorf("report registration source or destination identity is invalid")
		}
	}
	return bound, nil
}

func selectBindings(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification, namespace, clusterID string) ([]binding, error) {
	var policies nvcrev1alpha1.ReportExportPolicyList
	if err := c.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list report export policies: %w", err)
	}
	sort.Slice(policies.Items, func(i, j int) bool { return policies.Items[i].Name < policies.Items[j].Name })
	bound := make([]binding, 0, len(policies.Items))
	for _, policy := range policies.Items {
		if policy.Spec.Suspend || !policy.DeletionTimestamp.IsZero() ||
			cert.CreationTimestamp.Before(&policy.CreationTimestamp) ||
			!slices.Contains(policy.Spec.Source.Namespaces, cert.Namespace) {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.Source.Selector)
		if err != nil {
			return nil, fmt.Errorf("invalid selector in policy %s: %w", policy.Name, err)
		}
		if !selector.Matches(labels.Set(cert.Labels)) {
			continue
		}
		id := reportID(string(cert.UID), string(policy.UID))
		spec := nvcrev1alpha1.ReportExportSpec{
			ReportID: id, ClusterID: clusterID,
			SourceRef: nvcrev1alpha1.ReportExportSourceReference{
				Kind: "Certification", Namespace: cert.Namespace, Name: cert.Name, UID: cert.UID,
			},
			PolicyRef: nvcrev1alpha1.ReportExportPolicyReference{
				Name: policy.Name, UID: policy.UID, Generation: policy.Generation,
			},
			Webhook: policy.Spec.Webhook, Retry: policy.Spec.Retry, Retention: policy.Spec.Retention,
		}
		defaultSpec(&spec)
		bound = append(bound, binding{Namespace: namespace, Name: "report-" + id[:40], Spec: spec})
	}
	return bound, nil
}

func reportID(sourceUID, policyUID string) string {
	sum := sha256.Sum256([]byte(SchemaVersion + "\x00" + sourceUID + "\x00" + policyUID))
	return hex.EncodeToString(sum[:])
}

func readBindings(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification, namespace string) ([]binding, error) {
	var hint registrationHint
	if err := json.Unmarshal([]byte(cert.Annotations[BindingsAnnotation]), &hint); err != nil {
		return nil, fmt.Errorf("decode report registration reference: %w", err)
	}
	if namespace == "" || hint.Namespace != namespace {
		return nil, fmt.Errorf("restore the original report namespace configuration before continuing this registered certification")
	}
	if hint.Name != registrationName(string(cert.UID)) {
		return nil, fmt.Errorf("report registration reference does not match the certification UID")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: hint.Name}, cm); err != nil {
		return nil, err
	}
	return decodeBindings(cm, cert, namespace)
}

// ReadyForCleanup allows deleting source resources once every registered report
// has a durable snapshot or an explicit cancellation. Delivery may still retry.
func ReadyForCleanup(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification, namespace string) (bool, error) {
	_, found := cert.Annotations[BindingsAnnotation]
	if namespace == "" && !found {
		return true, nil
	}
	if !found {
		return false, nil
	}
	bound, err := readBindings(ctx, c, cert, namespace)
	if err != nil {
		return false, err
	}
	for _, b := range bound {
		export := &nvcrev1alpha1.ReportExport{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Name}, export); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if export.Spec.SourceRef.UID != cert.UID || export.Spec.ReportID != b.Spec.ReportID {
			return false, fmt.Errorf("report export %s/%s has a different identity", b.Namespace, b.Name)
		}
		if !export.Spec.Cancel && export.Status.Phase != "Skipped" && export.Status.Phase != "Cancelled" &&
			!meta.IsStatusConditionTrue(export.Status.Conditions, SnapshotReady) {
			return false, nil
		}
	}
	return true, nil
}

func defaultSpec(spec *nvcrev1alpha1.ReportExportSpec) {
	if spec.Webhook.Timeout.Duration <= 0 {
		spec.Webhook.Timeout.Duration = 10 * time.Second
	}
	if spec.Retry.InitialBackoff.Duration <= 0 {
		spec.Retry.InitialBackoff.Duration = 10 * time.Second
	}
	if spec.Retry.MaxBackoff.Duration <= 0 {
		spec.Retry.MaxBackoff.Duration = 15 * time.Minute
	}
	if spec.Retry.MaxElapsedTime.Duration <= 0 {
		spec.Retry.MaxElapsedTime.Duration = 24 * time.Hour
	}
	if spec.Retry.MaxAttempts <= 0 {
		spec.Retry.MaxAttempts = 100
	}
	if spec.Retention.Succeeded.Duration <= 0 {
		spec.Retention.Succeeded.Duration = 7 * 24 * time.Hour
	}
	if spec.Retention.Failed.Duration <= 0 {
		spec.Retention.Failed.Duration = 30 * 24 * time.Hour
	}
}
