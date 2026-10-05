// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podutil provides pod discovery and status helpers for Kubernetes workloads.
package podutil

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkerDiscoverer finds worker pods for training workloads.
type WorkerDiscoverer struct {
	client client.Reader
}

// NewWorkerDiscoverer creates a new WorkerDiscoverer.
func NewWorkerDiscoverer(c client.Reader) *WorkerDiscoverer {
	return &WorkerDiscoverer{client: c}
}

// GetWorkerPods returns the worker-0 and optionally the last worker pod.
// workloadKind is one of: "TrainJob".
// The lastWorker may be nil if the workload is single-worker or replica count is 1.
func (d *WorkerDiscoverer) GetWorkerPods(ctx context.Context, namespace, name, workloadKind string) (worker0 *corev1.Pod, lastWorker *corev1.Pod, err error) {
	switch workloadKind {
	case "TrainJob":
		return d.getTrainJobWorkerPods(ctx, namespace, name)
	default:
		return nil, nil, fmt.Errorf("unsupported workload kind: %s", workloadKind)
	}
}

// GetReplicatedJobPod returns the first pod of a replicatedJob within a TrainJob's JobSet, in the
// order defined by replicatedJobPodLess, so repeated calls pick the same pod.
// replicatedJobName is "launcher" for MPI workloads or "node" for torch workloads.
func (d *WorkerDiscoverer) GetReplicatedJobPod(ctx context.Context, namespace, workloadName, replicatedJobName string) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := d.client.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
		"jobset.sigs.k8s.io/jobset-name":        workloadName,
		"jobset.sigs.k8s.io/replicatedjob-name": replicatedJobName,
	}); err != nil {
		return nil, fmt.Errorf("listing pods for %s/%s replicatedJob %s: %w", namespace, workloadName, replicatedJobName, err)
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for %s/%s replicatedJob %s", namespace, workloadName, replicatedJobName)
	}

	sort.SliceStable(podList.Items, func(i, j int) bool {
		return replicatedJobPodLess(&podList.Items[i], &podList.Items[j])
	})

	return &podList.Items[0], nil
}

// replicatedJobPodLess orders the pods of one replicatedJob so the first is the
// same pod on every call: lowest completion index, then the current attempt
// over a replaced one. A pod the Job recreated after a failure shares its
// completion index with the failed pod, so the index alone is ambiguous.
func replicatedJobPodLess(a, b *corev1.Pod) bool {
	if ia, ib := getCompletionIndex(a), getCompletionIndex(b); ia != ib {
		return ia < ib
	}
	if ra, rb := podPhaseRank(a.Status.Phase), podPhaseRank(b.Status.Phase); ra != rb {
		return ra < rb
	}
	if ra, rb := labelInt(a, jobSetRestartAttemptLabel), labelInt(b, jobSetRestartAttemptLabel); ra != rb {
		return ra > rb
	}
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name < b.Name
}

// jobSetRestartAttemptLabel is the JobSet restart counter; a full JobSet
// restart recreates every pod under a higher attempt.
const jobSetRestartAttemptLabel = "jobset.sigs.k8s.io/restart-attempt"

// podPhaseRank prefers a pod that finished cleanly, then one still running,
// then one that failed.
func podPhaseRank(phase corev1.PodPhase) int {
	switch phase {
	case corev1.PodSucceeded:
		return 0
	case corev1.PodRunning:
		return 1
	case corev1.PodFailed:
		return 2
	default:
		return 3
	}
}

func labelInt(pod *corev1.Pod, key string) int {
	v, err := strconv.Atoi(pod.Labels[key])
	if err != nil {
		return 0
	}
	return v
}

// getTrainJobWorkerPods discovers worker pods for a Kubeflow TrainJob.
// TrainJob creates a JobSet, which creates batch Jobs, which create pods.
// Workers are identified by the batch.kubernetes.io/job-completion-index label.
func (d *WorkerDiscoverer) getTrainJobWorkerPods(ctx context.Context, namespace, name string) (*corev1.Pod, *corev1.Pod, error) {
	// List pods with the JobSet label matching the TrainJob name.
	podList := &corev1.PodList{}
	if err := d.client.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
		"jobset.sigs.k8s.io/jobset-name": name,
	}); err != nil {
		return nil, nil, fmt.Errorf("listing pods for TrainJob %s: %w", name, err)
	}

	if len(podList.Items) == 0 {
		return nil, nil, fmt.Errorf("no pods found for TrainJob %s", name)
	}

	// Sort pods by their completion index.
	sort.Slice(podList.Items, func(i, j int) bool {
		idxI := getCompletionIndex(&podList.Items[i])
		idxJ := getCompletionIndex(&podList.Items[j])
		return idxI < idxJ
	})

	worker0 := &podList.Items[0]
	if len(podList.Items) == 1 {
		return worker0, nil, nil
	}

	lastWorker := &podList.Items[len(podList.Items)-1]
	return worker0, lastWorker, nil
}

// getCompletionIndex extracts the batch job completion index from a pod's labels.
func getCompletionIndex(pod *corev1.Pod) int {
	idxStr, ok := pod.Labels["batch.kubernetes.io/job-completion-index"]
	if !ok {
		return 0
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return 0
	}
	return idx
}

// IsPodRunning checks if a pod is in the Running phase.
func IsPodRunning(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodRunning
}

// ContainerRestartStatus returns the restart count and whether the named
// container is in a waiting state (e.g., CrashLoopBackOff).
func ContainerRestartStatus(pod *corev1.Pod, name string) (restarts int32, waiting bool) {
	for i := range pod.Status.ContainerStatuses {
		if cs := &pod.Status.ContainerStatuses[i]; cs.Name == name {
			return cs.RestartCount, cs.State.Waiting != nil
		}
	}
	return 0, false
}

// GetContainerTerminationTime returns when a container terminated.
// Returns nil if the container has not terminated or is not found.
func GetContainerTerminationTime(pod *corev1.Pod, containerName string) *metav1.Time {
	if pod == nil {
		return nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Terminated != nil {
			return &cs.State.Terminated.FinishedAt
		}
		if cs.LastTerminationState.Terminated != nil {
			return &cs.LastTerminationState.Terminated.FinishedAt
		}
	}
	return nil
}
