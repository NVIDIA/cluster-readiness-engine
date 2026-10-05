// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// cleanupPVForPVC must read the PVC from the API server, not the cache: the
// manager role cannot watch PVCs (issue #424). Client plays the cache and
// APIReader the API server; the PVC exists only on the API server.
func TestCleanupPVForPVCReadsPVCLive(t *testing.T) {
	const (
		pvcName     = "checkpoint"
		pvName      = "pv-checkpoint"
		workflowUID = types.UID("wf-uid")
	)
	ctx := context.Background()
	scheme := newFallbackScheme(t)

	workflow := &nvcrev1alpha1.Workflow{Name: testWorkflowName, Namespace: testNS, UID: workflowUID}
	pv := &corev1.PersistentVolume{
		Name:        pvName,
		Annotations: map[string]string{annotationWorkflowUID: string(workflowUID)},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeReleased},
	}
	pvc := &corev1.PersistentVolumeClaim{
		Name: pvcName, Namespace: testNS,
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
	}

	cachedPVCReads := 0
	cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(
				ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
			) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					cachedPVCReads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	r := &WorkflowReconciler{Client: cache, APIReader: api, Scheme: scheme}
	require.True(t, r.cleanupPVForPVC(ctx, workflow, testNS, pvcName))

	require.Zero(t, cachedPVCReads, "PVC read through the cache")
	got := &corev1.PersistentVolume{}
	require.NoError(t, cache.Get(ctx, client.ObjectKey{Name: pvName}, got))
	require.Equal(t, corev1.PersistentVolumeReclaimDelete, got.Spec.PersistentVolumeReclaimPolicy)
}
