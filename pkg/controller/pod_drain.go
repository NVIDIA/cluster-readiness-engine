// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
)

// podDrainGracePeriod bounds the pod-drain barrier: how long dependency
// cleanup (Workflow tier) and finalizer removal (Job tier) wait for a deleted
// workload's pods to disappear before proceeding anyway. Without the bound, a
// pod stuck Terminating (e.g. an unreachable kubelet) would wedge the Workflow
// forever; with it, the worst case for such a pod is the pre-barrier behavior
// (its processes may fail with CUDA error 719 when the DRA-backed
// ComputeDomain is deleted underneath them), after a logged warning.
//
// The grace period is measured from persisted timestamps only — the Job's
// terminal condition transition times and its deletionTimestamp (see
// drainStart) — so it survives controller restarts.
const podDrainGracePeriod = 5 * time.Minute

// activeWorkloadPods returns the names of the Job's workload pods that may
// still be running GPU processes: every pod carrying the nvcre.nvidia.com/job
// label whose phase is not terminal. A Terminating pod stays in the Running
// phase until its containers exit, and its DRA allocations (e.g. the MNNVL
// ComputeDomain channel) are held until the pod is gone — exactly the pods
// the drain barrier must wait for. Pods in Succeeded/Failed phase have
// exited; they hold no running CUDA context, so they do not block cleanup.
//
// MPI launcher pods do not carry the label (see CacheOptions), but they also
// hold no GPU or ComputeDomain claims, so the label is the right selector
// here: the barrier protects DRA allocations, all of which belong to worker
// pods.
func activeWorkloadPods(ctx context.Context, c client.Reader, namespace, jobName string) ([]string, error) {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingFields{nodemonitor.PodNVCREJobIndexField: jobName},
	); err != nil {
		return nil, fmt.Errorf("failed to list workload pods for Job %s: %w", jobName, err)
	}
	var active []string
	for i := range podList.Items {
		switch podList.Items[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			// Terminal pods have exited and cannot fault on a revoked DRA allocation.
		default:
			active = append(active, podList.Items[i].Name)
		}
	}
	return active, nil
}

// drainStart returns the persisted instant the workload teardown began: the
// latest of the Job's true terminal condition transition times and its
// deletionTimestamp. Every path that deletes the workload does so on the
// reconcile that observes one of these (the timeout path sets JobFailed in
// the same reconcile), so this is the drain anchor for the grace period
// without any in-memory state. Returns nil when the Job carries none of them,
// which cannot happen on the barrier's call paths.
//
// Accepted tradeoff: the anchor predates the workload delete when the
// controller only observes a terminal condition after a long outage. If that
// observation lag exceeds podDrainGracePeriod, the grace period is already
// expired on the first barrier check and cleanup proceeds under still-running
// pods (the pre-barrier behavior). Closing this would require persisting a
// "workload deleted at" marker (status field or annotation write) on every
// terminal path; the >5-minute-outage window is rare enough that the API
// churn is not worth it.
func drainStart(job *nvcrev1alpha1.Job) *metav1.Time {
	var start *metav1.Time
	consider := func(t *metav1.Time) {
		if t == nil || t.IsZero() {
			return
		}
		if start == nil || start.Before(t) {
			start = t
		}
	}
	for _, condType := range []string{
		nvcrev1alpha1.JobFailed,
		nvcrev1alpha1.JobHardwareFailed,
		nvcrev1alpha1.JobSucceeded,
	} {
		if c := meta.FindStatusCondition(job.Status.Conditions, condType); c != nil && c.Status == metav1.ConditionTrue {
			t := c.LastTransitionTime
			consider(&t)
		}
	}
	consider(job.DeletionTimestamp)
	return start
}

// shouldWaitForPodDrain is the pod-drain barrier (issue #121). Deleting a
// workload (TrainJob) is asynchronous: the object goes away immediately while
// its pods keep running (Terminating) for their termination grace period. The
// scoped dependency resources — ComputeDomain, ResourceClaimTemplate — provide
// DRA allocations to those pods, and deleting them while pods still run kills
// every pod process with CUDA error 719, producing misleading failure
// artifacts in the certification report.
//
// Callers that are about to revoke those allocations (scoped-dependency
// cleanup on the Workflow tier, finalizer removal on the Job tier) must defer
// and requeue while this returns true. It returns false — proceed — when the
// workload's pods are gone, and also once podDrainGracePeriod has elapsed
// since drainStart, so a pod stuck Terminating cannot wedge the Workflow
// forever.
func shouldWaitForPodDrain(ctx context.Context, c client.Reader, job *nvcrev1alpha1.Job) bool {
	log := logf.FromContext(ctx)

	graceExpired := false
	if start := drainStart(job); start != nil && time.Since(start.Time) > podDrainGracePeriod {
		graceExpired = true
	}

	active, err := activeWorkloadPods(ctx, c, job.Namespace, job.Name)
	if err != nil {
		if graceExpired {
			log.Error(err, "Pod drain check failed after the grace period; proceeding without the drain barrier",
				"job", job.Name)
			return false
		}
		// Fail closed: retry on the next reconcile. The grace period above
		// bounds this, so a persistent list error cannot wedge the caller.
		log.Error(err, "Pod drain check failed; deferring cleanup until workload pods are confirmed gone",
			"job", job.Name)
		return true
	}
	if len(active) == 0 {
		return false
	}
	if graceExpired {
		log.Info("Warning: workload pods still present after the drain grace period; proceeding with cleanup anyway"+
			" — remaining pod processes may fail with CUDA error 719",
			"job", job.Name, "gracePeriod", podDrainGracePeriod, "activePods", active)
		return false
	}
	log.Info("Workload pods still terminating; deferring cleanup until they are gone",
		"job", job.Name, "activePods", active)
	return true
}
