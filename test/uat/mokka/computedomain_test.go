// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat_mokka

package mokka

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/test/uat/util"
)

// drainDeferralLog is what shouldWaitForPodDrain (pkg/controller/pod_drain.go)
// logs while it holds scoped-dependency cleanup for terminating pods.
const drainDeferralLog = "Workload pods still terminating; deferring cleanup until they are gone"

// TestComputeDomainPass runs nccl-all-reduce on an NVL72 profile, where the
// catalog adds a ComputeDomain. The real DRA driver and IMEX daemons must make
// it Ready, and NVCRE must remove every DRA object once the Certification
// succeeds. The KWOK lane's DRA stub allocates claims instantly and has no
// ComputeDomain status, so it can check neither.
//
// Workers starting before the domain is ready is not asserted from the API.
// The driver's kubelet plugin enforces it: it refuses to prepare a worker's
// channel claim ("current node not ready in ComputeDomain") until IMEX is up
// on that node. The ComputeDomain's own status is written separately and was
// measured lagging that by about 1.5s, so a worker can be Running while the
// API still says NotReady. That skew is logged, not failed.
//
// On success NVCRE deletes the ComputeDomain without the pod-drain barrier,
// while the idle workers are still running (ADR-096, Notes). That is logged,
// not failed: the barrier covers the failure and deletion paths only.
func TestComputeDomainPass(t *testing.T) {
	requireProfile(t, "gb200", "gb300")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "computedomain-pass.yaml")

	// Sample until the Certification finishes, recording workers seen running
	// before the API showed the domain Ready, and workers that outlived it.
	var sawReady, sawRunning bool
	runningBeforeReady := map[string]bool{}
	outlived := map[string]bool{}
	cert := &nvcrev1alpha1.Certification{}
	pollUntil(t, certTimeout, 500*time.Millisecond, func() bool {
		cds := computeDomains(ctx, t, ns)
		ready := len(cds) > 0
		for i := range cds {
			ready = ready && computeDomainReady(&cds[i])
		}
		sawReady = sawReady || ready
		for _, pod := range claimPods(ctx, t, ns) {
			if !anyContainerRunning(&pod) {
				continue
			}
			sawRunning = true
			switch {
			case len(cds) == 0 && sawReady:
				outlived[pod.Name] = true
			case !ready && !sawReady:
				runningBeforeReady[pod.Name] = true
			}
		}
		if err := c.Get(ctx, util.CertificationKey(name, ns), cert); err != nil {
			return false
		}
		return meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded) ||
			meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	}, func() string { return fmt.Sprintf("Certification %s/%s never finished", ns, name) })

	requireCondition(t, cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded, controller.ReasonAllWorkflowsSucceeded)
	require.True(t, sawReady, "the ComputeDomain was never observed Ready")
	require.True(t, sawRunning, "no worker holding a ResourceClaim was observed running")
	if len(runningBeforeReady) > 0 {
		t.Logf("start-up: %d worker(s) running before the ComputeDomain status showed Ready (status lags the driver's own readiness gate)", len(runningBeforeReady))
	}
	if len(outlived) > 0 {
		t.Logf("success path: %d worker(s) still running after the ComputeDomain was deleted (no drain barrier on success)", len(outlived))
	}
	requireNoDRALeftovers(ctx, t, ns)
}

// TestComputeDomainDeleteMidRun deletes a running Certification and checks the
// pod-drain barrier (#235): the ComputeDomain must outlive every worker that
// is still terminating, because tearing it down first strands the workers'
// IMEX channels. envtest has no kubelet and KWOK pods have no termination
// grace period, so this ordering exists only here.
func TestComputeDomainDeleteMidRun(t *testing.T) {
	requireProfile(t, "gb200")
	ctx := context.Background()
	ns := testNamespace(ctx, t)

	name := createCertification(ctx, t, ns, "computedomain-delete.yaml")
	pollUntil(t, certTimeout, pollInterval, func() bool {
		for _, pod := range claimPods(ctx, t, ns) {
			if anyContainerRunning(&pod) {
				return true
			}
		}
		return false
	}, func() string { return fmt.Sprintf("no worker started in %s", ns) })
	require.NotEmpty(t, computeDomains(ctx, t, ns))

	deletedAt := time.Now()
	util.DeleteCertification(ctx, c, name, ns)
	t.Logf("deleted Certification %s/%s while workers were running", ns, name)

	// Until the last worker is gone, the ComputeDomain must still exist.
	pollUntil(t, cleanupTimeout, 500*time.Millisecond, func() bool {
		live := 0
		for _, pod := range claimPods(ctx, t, ns) {
			if !podTerminal(&pod) {
				live++
			}
		}
		if live == 0 {
			return true
		}
		require.NotEmpty(t, computeDomains(ctx, t, ns),
			"ComputeDomain deleted while %d worker pods were still not terminal", live)
		return false
	}, func() string { return fmt.Sprintf("workers never terminated in %s", ns) })

	requireNoDRALeftovers(ctx, t, ns)
	logs := controllerLogs(ctx, t, deletedAt)
	require.True(t, strings.Contains(logs, drainDeferralLog),
		"controller never logged the drain deferral after the delete")
}
