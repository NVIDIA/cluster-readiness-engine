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
	client client.Client
}

// NewWorkerDiscoverer creates a new WorkerDiscoverer.
func NewWorkerDiscoverer(c client.Client) *WorkerDiscoverer {
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

// GetReplicatedJobPod finds the first running pod for a specific replicatedJob within a TrainJob's JobSet.
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

	// Sort by completion index and return the first one.
	sort.Slice(podList.Items, func(i, j int) bool {
		return getCompletionIndex(&podList.Items[i]) < getCompletionIndex(&podList.Items[j])
	})

	return &podList.Items[0], nil
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

// GetPodStartTime returns the pod's start time. If the pod has not started,
// a zero-value metav1.Time is returned.
func GetPodStartTime(pod *corev1.Pod) metav1.Time {
	if pod == nil || pod.Status.StartTime == nil {
		return metav1.Time{}
	}
	return *pod.Status.StartTime
}

// GetTerminationReason returns the reason for pod termination for the given container.
// It checks terminated container statuses first, then last terminated state.
// Returns an empty string if no termination reason is found.
func GetTerminationReason(pod *corev1.Pod, containerName string) string {
	if pod == nil {
		return ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Terminated != nil {
			return cs.State.Terminated.Reason
		}
		if cs.LastTerminationState.Terminated != nil {
			return cs.LastTerminationState.Terminated.Reason
		}
	}
	return ""
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
