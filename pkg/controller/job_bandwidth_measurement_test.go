// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	bandwidthLabelTestJob      = "job-bandwidth-label"
	bandwidthLabelTestWorkflow = "workflow-bandwidth-label"
	bandwidthLabelTestProfile  = "nccl-bandwidth-label-test"
)

func TestEnsureBandwidthMeasurementCopiesWorkflowLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	job := &nvcrev1alpha1.Job{
		Name:      bandwidthLabelTestJob,
		Namespace: testNS,
		Labels: map[string]string{
			labelWorkflowTracking: bandwidthLabelTestWorkflow,
		},
		Spec: nvcrev1alpha1.JobSpec{
			BandwidthMeasurement: &nvcrev1alpha1.BandwidthMeasurementConfig{
				LogProfileRef: bandwidthLabelTestProfile,
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.ensureBandwidthMeasurement(context.Background(), job))

	measurement := &nvcrev1alpha1.BandwidthMeasurement{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: job.Namespace, Name: bandwidthLabelTestJob + "-bandwidth"}, measurement))
	require.Equal(t, bandwidthLabelTestWorkflow, measurement.Labels[labelWorkflowTracking])
}
