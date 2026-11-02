// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestJobStallWorkersExited pins the stall verdict for a workload whose
// worker pods have already exited while the TrainJob has not yet reported a
// terminal condition (issue #575).
//
// After Megatron reaches --exit-duration-in-mins it stops logging steps, saves
// its exit checkpoint and exits 0. The TrainJob reports Complete some seconds
// later. Until then the adapter reports the workload as running, so the stall
// budget keeps counting from the last logged step and could fail a finished
// run as WorkloadStalled. A stall cannot be declared over workers that have
// all exited successfully: the Job waits for the TrainJob's own verdict, and
// timeoutPerJob still bounds that wait.
//
// Every case has a stalled GoodputMeasurement, so only the worker pods decide.
func TestJobStallWorkersExited(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "job-stall-workers-exited",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		defer cleanupJobMetrics(testNS, testJobName)

		var input struct {
			// Pods are worker pods carrying the job label.
			Pods []struct {
				Name string `yaml:"name"`
				// Phase is Succeeded, Failed or Running. A Succeeded pod's
				// container exited 0, a Failed pod's exited 1.
				Phase corev1.PodPhase `yaml:"phase"`
				// BeforeWorkload creates the pod before the current workload,
				// as a pod of the workload a checkpoint restart replaced is.
				BeforeWorkload bool `yaml:"beforeWorkload"`
			} `yaml:"pods"`
			// MaxRestarts, when set, gives the Job a checkpoint restart budget
			// and the measurement a saved checkpoint, so a stall restarts the
			// workload instead of failing the Job.
			MaxRestarts *int32 `yaml:"maxRestarts"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		ctx := context.Background()
		scheme := newJobLookupWorkloadScheme(t)
		now := time.Now()

		job := newLookupJob(true)
		multiplier := int32(3)
		job.Spec.StallMultiplier = &multiplier
		start := metav1.NewTime(now.Add(-2 * time.Hour))
		job.Status.WorkloadStartTime = &start
		if input.MaxRestarts != nil {
			job.Spec.Checkpoint = &nvcrev1alpha1.CheckpointConfig{MaxRestarts: input.MaxRestarts}
		}

		tj := newLookupTrainJob(holderOwn)
		tj.CreationTimestamp = metav1.NewTime(now.Add(-2 * time.Hour))

		// The numbers from the issue: 0.525 s/step, --log-interval 10 and a
		// 10s sample interval give a 35.75s budget. The last step is a
		// minute old, so the budget has run out.
		lastStep := metav1.NewTime(now.Add(-time.Minute))
		sample := metav1.Duration{Duration: 10 * time.Second}
		gm := &nvcrev1alpha1.GoodputMeasurement{
			Name: "gm", Namespace: testNS,
			Spec: nvcrev1alpha1.GoodputMeasurementSpec{
				JobRef:         corev1.TypedLocalObjectReference{Kind: kindJob, Name: testJobName},
				SampleInterval: &sample,
			},
			Status: nvcrev1alpha1.GoodputMeasurementStatus{
				LastStepTimestamp: &lastStep,
				AvgStepTimeSec:    "0.525",
				LogInterval:       10,
			},
		}
		if input.MaxRestarts != nil {
			gm.Status.LastCheckpointStep = 6594
		}

		objects := []client.Object{job.DeepCopy(), tj, gm}
		for _, wp := range input.Pods {
			created := now.Add(-time.Hour)
			if wp.BeforeWorkload {
				created = now.Add(-3 * time.Hour)
			}
			pod := &corev1.Pod{
				Name: wp.Name, Namespace: testNS,
				Labels:            map[string]string{podread.JobLabel: testJobName},
				CreationTimestamp: metav1.NewTime(created),
			}
			pod.Spec.NodeName = "node-" + wp.Name
			pod.Status.Phase = wp.Phase
			state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			switch wp.Phase {
			case corev1.PodSucceeded:
				state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0, Reason: "Completed",
				}}
			case corev1.PodFailed:
				state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error",
				}}
			}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: stubContainer, State: state}}
			objects = append(objects, pod)
		}

		apiServer := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objects...).
			WithStatusSubresource(&nvcrev1alpha1.Job{}).
			WithIndex(&nvcrev1alpha1.GoodputMeasurement{}, measurementJobRefIndexField, func(obj client.Object) []string {
				return []string{obj.(*nvcrev1alpha1.GoodputMeasurement).Spec.JobRef.Name}
			}).
			Build()
		if err := apiServer.Get(ctx,
			client.ObjectKey{Namespace: testNS, Name: testJobName}, job); err != nil {
			return err
		}

		r := &JobReconciler{
			Client: apiServer, APIReader: apiServer, Scheme: scheme,
			Recorder: events.NewFakeRecorder(20),
		}
		result, reconcileErr := r.updateStatusFromWorkload(ctx, job)

		out, err := readJobOutput(ctx, apiServer, result, reconcileErr)
		if err != nil {
			return err
		}
		tc.Actual = out
		return nil
	})
}
