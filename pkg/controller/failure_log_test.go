// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// The stored tail was 30 lines, right for a workload whose last line is the
// error and wrong for one printing a structured document: a failed dcgmi diag
// stored 392 bytes ending in "status" : "Pass". The byte cap also has to trim
// from the front, because the newest output is the part that matters.
func TestFailureLogTail(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "failure-log-tail",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			MaxBytes int    `yaml:"maxBytes"`
			Text     string `yaml:"text"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		got, err := readLogTail(strings.NewReader(in.Text), in.MaxBytes)
		if err != nil {
			return err
		}

		b, err := json.MarshalIndent(struct {
			Kept      string `json:"kept"`
			KeptBytes int    `json:"keptBytes"`
		}{got, len(got)}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// stubContainer is the main container's name in the catalog's pod templates,
// and the worker replicatedJob's name.
const stubContainer = "node"

// failureLogInput is a Job's pods as captureFailureLog and launcherFailureLog
// see them. A launcher pod carries the JobSet labels of the MPI launcher and,
// as in a real run, not the nvcre.nvidia.com/job label the workers carry.
//
// The workload named by workloadRef exists, created at workloadCreated
// (seconds, like a pod's created), unless workloadGone is set. Pods created
// before it belong to a generation a checkpoint restart replaced. A pod with
// disruptionTarget set carries DisruptionTarget=True, as an evicted or
// preempted pod does.
type failureLogInput struct {
	WorkloadRef     string `yaml:"workloadRef"`
	WorkloadCreated int64  `yaml:"workloadCreated"`
	WorkloadGone    bool   `yaml:"workloadGone"`
	Recorded        bool   `yaml:"recorded"`
	Pods            []struct {
		Name       string `yaml:"name"`
		Created    int64  `yaml:"created"`
		Launcher   bool   `yaml:"launcher"`
		Running    bool   `yaml:"running"`
		ExitCode   int32  `yaml:"exitCode"`
		Reason     string `yaml:"reason"`
		FinishedAt int64  `yaml:"finishedAt"`
		Log        string `yaml:"log"`
		// DisruptionTarget marks the pod evicted or preempted.
		DisruptionTarget bool `yaml:"disruptionTarget"`
	} `yaml:"pods"`
}

// newFailureLogReconciler builds a reconciler over the input's pods, with a
// Clientset whose log endpoint serves each pod's log.
func newFailureLogReconciler(t *testing.T, raw string) (*JobReconciler, *nvcrev1alpha1.Job, error) {
	t.Helper()
	var in failureLogInput
	if err := yaml.Unmarshal([]byte(raw), &in); err != nil {
		return nil, nil, err
	}

	scheme := runtime.NewScheme()
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	if err := trainerv1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}
	at := func(sec int64) metav1.Time { return metav1.NewTime(time.Unix(1_700_000_000+sec, 0)) }

	logs := map[string]string{}
	objs := make([]client.Object, 0, len(in.Pods))
	for _, sp := range in.Pods {
		labels := map[string]string{"nvcre.nvidia.com/job": "j"}
		if sp.Launcher {
			labels = map[string]string{
				jobSetNameLabel:        in.WorkloadRef,
				replicatedJobNameLabel: workload.LauncherJobName,
			}
		}
		pod := &corev1.Pod{Name: sp.Name, Namespace: "ns", Labels: labels, CreationTimestamp: at(sp.Created)}
		pod.Spec.NodeName = "node-" + sp.Name
		switch {
		case sp.Running:
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  stubContainer,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}
		case sp.ExitCode != 0:
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: stubContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   sp.ExitCode,
					Reason:     sp.Reason,
					FinishedAt: at(sp.FinishedAt),
				}},
			}}
		}
		if sp.DisruptionTarget {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue,
			}}
		}
		logs[sp.Name] = sp.Log
		objs = append(objs, pod)
	}

	job := &nvcrev1alpha1.Job{Name: "j", Namespace: "ns"}
	job.Spec.Workload.TrainJob = &trainerv1alpha1.TrainJobSpec{}
	if in.WorkloadRef != "" {
		job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{Kind: testKindTrainJob, Name: in.WorkloadRef}
		if !in.WorkloadGone {
			objs = append(objs, &trainerv1alpha1.TrainJob{
				Name: in.WorkloadRef, Namespace: "ns", CreationTimestamp: at(in.WorkloadCreated),
			})
		}
	}
	if in.Recorded {
		job.Status.FailureLog = &nvcrev1alpha1.FailureLog{PodName: "recorded-earlier"}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&corev1.Pod{}, nodemonitor.PodNVCREJobIndexField, func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return nil
			}
			if jn, found := pod.Labels[nodemonitor.NVCREJobLabel]; found {
				return []string{jn}
			}
			return nil
		}).
		Build()

	cs, err := newPodLogClientset(t, logs)
	if err != nil {
		return nil, nil, err
	}
	return &JobReconciler{Client: c, Scheme: scheme, Clientset: cs}, job, nil
}

// newPodLogClientset returns a Clientset backed by an httptest server whose
// pod log endpoint serves logs[pod name], in any namespace.
func newPodLogClientset(t *testing.T, logs map[string]string) (*kubernetes.Clientset, error) {
	t.Helper()
	// GET /api/v1/namespaces/<ns>/pods/<name>/log
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) != 7 || parts[6] != "log" {
			http.NotFound(w, req)
			return
		}
		_, _ = io.WriteString(w, logs[parts[5]])
	}))
	t.Cleanup(srv.Close)
	return kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
}

// A Job that fails after its pod is gone must still carry a record saying so.
// captureFailureLog used to return silently, leaving status.failureLog unset.
// The MPI launcher is found by its JobSet labels, since it does not carry the
// label the workers do (issue #498).
func TestFailureLogCapture(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "failure-log-capture",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		r, job, err := newFailureLogReconciler(t, tc.Inputs["input.yaml"])
		if err != nil {
			return err
		}

		r.captureFailureLog(context.Background(), job)

		b, err := json.MarshalIndent(job.Status.FailureLog, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// launcherFailureLog records a failed MPI launcher while the workload still
// reports running, because JobSet deletes the launcher pod about half a second
// after it fails, before the failure reaches the Job (issue #498). It returns
// null when there is nothing to record or the Job already carries a record.
func TestLauncherFailureLog(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "launcher-failure-log",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		r, job, err := newFailureLogReconciler(t, tc.Inputs["input.yaml"])
		if err != nil {
			return err
		}

		got := r.launcherFailureLog(context.Background(), job)

		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// Two properties are not input-in, JSON-out: that the window survives a stream
// arriving one byte at a time, and the limits themselves.
func TestReadLogTailAcrossChunkedReads(t *testing.T) {
	var sb strings.Builder
	for i := range 5000 {
		sb.WriteString("log line ")
		sb.WriteString(strings.Repeat("x", i%3))
		sb.WriteString("\n")
	}
	full := sb.String()

	got, err := readLogTail(oneByteReader{strings.NewReader(full)}, 512)
	require.NoError(t, err)
	require.Len(t, got, 512)
	require.Equal(t, full[len(full)-512:], got)
}

type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestFailureLogLimits(t *testing.T) {
	require.Equal(t, 800, failureLogTailLines,
		"30 lines truncated dcgmi diag --json to its closing braces")
	require.Equal(t, 32*1024, failureLogMaxBytes)
}

// failedContainerOf decides whose log status.failureLog carries. When an MPI
// launcher fails, JobSet tears the workers down while NVCRE records the
// failure, so a worker's sshd can exit non-zero too. The pod list has no
// meaningful order, so the pick has to be the container that failed first
// (issue #498), with OOMKilled still winning as the root cause.
func TestFailedContainerOf(t *testing.T) {
	at := func(sec int) metav1.Time { return metav1.NewTime(time.Unix(1_700_000_000+int64(sec), 0)) }
	terminated := func(name string, code int32, reason string, finished metav1.Time) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, FinishedAt: finished,
		}}}
	}
	pod := func(name string, init, main []corev1.ContainerStatus) corev1.Pod {
		return corev1.Pod{
			Name:   name,
			Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: main},
		}
	}
	// The pods' main containers are all named "node" in the catalog; distinct
	// names here make each case's expected pick readable.
	const launcher, mpirun, sshd = "launcher-a", "mpirun", "sshd"
	running := corev1.ContainerStatus{Name: sshd, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}

	tests := []struct {
		name          string
		pods          []corev1.Pod
		wantPod       string
		wantContainer string
	}{{
		name: "launcher failed before a worker's sshd exited, worker listed first",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", at(40))}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
			pod("worker-1", nil, []corev1.ContainerStatus{running}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "first launcher attempt over its replacement",
		pods: []corev1.Pod{
			pod("launcher-b", nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(30))}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "OOMKilled outranks an earlier failure",
		pods: []corev1.Pod{
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 137, "OOMKilled", at(20))}),
		},
		wantPod: "worker-0", wantContainer: sshd,
	}, {
		name: "an init container failure is found (the KAI launcher wait)",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", at(1300))}),
			pod(launcher, []corev1.ContainerStatus{
				terminated("fix-ssh-permissions", 0, "Completed", at(1)),
				terminated("wait-for-workers", 1, "Error", at(1201)),
			}, []corev1.ContainerStatus{{Name: mpirun, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}),
		},
		wantPod: launcher, wantContainer: "wait-for-workers",
	}, {
		name: "a recorded finish time outranks a missing one",
		pods: []corev1.Pod{
			pod("worker-0", nil, []corev1.ContainerStatus{terminated(sshd, 255, "Error", metav1.Time{})}),
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 1, "Error", at(10))}),
		},
		wantPod: launcher, wantContainer: mpirun,
	}, {
		name: "nothing failed",
		pods: []corev1.Pod{
			pod(launcher, nil, []corev1.ContainerStatus{terminated(mpirun, 0, "Completed", at(10))}),
			pod("worker-0", nil, []corev1.ContainerStatus{running}),
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPod, gotContainer := failedContainerOf(tt.pods)
			if tt.wantPod == "" {
				require.Nil(t, gotPod)
				require.Nil(t, gotContainer)
				return
			}
			require.NotNil(t, gotPod)
			require.Equal(t, tt.wantPod, gotPod.Name)
			require.Equal(t, tt.wantContainer, gotContainer.Name)
		})
	}
}

// The launcher pod watch wakes the Job only on the event in which a launcher
// is first seen failed. Every other pod event in the cluster passes through
// this predicate, so it must stay this narrow.
func TestLauncherFailedPredicate(t *testing.T) {
	pod := func(launcher bool, exitCode int32, conditions ...corev1.PodCondition) *corev1.Pod {
		p := &corev1.Pod{Name: "p", Namespace: "ns", Labels: map[string]string{replicatedJobNameLabel: stubContainer}}
		if launcher {
			p.Labels[replicatedJobNameLabel] = workload.LauncherJobName
		}
		state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		if exitCode >= 0 {
			state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}}
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: stubContainer, State: state}}
		p.Status.Conditions = conditions
		return p
	}
	const running = -1
	evicted := corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue}
	pred := launcherFailedPredicate()
	tests := []struct {
		name     string
		old, new *corev1.Pod
		want     bool
	}{
		{name: "launcher turns failed", old: pod(true, running), new: pod(true, 1), want: true},
		{name: "launcher already failed", old: pod(true, 1), new: pod(true, 1)},
		{name: "launcher succeeds", old: pod(true, running), new: pod(true, 0)},
		{name: "worker turns failed", old: pod(false, running), new: pod(false, 137)},
		// The launcher Job's podFailurePolicy ignores this failure and
		// replaces the pod, so it is not the run's failure.
		{name: "evicted launcher turns failed", old: pod(true, running, evicted), new: pod(true, 137, evicted)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, pred.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
	require.True(t, pred.Create(event.CreateEvent{Object: pod(true, 1)}), "a failed launcher seen at startup")
	require.False(t, pred.Create(event.CreateEvent{Object: pod(true, running)}))
	require.False(t, pred.Create(event.CreateEvent{Object: pod(true, 137, evicted)}), "an evicted launcher seen at startup")
	require.False(t, pred.Delete(event.DeleteEvent{Object: pod(true, 1)}))
}

// A launcher pod names its JobSet, which is named after the TrainJob; the
// TrainJob names the Job in its nvcre.nvidia.com/job label.
func TestLauncherPodToJob(t *testing.T) {
	scheme := newWorkflowScheme(t)
	require.NoError(t, trainerv1alpha1.AddToScheme(scheme))
	trainJob := &trainerv1alpha1.TrainJob{Name: "j-workload", Namespace: "ns",
		Labels: map[string]string{labelJobKey: "j"}}
	unlabelled := &trainerv1alpha1.TrainJob{Name: "other-workload", Namespace: "ns"}
	r := &JobReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(trainJob, unlabelled).Build()}
	launcher := func(jobSet string) *corev1.Pod {
		return &corev1.Pod{Name: jobSet + "-launcher-0-0", Namespace: "ns",
			Labels: map[string]string{jobSetNameLabel: jobSet, replicatedJobNameLabel: workload.LauncherJobName}}
	}
	ctx := context.Background()

	require.Equal(t, []reconcile.Request{{Namespace: "ns", Name: "j"}}, r.launcherPodToJob(ctx, launcher("j-workload")))
	require.Empty(t, r.launcherPodToJob(ctx, launcher("other-workload")), "a TrainJob NVCRE did not create")
	require.Empty(t, r.launcherPodToJob(ctx, launcher("missing")), "no TrainJob")
	require.Empty(t, r.launcherPodToJob(ctx, &corev1.Pod{Name: "bare", Namespace: "ns"}), "not a JobSet pod")
}

// A failure log recorded from a launcher attempt is dropped when the run
// succeeds after the launcher Job's retry, so a Succeeded Job never carries a
// failure that did not decide the run. restartFromCheckpoint drops it with the
// same clearFailureLog, which TestJobRestartDoesNotReAnnounceInProgress pins.
func TestSucceededJobDropsLauncherFailureLog(t *testing.T) {
	ctx := context.Background()
	job := &nvcrev1alpha1.Job{Name: "j", Namespace: testNS, Status: nvcrev1alpha1.JobStatus{
		FailureLog: &nvcrev1alpha1.FailureLog{PodName: "j-workload-launcher-0-0-a", ExitCode: 1},
	}}
	c := fake.NewClientBuilder().WithScheme(newWorkflowScheme(t)).WithObjects(job).
		WithStatusSubresource(job).Build()
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), job))
	r := &JobReconciler{Client: c, Recorder: events.NewFakeRecorder(10)}

	require.NoError(t, r.setJobSucceeded(ctx, job, ReasonWorkloadCompleted, "done"))

	persisted := &nvcrev1alpha1.Job{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(job), persisted))
	require.Nil(t, persisted.Status.FailureLog)
	require.False(t, clearFailureLog(persisted), "nothing left to clear")
}
