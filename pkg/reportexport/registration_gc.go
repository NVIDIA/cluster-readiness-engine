// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package reportexport

import (
	"context"
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// registrationCollector also handles a crash after the last export was removed
// but before its immutable registration could be collected.
type registrationCollector struct{ reconciler *Reconciler }

func (registrationCollector) NeedLeaderElection() bool { return true }

func (g registrationCollector) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := g.reconciler.collectRegistrations(ctx); err != nil && ctx.Err() == nil {
			logf.FromContext(ctx).Error(err, "unable to collect expired report registrations")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) collectRegistrations(ctx context.Context) error {
	var registrations corev1.ConfigMapList
	if err := r.reader().List(ctx, &registrations, client.InNamespace(r.Namespace),
		client.MatchingLabels{registrationLabel: "true"}); err != nil {
		return err
	}
	for i := range registrations.Items {
		cm := &registrations.Items[i]
		var source nvcrev1alpha1.ReportExportSourceReference
		if json.Unmarshal([]byte(cm.Data["source.json"]), &source) != nil || source.UID == "" {
			continue
		}
		cert := &nvcrev1alpha1.Certification{}
		err := r.reader().Get(ctx, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, cert)
		if err == nil && cert.UID == source.UID {
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			return err
		}
		var exports nvcrev1alpha1.ReportExportList
		if err := r.reader().List(ctx, &exports, client.InNamespace(r.Namespace),
			client.MatchingLabels{SourceUIDLabel: string(source.UID)}); err != nil {
			return err
		}
		if len(exports.Items) == 0 {
			if err := r.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
