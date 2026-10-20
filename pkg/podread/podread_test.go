// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podread

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testNamespace = "ns"
	testJob       = "job-1"
	// testNewWorker is the running pod a lagging cache misses.
	testNewWorker = "worker-new"
)

// pod returns a pod in testNamespace with the given labels, bound to node.
func pod(name, node string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		Name: name, Namespace: testNamespace, Labels: labels,
		Spec: corev1.PodSpec{NodeName: node},
	}
}

// indexedClient is a fake client holding pods, with the two Pod field indexes
// the manager registers.
func indexedClient(pods ...*corev1.Pod) client.Client {
	objs := make([]client.Object, len(pods))
	for i := range pods {
		objs[i] = pods[i]
	}
	return fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, JobIndexField, func(o client.Object) []string {
			if v, ok := o.GetLabels()[JobLabel]; ok {
				return []string{v}
			}
			return nil
		}).
		WithIndex(&corev1.Pod{}, NodeNameIndexField, func(o client.Object) []string {
			if n := o.(*corev1.Pod).Spec.NodeName; n != "" {
				return []string{n}
			}
			return nil
		}).
		Build()
}

// names returns the pods' names, sorted.
func names(pods []corev1.Pod) []string {
	out := make([]string, 0, len(pods))
	for i := range pods {
		out = append(out, pods[i].Name)
	}
	slices.Sort(out)
	return out
}

// The cached and live fakes hold different pods, so each read shows which
// source answered it.
var (
	worker   = pod("worker-0", "node-a", map[string]string{JobLabel: testJob, jobSetNameLabel: "w"})
	other    = pod("other-job-0", "node-a", map[string]string{JobLabel: "job-2"})
	launcher = pod("launcher-0", "node-b", map[string]string{jobSetNameLabel: "w", replicatedJobNameLabel: "launcher"})
	daemon   = pod("daemon-0", "node-a", map[string]string{"app": "daemon"})
	liveOnly = pod("worker-1", "node-c", map[string]string{JobLabel: testJob, jobSetNameLabel: "w"})
)

func TestReaderSources(t *testing.T) {
	ctx := context.Background()
	r := New(
		indexedClient(worker, other),
		indexedClient(worker, liveOnly, launcher, daemon),
	)

	tests := []struct {
		name string
		read func() ([]corev1.Pod, error)
		want []string
	}{
		{"JobPods reads the cache", func() ([]corev1.Pod, error) {
			return r.JobPods(ctx, testNamespace, testJob)
		}, []string{worker.Name}},
		{"JobPodsOnNode reads the cache, job pods only", func() ([]corev1.Pod, error) {
			return r.JobPodsOnNode(ctx, "node-a")
		}, []string{other.Name, worker.Name}},
		{"WorkloadPods reads the API server, launcher included", func() ([]corev1.Pod, error) {
			return r.WorkloadPods(ctx, testNamespace, "w", "")
		}, []string{launcher.Name, worker.Name, liveOnly.Name}},
		{"WorkloadPods narrows to a replicatedJob", func() ([]corev1.Pod, error) {
			return r.WorkloadPods(ctx, testNamespace, "w", "launcher")
		}, []string{launcher.Name}},
		{"NamespacePods reads the API server", func() ([]corev1.Pod, error) {
			return r.NamespacePods(ctx, testNamespace)
		}, []string{daemon.Name, launcher.Name, worker.Name, liveOnly.Name}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods, err := tt.read()
			require.NoError(t, err)
			assert.Equal(t, tt.want, names(pods))
		})
	}
}

// withPhase returns a copy of p in the given phase.
func withPhase(p *corev1.Pod, phase corev1.PodPhase) *corev1.Pod {
	c := p.DeepCopy()
	c.Status.Phase = phase
	return c
}

// countingClient counts the Lists that reach it.
func countingClient(lists *int, pods ...*corev1.Pod) client.Client {
	inner := indexedClient(pods...)
	return interceptor.NewClient(inner.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			*lists++
			return c.List(ctx, list, opts...)
		},
	})
}

// TestActiveJobPods pins when the drain barrier's read reaches the API
// server: whenever the cache leaves no active pod, including when the cache
// holds only finished pods while a newer pod is still running (ADR-102).
func TestActiveJobPods(t *testing.T) {
	failedOld := withPhase(pod("worker-old", "node-a", map[string]string{JobLabel: testJob}), corev1.PodFailed)
	running := withPhase(pod(testNewWorker, "node-a", map[string]string{JobLabel: testJob}), corev1.PodRunning)
	succeeded := withPhase(running, corev1.PodSucceeded)

	tests := []struct {
		name      string
		cached    []*corev1.Pod
		live      []*corev1.Pod
		want      []string
		liveReads int
	}{
		{"a finished pod in the cache does not hide a running one",
			[]*corev1.Pod{failedOld}, []*corev1.Pod{failedOld, running}, []string{testNewWorker}, 1},
		{"an empty cache does not hide a running pod",
			nil, []*corev1.Pod{running}, []string{testNewWorker}, 1},
		{"an active cached pod needs no API server read",
			[]*corev1.Pod{running}, []*corev1.Pod{running}, []string{testNewWorker}, 0},
		{"finished pods everywhere are none active, confirmed",
			[]*corev1.Pod{failedOld}, []*corev1.Pod{failedOld, succeeded}, []string{}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			liveReads := 0
			r := New(indexedClient(tt.cached...), countingClient(&liveReads, tt.live...))
			pods, err := r.ActiveJobPods(context.Background(), testNamespace, testJob)
			require.NoError(t, err)
			assert.Equal(t, tt.want, names(pods))
			assert.Equal(t, tt.liveReads, liveReads)
		})
	}
}

func TestJobPodsFallsBackToTheLabel(t *testing.T) {
	// A fake client without the job index fails the indexed read.
	noIndex := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(worker, other).Build()
	pods, err := New(noIndex, noIndex).JobPods(context.Background(), testNamespace, testJob)
	require.NoError(t, err)
	assert.Equal(t, []string{worker.Name}, names(pods))
}

func TestReadErrorsAreWrappedAndCounted(t *testing.T) {
	failing := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errors.New("etcdserver: request timed out")
		},
	}).Build()
	before := testutil.ToFloat64(podReadsTotal.WithLabelValues("WorkloadPods", sourceLive, resultError))

	pods, err := New(failing, failing).WorkloadPods(context.Background(), testNamespace, "w", "launcher")
	assert.Nil(t, pods)
	require.Error(t, err)
	assert.Equal(t, "podread WorkloadPods ns/w/launcher: etcdserver: request timed out", err.Error())
	assert.Equal(t, before+1, testutil.ToFloat64(podReadsTotal.WithLabelValues("WorkloadPods", sourceLive, resultError)))
}

func TestEmptyIsNotAnError(t *testing.T) {
	before := testutil.ToFloat64(podReadsTotal.WithLabelValues("NamespacePods", sourceLive, resultEmpty))
	pods, err := New(indexedClient(), indexedClient()).NamespacePods(context.Background(), "kube-system")
	require.NoError(t, err)
	assert.Empty(t, pods)
	assert.Equal(t, before+1, testutil.ToFloat64(podReadsTotal.WithLabelValues("NamespacePods", sourceLive, resultEmpty)))
}
