// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat_mokka

package mokka

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
)

// TestNCCLAllReducePass runs the nccl-all-reduce category end to end: real
// scheduling and allocation, a real MPI launch across two nodes, and the
// replayed log read back through the kubelet into a BandwidthMeasurement.
func TestNCCLAllReducePass(t *testing.T) {
	requireProfile(t, "h100", "b200")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "nccl-pass.yaml")
	cert := waitTerminal(ctx, t, ns, name)
	requireCondition(t, cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded, controller.ReasonAllWorkflowsSucceeded)

	bwm := onlyBandwidthMeasurement(ctx, t, ns)
	requireCondition(t, bwm.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete, "JobSucceeded")
	require.Len(t, bwm.Status.Results, replayedRows(t), "one result per replayed row")
	require.NotEmpty(t, bwm.Status.Transport, "transport parsed from the replayed NCCL INFO lines")
}

// TestNCCLAllReduceWorkloadFailed proves a workload that exits non-zero fails
// the Job with WorkloadFailed, promptly (see workloadFailureTimeout), and that
// status.failureLog carries the launcher's own output rather than a worker's
// sshd exit while the JobSet tears the workers down (issue #498). The KWOK
// lane cannot reach this path: its pods always succeed.
func TestNCCLAllReduceWorkloadFailed(t *testing.T) {
	requireProfile(t, "h100")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "nccl-workload-failed.yaml")
	cert := waitTerminalWithin(ctx, t, ns, name, workloadFailureTimeout)
	require.True(t, meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed),
		"Certification should fail; conditions: %s", describeConditions(cert.Status.Conditions))

	job := onlyJob(ctx, t, ns)
	requireCondition(t, job.Status.Conditions, nvcrev1alpha1.JobFailed, controller.ReasonWorkloadFailed)
	require.Len(t, job.Status.FailedNodes, 2, "both workers attributed")
	for _, node := range job.Status.FailedNodes {
		require.Equal(t, nvcrev1alpha1.NodeFailureWorkloadFailed, node.Reason, "failed node %s", node.Name)
	}

	fl := job.Status.FailureLog
	require.NotNil(t, fl, "failed Job should carry status.failureLog")
	require.Contains(t, fl.PodName, "-launcher-", "failureLog should name the launcher pod; got %+v", *fl)
	require.Contains(t, fl.Tail, "[stub] simulated failure",
		"failureLog should carry the launcher's mpirun output; got %+v", *fl)
}

// TestNCCLAllReduceThreshold proves a measured value below the configured
// minimum fails validation and attributes the nodes to ThresholdViolation.
func TestNCCLAllReduceThreshold(t *testing.T) {
	requireProfile(t, "h100")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "nccl-threshold.yaml")
	cert := waitTerminal(ctx, t, ns, name)
	require.True(t, meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed),
		"Certification should fail; conditions: %s", describeConditions(cert.Status.Conditions))

	job := onlyJob(ctx, t, ns)
	require.True(t, meta.IsStatusConditionTrue(job.Status.Conditions, nvcrev1alpha1.JobValidationFailed),
		"Job should carry ValidationFailed; conditions: %s", describeConditions(job.Status.Conditions))
	require.NotEmpty(t, job.Status.FailedNodes)
	for _, node := range job.Status.FailedNodes {
		require.Equal(t, nvcrev1alpha1.NodeFailureThresholdViolation, node.Reason, "failed node %s: %s", node.Name, node.Message)
	}
}

// TestNCCLAllReduceNoData proves a run that prints no result rows yields
// NoDataCollected, and that missing data fails a threshold, which any measured
// value would have passed, with MeasurementTimeout rather than passing it.
func TestNCCLAllReduceNoData(t *testing.T) {
	requireProfile(t, "h100")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "nccl-no-data.yaml")
	cert := waitTerminal(ctx, t, ns, name)

	bwm := onlyBandwidthMeasurement(ctx, t, ns)
	requireCondition(t, bwm.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete, "NoDataCollected")
	require.Empty(t, bwm.Status.Results)

	require.True(t, meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed),
		"missing data must not pass a threshold; conditions: %s", describeConditions(cert.Status.Conditions))
	job := onlyJob(ctx, t, ns)
	requireCondition(t, job.Status.Conditions, nvcrev1alpha1.JobValidationFailed, "MeasurementTimeout")
}
