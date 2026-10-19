// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// testManagerPod names the controller pod most cases use.
const testManagerPod = "nvcre-manager-a"

// testLimit is the memory limit the newer cases give the controller.
const testLimit = "1Gi"

// otherRelease is the instance label of a manager pod NVCRE does not own.
const otherRelease = "other"

// otherManagerPod names that pod.
const otherManagerPod = "other-manager"

// testStart is the fake clock's start; managerPod times are relative to it.
var testStart = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// managerPod describes one controller pod as one poll sees it.
type managerPod struct {
	name       string
	ready      bool
	startedAgo time.Duration // before testStart; zero means not running
	// readyAgo is when the pod last turned Ready, before testStart; zero
	// means when its container started. noReadyTime leaves the Ready
	// condition's transition time unset.
	readyAgo    time.Duration
	noReadyTime bool
	restarts    int32
	waiting     string
	lastReason  string
	lastExit    int32
	// exitReason and exitCode set a current termination: the container has
	// exited and the kubelet has not restarted it yet.
	exitReason  string
	exitCode    int32
	limit       string
	terminating bool
	// instance is the pod's app.kubernetes.io/instance; empty means nvcre.
	instance string
}

func (p managerPod) build() corev1.Pod {
	instance := p.instance
	if instance == "" {
		instance = helmReleaseName
	}
	pod := corev1.Pod{
		Name:      p.name,
		Namespace: nvcreNamespace,
		UID:       types.UID(p.name),
		Labels:    map[string]string{managerPodLabel: managerPodLabelValue, releaseInstanceLabel: instance},
		Spec:      corev1.PodSpec{Containers: []corev1.Container{{Name: managerContainerName}}},
	}
	if p.limit != "" {
		pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(p.limit),
		}
	}
	if p.terminating {
		pod.DeletionTimestamp = &metav1.Time{Time: testStart}
	}
	readyStatus := corev1.ConditionFalse
	if p.ready {
		readyStatus = corev1.ConditionTrue
	}
	readyCond := corev1.PodCondition{Type: corev1.PodReady, Status: readyStatus}
	if p.ready && !p.noReadyTime {
		readyAgo := p.readyAgo
		if readyAgo == 0 {
			readyAgo = p.startedAgo
		}
		readyCond.LastTransitionTime = metav1.Time{Time: testStart.Add(-readyAgo)}
	}
	pod.Status.Conditions = []corev1.PodCondition{readyCond}
	cs := corev1.ContainerStatus{Name: managerContainerName, Ready: p.ready, RestartCount: p.restarts}
	switch {
	case p.exitReason != "":
		cs.State.Terminated = &corev1.ContainerStateTerminated{Reason: p.exitReason, ExitCode: p.exitCode}
	case p.waiting != "":
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: p.waiting}
	case p.startedAgo > 0:
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: testStart.Add(-p.startedAgo)}}
	}
	if p.lastReason != "" {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: p.lastReason, ExitCode: p.lastExit}
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	return pod
}

// pollingClient answers the n-th pod List with the pods of polls[n] that
// match the List's label selector, repeating the last entry once they run
// out, and counts the Lists.
func pollingClient(t *testing.T, polls [][]managerPod, lists *int) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			podList, ok := list.(*corev1.PodList)
			if !ok {
				return errors.New("unexpected list type")
			}
			listOpts := (&client.ListOptions{}).ApplyOptions(opts)
			poll := polls[min(*lists, len(polls)-1)]
			*lists++
			podList.Items = nil
			for _, p := range poll {
				pod := p.build()
				if listOpts.LabelSelector == nil || listOpts.LabelSelector.Matches(labels.Set(pod.Labels)) {
					podList.Items = append(podList.Items, pod)
				}
			}
			return nil
		},
	}).Build()
}

// stalledClient answers every List only when its context ends, as a read
// against an API server that stopped responding would.
func stalledClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}).Build()
}

// fakeStability runs the check on a fake clock that each sleep advances.
func fakeStability(c client.Reader) managerStability {
	now := testStart
	m := newManagerStability(c)
	m.now = func() time.Time { return now }
	m.sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		return nil
	}
	return m
}

