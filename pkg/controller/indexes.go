// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
)

// Field index names. Reads that filter on these fields must use
// client.MatchingFields so the informer cache can serve them as a keyed lookup;
// listing and filtering in Go walks every cached object of that kind on every
// call, which is what these indexes exist to avoid.
const (
	// podNodeNameIndexField indexes Pods by spec.nodeName (node health monitoring).
	podNodeNameIndexField = "spec.nodeName"

	// pvClaimRefIndexField indexes PersistentVolumes by the "namespace/name" of
	// the PVC they are bound to, so PV lookup during PVC cleanup is a keyed read
	// rather than a cluster-wide list.
	pvClaimRefIndexField = "spec.claimRef"

	// measurementJobRefIndexField indexes GoodputMeasurements and
	// BandwidthMeasurements by the Job they measure.
	measurementJobRefIndexField = "spec.jobRef.name"
)

// RegisterFieldIndexes registers every field index the controllers rely on.
//
// It must be called once per manager, before the controllers start, and is the
// single source of truth for both production (cmd/manager) and the integration
// harness — registering these in two places previously allowed prod and test to
// drift apart.
func RegisterFieldIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &corev1.Pod{}, podNodeNameIndexField,
		func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok || pod.Spec.NodeName == "" {
				return nil
			}
			return []string{pod.Spec.NodeName}
		}); err != nil {
		return fmt.Errorf("registering Pod nodeName index: %w", err)
	}

	if err := indexer.IndexField(ctx, &corev1.Pod{}, nodemonitor.PodCREJobIndexField,
		func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return nil
			}
			if jobName, found := pod.Labels[nodemonitor.CREJobLabel]; found {
				return []string{jobName}
			}
			return nil
		}); err != nil {
		return fmt.Errorf("registering Pod CRE job label index: %w", err)
	}

	if err := indexer.IndexField(ctx, &corev1.PersistentVolume{}, pvClaimRefIndexField,
		func(obj client.Object) []string {
			pv, ok := obj.(*corev1.PersistentVolume)
			if !ok || pv.Spec.ClaimRef == nil {
				return nil
			}
			return []string{pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name}
		}); err != nil {
		return fmt.Errorf("registering PersistentVolume claimRef index: %w", err)
	}

	if err := indexer.IndexField(ctx, &crev1alpha1.GoodputMeasurement{}, measurementJobRefIndexField,
		func(obj client.Object) []string {
			m, ok := obj.(*crev1alpha1.GoodputMeasurement)
			if !ok || m.Spec.JobRef.Name == "" {
				return nil
			}
			return []string{m.Spec.JobRef.Name}
		}); err != nil {
		return fmt.Errorf("registering GoodputMeasurement jobRef index: %w", err)
	}

	if err := indexer.IndexField(ctx, &crev1alpha1.BandwidthMeasurement{}, measurementJobRefIndexField,
		func(obj client.Object) []string {
			m, ok := obj.(*crev1alpha1.BandwidthMeasurement)
			if !ok || m.Spec.JobRef.Name == "" {
				return nil
			}
			return []string{m.Spec.JobRef.Name}
		}); err != nil {
		return fmt.Errorf("registering BandwidthMeasurement jobRef index: %w", err)
	}

	return nil
}

// matchingJobRef returns the list options selecting measurements for a Job.
func matchingJobRef(namespace, jobName string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{measurementJobRefIndexField: jobName},
	}
}
