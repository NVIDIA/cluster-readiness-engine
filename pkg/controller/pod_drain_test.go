// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/nodemonitor"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// failingPodLister makes every List call fail so the barrier's error branches
// can be exercised: fail closed (keep waiting) before the grace period, fail
// open (proceed) after it.
type failingPodLister struct{ client.Reader }

func (f failingPodLister) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errors.New("pod list unavailable")
}

// The pod-drain barrier (issue #121) is what stands between a deleted workload
// and the DRA-backed ComputeDomain its terminating pods still depend on. These
// cases pin the decision table directly, including the branches the
// integration harness cannot reach: the 5-minute grace expiry (fabricated by
// giving the Job terminal-condition timestamps older than the grace period —
// the anchor is persisted state, so no clock injection is needed) and the
// list-error fallbacks. The anchor selection in drainStart (latest of the true
// terminal conditions and the deletionTimestamp) is pinned by labeling which
// input timestamp the returned anchor equals.
func TestShouldWaitForPodDrain(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "pod-drain",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Job struct {
				FailedAgeSeconds         int  `yaml:"failedAgeSeconds"`
				HardwareFailedAgeSeconds int  `yaml:"hardwareFailedAgeSeconds"`
				SucceededAgeSeconds      int  `yaml:"succeededAgeSeconds"`
				DeletionAgeSeconds       int  `yaml:"deletionAgeSeconds"`
				NoConditions             bool `yaml:"noConditions"`
			} `yaml:"job"`
			Pods []struct {
				Name     string `yaml:"name"`
				Phase    string `yaml:"phase"`
				OtherJob bool   `yaml:"otherJob"`
			} `yaml:"pods"`
			ListError bool `yaml:"listError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		now := time.Now()
		anchors := map[string]metav1.Time{}
		job := &nvcrev1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"}}
		addCondition := func(condType string, ageSeconds int) {
			if ageSeconds == 0 {
				return
			}
			at := metav1.NewTime(now.Add(-time.Duration(ageSeconds) * time.Second))
			anchors["condition:"+condType] = at
			job.Status.Conditions = append(job.Status.Conditions, metav1.Condition{
				Type:               condType,
				Status:             metav1.ConditionTrue,
				Reason:             "TestFixture",
				LastTransitionTime: at,
			})
		}
		addCondition(nvcrev1alpha1.JobFailed, input.Job.FailedAgeSeconds)
		addCondition(nvcrev1alpha1.JobHardwareFailed, input.Job.HardwareFailedAgeSeconds)
		addCondition(nvcrev1alpha1.JobSucceeded, input.Job.SucceededAgeSeconds)
		if input.Job.DeletionAgeSeconds != 0 {
			at := metav1.NewTime(now.Add(-time.Duration(input.Job.DeletionAgeSeconds) * time.Second))
			anchors["deletionTimestamp"] = at
			job.DeletionTimestamp = &at
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		objs := make([]client.Object, 0, len(input.Pods))
		for _, sp := range input.Pods {
			jobLabel := job.Name
			if sp.OtherJob {
				jobLabel = "some-other-job"
			}
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: sp.Name, Namespace: job.Namespace,
					Labels: map[string]string{nodemonitor.NVCREJobLabel: jobLabel},
				},
				Status: corev1.PodStatus{Phase: corev1.PodPhase(sp.Phase)},
			})
		}
		var reader client.Reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
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
		if input.ListError {
			reader = failingPodLister{reader}
		}

		anchorLabel := "none"
		if anchor := drainStart(job); anchor != nil {
			for label, at := range anchors {
				if anchor.Time.Equal(at.Time) {
					anchorLabel = label
				}
			}
		}

		active, activeErr := activeWorkloadPods(context.Background(), reader, job.Namespace, job.Name)
		if active == nil && activeErr == nil {
			active = []string{}
		}

		out := struct {
			Wait        bool     `json:"wait"`
			DrainAnchor string   `json:"drainAnchor"`
			ActivePods  []string `json:"activePods"`
			ListError   bool     `json:"listError,omitempty"`
		}{
			Wait:        shouldWaitForPodDrain(context.Background(), reader, job),
			DrainAnchor: anchorLabel,
			ActivePods:  active,
			ListError:   activeErr != nil,
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