func TestManagerStabilityVerify(t *testing.T) {
	stable := managerPod{name: testManagerPod, ready: true, startedAgo: 2 * time.Hour}
	tests := []struct {
		name      string
		polls     [][]managerPod
		wantErr   string
		wantLists int
	}{
		{
			name: "a pod up for hours passes at once, whatever its old restarts",
			polls: [][]managerPod{{{name: testManagerPod, ready: true, startedAgo: 2 * time.Hour,
				restarts: 3, lastReason: reasonOOMKilled, lastExit: 137}}},
			wantLists: 1,
		},
		{
			name:  "a new pod passes once it has been up for a minute",
			polls: [][]managerPod{{{name: testManagerPod, ready: true, startedAgo: 10 * time.Second}}},
			// Up 10s at the first read; the read at +50s sees it up 60s.
			wantLists: 26,
		},
		{
			name: "a restart during the check fails with the memory hint",
			polls: [][]managerPod{
				{{name: testManagerPod, ready: true, startedAgo: 5 * time.Second, limit: "96Mi"}},
				{{name: testManagerPod, startedAgo: time.Second, restarts: 1,
					lastReason: reasonOOMKilled, lastExit: 137, limit: "96Mi"}},
			},
			wantErr: "the NVCRE controller is crash-looping: pod nvcre/nvcre-manager-a restarted 1 time, " +
				"last exit OOMKilled (exit code 137).\n" +
				"It ran out of memory at its 96Mi limit. The controller's memory grows with the cluster's size; " +
				"raise the limit, for example:\n  nvcrectl setup init --set manager.resources.limits.memory=192Mi",
			wantLists: 2,
		},
		{
			name: "an OOMKill with no limit asks for a request and a limit",
			polls: [][]managerPod{{{name: testManagerPod, waiting: reasonCrashLoopBackOff, restarts: 2,
				lastReason: reasonOOMKilled, lastExit: 137}}},
			wantErr: "nvcrectl setup init --set manager.resources.requests.memory=2Gi " +
				"--set manager.resources.limits.memory=4Gi",
			wantLists: 1,
		},
		{
			name: "CrashLoopBackOff for another reason points at the previous logs",
			polls: [][]managerPod{{{name: testManagerPod, waiting: reasonCrashLoopBackOff, restarts: 4,
				lastReason: reasonError, lastExit: 1}}},
			wantErr: "the NVCRE controller is crash-looping: pod nvcre/nvcre-manager-a restarted 4 times, " +
				"last exit Error (exit code 1).\n" +
				"Inspect its last run with: kubectl -n nvcre logs nvcre-manager-a -c manager --previous",
			wantLists: 1,
		},
		{
			name:  "a pod that never turns Ready times out",
			polls: [][]managerPod{{{name: testManagerPod, startedAgo: time.Hour}}},
			wantErr: "the NVCRE controller did not stay Ready for 1m0s within 3m0s: " +
				"nvcre-manager-a (running, not Ready)",
			wantLists: 91,
		},
		{
			name:      "no controller pod times out",
			polls:     [][]managerPod{{}},
			wantErr:   "no NVCRE controller pod appeared in namespace nvcre within 3m0s",
			wantLists: 91,
		},
		{
			name: "the terminating pod of the previous rollout is ignored",
			polls: [][]managerPod{{
				{name: "nvcre-manager-old", waiting: reasonCrashLoopBackOff, restarts: 9, terminating: true},
				stable,
			}},
			wantLists: 1,
		},
		{
			name: "every replica must be up, and a restart of either fails",
			polls: [][]managerPod{
				{stable, {name: "nvcre-manager-b", ready: true, startedAgo: 2 * time.Second}},
				{stable, {name: "nvcre-manager-b", startedAgo: time.Second, restarts: 1, lastReason: reasonError, lastExit: 2}},
			},
			wantErr:   "pod nvcre/nvcre-manager-b restarted 1 time, last exit Error (exit code 2)",
			wantLists: 2,
		},
		{
			name: "a container that exits during the check fails before the kubelet restarts it",
			polls: [][]managerPod{
				{{name: testManagerPod, ready: true, startedAgo: 5 * time.Second, limit: testLimit}},
				{{name: testManagerPod, exitReason: reasonOOMKilled, exitCode: 137, limit: testLimit}},
			},
			wantErr: "restarted 0 times, last exit OOMKilled (exit code 137).\n" +
				"It ran out of memory at its 1Gi limit.",
			wantLists: 2,
		},
		{
			name: "the current exit is reported, not the attempt before it",
			polls: [][]managerPod{
				{{name: testManagerPod, ready: true, startedAgo: 5 * time.Second, restarts: 1,
					lastReason: reasonError, lastExit: 1, limit: testLimit}},
				{{name: testManagerPod, restarts: 1, exitReason: reasonOOMKilled, exitCode: 137,
					lastReason: reasonError, lastExit: 1, limit: testLimit}},
			},
			wantErr: "restarted 1 time, last exit OOMKilled (exit code 137).\n" +
				"It ran out of memory at its 1Gi limit.",
			wantLists: 2,
		},
		{
			name:  "a pod running for hours that turned Ready seconds ago waits a minute from Ready",
			polls: [][]managerPod{{{name: testManagerPod, ready: true, startedAgo: 2 * time.Hour, readyAgo: 10 * time.Second}}},
			// Ready 10s at the first read; the read at +50s sees it Ready 60s.
			wantLists: 26,
		},
		{
			name:  "a Ready condition with no transition time never counts as settled",
			polls: [][]managerPod{{{name: testManagerPod, ready: true, startedAgo: time.Hour, noReadyTime: true}}},
			wantErr: "the NVCRE controller did not stay Ready for 1m0s within 3m0s: " +
				"nvcre-manager-a (running, not Ready)",
			wantLists: 91,
		},
		{
			name: "another release's manager pod in the namespace is ignored",
			polls: [][]managerPod{{
				{name: otherManagerPod, instance: otherRelease, waiting: reasonCrashLoopBackOff, restarts: 7},
				stable,
			}},
			wantLists: 1,
		},
		{
			name: "only another release's manager pod is no controller pod",
			polls: [][]managerPod{{
				{name: otherManagerPod, instance: otherRelease, ready: true, startedAgo: time.Hour},
			}},
			wantErr:   "no NVCRE controller pod appeared in namespace nvcre within 3m0s",
			wantLists: 91,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lists := 0
			err := fakeStability(pollingClient(t, tt.polls, &lists)).verify(context.Background())
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
			assert.Equal(t, tt.wantLists, lists)
		})
	}
}

