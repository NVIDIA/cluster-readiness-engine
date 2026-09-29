// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

const (
	// MaxPayloadBytes bounds JSON before compression and after decompression.
	MaxPayloadBytes = 8 * 1024 * 1024
	// MaxCompressedBytes leaves headroom below the Kubernetes object-size limit.
	MaxCompressedBytes = 512 * 1024
	snapshotKey        = "report.json.gz"
	kindReportExport   = "ReportExport"
)

var errPayloadTooLarge = errors.New("report exceeds the supported snapshot size")

type snapshotReadError struct{ error }

// Envelope is the versioned webhook request body. Report retains the CLI's JSON model.
type Envelope struct {
	SchemaVersion string             `json:"schemaVersion"`
	ReportID      string             `json:"reportID"`
	Source        Source             `json:"source"`
	GeneratedAt   time.Time          `json:"generatedAt"`
	Report        *report.CertReport `json:"report"`
}

// Source identifies the cluster and exact certification incarnation.
type Source struct {
	ClusterID                                 string `json:"clusterID"`
	nvcrev1alpha1.ReportExportSourceReference `json:",inline"`
	Reason                                    string `json:"reason,omitempty"`
	Message                                   string `json:"message,omitempty"`
}

func encodeSnapshot(body []byte) ([]byte, error) {
	if len(body) > MaxPayloadBytes {
		return nil, errPayloadTooLarge
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > MaxCompressedBytes {
		return nil, errPayloadTooLarge
	}
	return compressed.Bytes(), nil
}

func decodeSnapshot(compressed []byte) ([]byte, error) {
	if len(compressed) > MaxCompressedBytes {
		return nil, errPayloadTooLarge
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("invalid snapshot compression: %w", err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(io.LimitReader(reader, MaxPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}
	if len(body) > MaxPayloadBytes {
		return nil, errPayloadTooLarge
	}
	return body, nil
}

func snapshotName(export *nvcrev1alpha1.ReportExport) string {
	return "snapshot-" + export.Spec.ReportID[:min(40, len(export.Spec.ReportID))]
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validateSnapshot(cm *corev1.ConfigMap, export *nvcrev1alpha1.ReportExport) ([]byte, *Envelope, error) {
	owner := metav1.GetControllerOf(cm)
	if cm.Namespace != export.Namespace || owner == nil || owner.UID != export.UID ||
		owner.Kind != kindReportExport || cm.Immutable == nil || !*cm.Immutable {
		return nil, nil, errors.New("snapshot ownership or immutability does not match the export")
	}
	body, err := decodeSnapshot(cm.BinaryData[snapshotKey])
	if err != nil {
		return nil, nil, err
	}
	var envelope Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, nil, errors.New("snapshot does not contain a valid report envelope")
	}
	if envelope.SchemaVersion != SchemaVersion || envelope.ReportID != export.Spec.ReportID ||
		envelope.Source.ReportExportSourceReference != export.Spec.SourceRef ||
		envelope.Source.ClusterID != export.Spec.ClusterID || envelope.Report == nil ||
		(envelope.Report.Result != "PASSED" && envelope.Report.Result != "FAILED" && envelope.Report.Result != "INCOMPLETE") {
		return nil, nil, errors.New("snapshot identity or final result does not match the export")
	}
	if export.Status.SHA256 != "" && digest(body) != export.Status.SHA256 {
		return nil, nil, errors.New("snapshot digest does not match the persisted report")
	}
	return body, &envelope, nil
}

// persistSnapshot adopts an already-created immutable payload after a crash,
// rather than generating a new payload with a different generation timestamp.
func (r *Reconciler) persistSnapshot(ctx context.Context, export *nvcrev1alpha1.ReportExport, cert *nvcrev1alpha1.Certification) ([]byte, *Envelope, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: export.Namespace, Name: snapshotName(export)}
	if err := r.reader().Get(ctx, key, cm); err == nil {
		return validateSnapshot(cm, export)
	} else if !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	result, err := report.BuildSnapshot(ctx, r.sourceClient(), cert)
	if err != nil {
		return nil, nil, err
	}
	envelope := &Envelope{
		SchemaVersion: SchemaVersion, ReportID: export.Spec.ReportID,
		Source:      Source{ClusterID: export.Spec.ClusterID, ReportExportSourceReference: export.Spec.SourceRef},
		GeneratedAt: r.now().UTC(), Report: result,
	}
	for _, conditionType := range []string{nvcrev1alpha1.CertificationFailed, nvcrev1alpha1.CertificationSucceeded} {
		if condition := meta.FindStatusCondition(cert.Status.Conditions, conditionType); condition != nil && condition.Status == metav1.ConditionTrue {
			envelope.Source.Reason, envelope.Source.Message = condition.Reason, condition.Message
			break
		}
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	compressed, err := encodeSnapshot(body)
	if err != nil {
		return nil, nil, err
	}
	cm = &corev1.ConfigMap{
		Namespace: key.Namespace, Name: key.Name,
		Labels: map[string]string{SourceUIDLabel: string(export.Spec.SourceRef.UID)},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: kindReportExport,
			Name: export.Name, UID: export.UID, Controller: new(true), BlockOwnerDeletion: new(true),
		}},
		Immutable: new(true), BinaryData: map[string][]byte{snapshotKey: compressed},
	}
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, nil, err
		}
		if err := r.reader().Get(ctx, key, cm); err != nil {
			return nil, nil, err
		}
		return validateSnapshot(cm, export)
	}
	return body, envelope, nil
}

func (r *Reconciler) loadSnapshot(ctx context.Context, export *nvcrev1alpha1.ReportExport) ([]byte, error) {
	if export.Status.SnapshotRef == nil {
		return nil, errors.New("snapshot has expired or was never persisted")
	}
	cm := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, client.ObjectKey{
		Namespace: export.Namespace, Name: export.Status.SnapshotRef.Name,
	}, cm); err != nil {
		return nil, snapshotReadError{err}
	}
	body, _, err := validateSnapshot(cm, export)
	return body, err
}
