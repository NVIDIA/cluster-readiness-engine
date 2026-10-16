// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// The manager client must never cache ConfigMaps or PVCs (ADR-092): a cached
// read starts a cluster-wide informer that rbac.scope: namespaced cannot list,
// and the manager loops on a forbidden watch (issue #424). PersistentVolumes
// must stay cached, since findPVByClaimRef reads the cached claimRef index.
func TestClientOptionsReadConfigMapsAndPVCsLive(t *testing.T) {
	opts := ClientOptions()
	require.NotNil(t, opts.Cache)

	uncached := map[reflect.Type]bool{}
	for _, obj := range opts.Cache.DisableFor {
		uncached[reflect.TypeOf(obj)] = true
	}
	for _, tc := range []struct {
		obj  client.Object
		want bool
	}{
		{obj: &corev1.ConfigMap{}, want: true},
		{obj: &corev1.PersistentVolumeClaim{}, want: true},
		{obj: &corev1.PersistentVolume{}, want: false},
	} {
		require.Equal(t, tc.want, uncached[reflect.TypeOf(tc.obj)], "%T uncached", tc.obj)
	}
}

// cleanupPVForPVC reads the PVC through the reconciler's client, which
// ClientOptions keeps uncached for PVCs, so it needs no separate APIReader.
func TestCleanupPVForPVCReadsPVCThroughClient(t *testing.T) {
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, pvc).Build()

	r := &WorkflowReconciler{Client: c, Scheme: scheme}
	require.True(t, r.cleanupPVForPVC(ctx, workflow, testNS, pvcName))

	got := &corev1.PersistentVolume{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: pvName}, got))
	require.Equal(t, corev1.PersistentVolumeReclaimDelete, got.Spec.PersistentVolumeReclaimPolicy)
}