func TestManagerStabilityVerifyStopsOnCancel(t *testing.T) {
	lists := 0
	m := fakeStability(pollingClient(t, [][]managerPod{{}}, &lists))
	m.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	err := m.verify(context.Background())
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, lists)
}

func TestManagerStabilityVerifyBoundsEachRead(t *testing.T) {
	m := fakeStability(stalledClient(t))
	m.readTimeout = 20 * time.Millisecond
	start := time.Now()
	err := m.verify(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "list NVCRE controller pods")
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestDiagnoseManagerCrash(t *testing.T) {
	tests := []struct {
		name    string
		pods    []managerPod
		wantErr string
	}{
		{
			name: "a healthy pod gives no diagnosis",
			pods: []managerPod{{name: testManagerPod, ready: true, startedAgo: time.Hour}},
		},
		{
			name: "a Ready pod that restarted long ago gives no diagnosis",
			pods: []managerPod{{name: testManagerPod, ready: true, startedAgo: time.Hour,
				restarts: 1, lastReason: reasonOOMKilled, lastExit: 137}},
		},
		{
			name: "a pod down after an OOMKill is named, with the limit",
			pods: []managerPod{{name: testManagerPod, startedAgo: time.Second, restarts: 5,
				lastReason: reasonOOMKilled, lastExit: 137, limit: testLimit}},
			wantErr: "restarted 5 times, last exit OOMKilled (exit code 137).\n" +
				"It ran out of memory at its 1Gi limit.",
		},
		{
			name: "the current exit is named, not the attempt before it",
			pods: []managerPod{{name: testManagerPod, restarts: 1, exitReason: reasonOOMKilled, exitCode: 137,
				lastReason: reasonError, lastExit: 1, limit: testLimit}},
			wantErr: "restarted 1 time, last exit OOMKilled (exit code 137).\n" +
				"It ran out of memory at its 1Gi limit.",
		},
		{
			name:    "a first exit before any restart is named",
			pods:    []managerPod{{name: testManagerPod, exitReason: reasonError, exitCode: 2}},
			wantErr: "restarted 0 times, last exit Error (exit code 2)",
		},
		{
			name: "another release's crash-looping manager pod gives no diagnosis",
			pods: []managerPod{{name: otherManagerPod, instance: otherRelease, waiting: reasonCrashLoopBackOff,
				restarts: 4, lastReason: reasonOOMKilled, lastExit: 137}},
		},
		{
			name:    "CrashLoopBackOff is named",
			pods:    []managerPod{{name: testManagerPod, waiting: reasonCrashLoopBackOff, restarts: 2, lastReason: reasonError, lastExit: 1}},
			wantErr: "pod nvcre/nvcre-manager-a restarted 2 times",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lists := 0
			err := diagnoseManagerCrash(context.Background(), pollingClient(t, [][]managerPod{tt.pods}, &lists), managerReadTimeout)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}

	t.Run("a failed read gives no diagnosis", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("forbidden")
			},
		}).Build()
		assert.NoError(t, diagnoseManagerCrash(context.Background(), c, managerReadTimeout))
	})

	t.Run("a stalled read gives no diagnosis once its bound passes", func(t *testing.T) {
		start := time.Now()
		assert.NoError(t, diagnoseManagerCrash(context.Background(), stalledClient(t), 20*time.Millisecond))
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}
