// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podread is the only place controller code reads Pods (ADR-102).
//
// The manager's cache holds only Pods that carry the NVCRE job label. A Pod
// read therefore has to know whether every pod it needs carries that label,
// and the answer belongs to what is being read, not to each caller. Each
// method here names what it reads and picks its source: the cache, under a
// selector the method builds itself, or the API server. A new kind of read is
// a new method, where its source is decided and reviewed.
package podread

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// JobLabel is injected into the pod template of an NVCRE Job's workers and
	// names the Job. The manager's cache holds only Pods that carry it.
	JobLabel = "nvcre.nvidia.com/job"

	// JobIndexField indexes cached Pods by JobLabel's value.
	JobIndexField = "metadata.labels." + JobLabel

	// NodeNameIndexField indexes cached Pods by spec.nodeName.
	NodeNameIndexField = "spec.nodeName"

	// jobSetNameLabel and replicatedJobNameLabel are set by JobSet on every
	// pod of a workload, launchers included.
	jobSetNameLabel        = "jobset.sigs.k8s.io/jobset-name"
	replicatedJobNameLabel = "jobset.sigs.k8s.io/replicatedjob-name"
)

// Read sources and results, as recorded in nvcre_pod_reads_total.
const (
	sourceCache = "cache"
	sourceLive  = "live"

	resultFound = "found"
	resultEmpty = "empty"
	resultError = "error"
)

// podReadsTotal counts every read by method, the source that answered it, and
// whether it found pods, found none, or failed.
var podReadsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "nvcre_pod_reads_total",
		Help: "Pod reads by the NVCRE controller, by read, the source that answered (cache or live), and result (found, empty or error).",
	},
	[]string{"read", "source", "result"},
)

func init() {
	metrics.Registry.MustRegister(podReadsTotal)
}

// Reader reads Pods from the manager's job-scoped cache or from the API
// server, whichever the read needs.
type Reader struct {
	cached client.Reader
	live   client.Reader
}

// New returns a Reader. cached is the manager's client, which reads through
// the job-scoped cache; live reads the API server, normally the manager's
// APIReader. A unit test may pass the same fake client for both.
func New(cached, live client.Reader) *Reader {
	return &Reader{cached: cached, live: live}
}

// JobPods returns a Job's workload pods from the cache: the pods carrying
// JobLabel=job in namespace. No pods is an empty slice and a nil error.
func (r *Reader) JobPods(ctx context.Context, namespace, job string) ([]corev1.Pod, error) {
	pods, err := r.cachedJobPods(ctx, namespace, job)
	return finish(ctx, "JobPods", sourceCache, namespace+"/"+job, pods, err)
}

// ActiveJobPods returns a Job's workload pods that have not finished: the
// pods carrying JobLabel=job in namespace whose phase is neither Succeeded
// nor Failed. It reads the cache, and when no active pod remains there it
// reads the API server before returning none.
//
// A caller acts on "none active": the drain barrier lets teardown proceed.
// The cache can hold a Job's finished pods and miss a newer one that is still
// running, so the API server is checked whenever filtering leaves no active
// pod, not only when the cached list is empty. The check selects on the job
// label, so it sees what the cache would hold once it catches up.
func (r *Reader) ActiveJobPods(ctx context.Context, namespace, job string) ([]corev1.Pod, error) {
	const read = "ActiveJobPods"
	target := namespace + "/" + job
	pods, err := r.cachedJobPods(ctx, namespace, job)
	if err != nil {
		return finish(ctx, read, sourceCache, target, nil, err)
	}
	if active := activeOnly(pods); len(active) > 0 {
		return finish(ctx, read, sourceCache, target, active, nil)
	}
	pods, err = list(ctx, r.live, client.InNamespace(namespace), client.MatchingLabels{JobLabel: job})
	return finish(ctx, read, sourceLive, target, activeOnly(pods), err)
}

// cachedJobPods reads a Job's pods from the cache through the job field
// index, falling back to the label when the index read fails, as a fake
// client without the index does.
func (r *Reader) cachedJobPods(ctx context.Context, namespace, job string) ([]corev1.Pod, error) {
	pods, err := list(ctx, r.cached, client.InNamespace(namespace), client.MatchingFields{JobIndexField: job})
	if err != nil {
		pods, err = list(ctx, r.cached, client.InNamespace(namespace), client.MatchingLabels{JobLabel: job})
	}
	return pods, err
}

// activeOnly returns the pods whose phase is neither Succeeded nor Failed. A
// pod being deleted stays Running until its containers exit, so it counts.
func activeOnly(pods []corev1.Pod) []corev1.Pod {
	var active []corev1.Pod
	for i := range pods {
		switch pods[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
		default:
			active = append(active, pods[i])
		}
	}
	return active
}

// JobPodsOnNode returns the NVCRE workload pods bound to node, from the
// cache, through the spec.nodeName index and JobLabel. Pods of other
// components on the node are not returned.
func (r *Reader) JobPodsOnNode(ctx context.Context, node string) ([]corev1.Pod, error) {
	pods, err := list(ctx, r.cached, client.MatchingFields{NodeNameIndexField: node}, client.HasLabels{JobLabel})
	return finish(ctx, "JobPodsOnNode", sourceCache, node, pods, err)
}

// WorkloadPods returns a workload's pods from the API server: every pod JobSet
// labelled with the workload's name, in one replicatedJob when replicatedJob
// is not empty. Launchers carry no JobLabel, so the cache cannot hold them.
func (r *Reader) WorkloadPods(ctx context.Context, namespace, workload, replicatedJob string) ([]corev1.Pod, error) {
	selector := client.MatchingLabels{jobSetNameLabel: workload}
	target := namespace + "/" + workload
	if replicatedJob != "" {
		selector[replicatedJobNameLabel] = replicatedJob
		target += "/" + replicatedJob
	}
	pods, err := list(ctx, r.live, client.InNamespace(namespace), selector)
	return finish(ctx, "WorkloadPods", sourceLive, target, pods, err)
}

// NamespacePods returns every pod in namespace from the API server, for
// reads of other components' pods, such as installers in kube-system.
func (r *Reader) NamespacePods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	pods, err := list(ctx, r.live, client.InNamespace(namespace))
	return finish(ctx, "NamespacePods", sourceLive, namespace, pods, err)
}

func list(ctx context.Context, reader client.Reader, opts ...client.ListOption) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, opts...); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// finish counts the read and, for a failure, logs it at error level and wraps
// it with the method and target. A caller may still treat the error as "not
// found yet"; the log and the count keep the failure visible.
func finish(ctx context.Context, read, source, target string, pods []corev1.Pod, err error) ([]corev1.Pod, error) {
	switch {
	case err != nil:
		podReadsTotal.WithLabelValues(read, source, resultError).Inc()
		err = fmt.Errorf("podread %s %s: %w", read, target, err)
		if !errors.Is(err, context.Canceled) {
			logf.FromContext(ctx).Error(err, "Pod read failed", "read", read, "source", source)
		}
		return nil, err
	case len(pods) == 0:
		podReadsTotal.WithLabelValues(read, source, resultEmpty).Inc()
	default:
		podReadsTotal.WithLabelValues(read, source, resultFound).Inc()
	}
	return pods, nil
}
