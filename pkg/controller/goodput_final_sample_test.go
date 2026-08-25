// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/podlogs"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// stubLogFetcher returns fixed log lines for every pod and records the
// options of the last fetch so tests can assert the read window.
type stubLogFetcher struct {
	lines    []string
	lastOpts *podlogs.LogOptions
}

func (s *stubLogFetcher) FetchLogs(_ context.Context, _, _ string, opts podlogs.LogOptions) ([]string, error) {
	s.lastOpts = &opts
	return s.lines, nil
}

// handleSucceeded and handleFailed build from the stored status, so the last
// sampling window would be lost without a final log read — observed on
// hardware as a script that logged to iteration 100 recorded as step 90.
//
// Before ADR-072 that final read went through handleRunning: it had to clear
// the sampling throttle to run at all, wrote an intermediate status, and
// re-asserted Measuring=True on an already-terminal Job — the two-write
// terminal sequence behind issue #177. collectFinalSample now bypasses the
// throttle without touching it, folds the window into the in-memory status,
// and performs no API write: only setComplete persists the terminal state.
// These cases pin all of that, plus the anchor cap on ApplicationStopTime.
func TestGoodputFinalSample(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "goodput-final-sample",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Call              string   `yaml:"call"`
			SampledSecondsAgo int      `yaml:"sampledSecondsAgo"`
			LogProfileRef     string   `yaml:"logProfileRef"`
			Logs              []string `yaml:"logs"`
			// Status seeds the measurement's persisted status, standing in
			// for values written by earlier periodic samples.
			Status struct {
				CurrentStep       int    `yaml:"currentStep"`
				HighestStep       int    `yaml:"highestStep"`
				LastStepTimestamp string `yaml:"lastStepTimestamp"`
				AvgStepTimeSec    string `yaml:"avgStepTimeSec"`
			} `yaml:"status"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := crev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		anchor := time.Date(2026, 1, 22, 10, 5, 0, 0, time.UTC)

		profile := &crev1alpha1.LogProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "lp"},
			Spec: crev1alpha1.LogProfileSpec{
				Timestamp: crev1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
				Patterns: crev1alpha1.LogPatternSet{
					TrainingStep: &crev1alpha1.EventPattern{
						Regex:   `step (?P<globalStep>\d+)`,
						Example: "step 100",
					},
				},
			},
		}
		m := &crev1alpha1.GoodputMeasurement{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
			Spec: crev1alpha1.GoodputMeasurementSpec{
				JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
				LogProfileRef: in.LogProfileRef,
			},
		}
		m.Status.CurrentStep = in.Status.CurrentStep
		m.Status.HighestStep = in.Status.HighestStep
		m.Status.AvgStepTimeSec = in.Status.AvgStepTimeSec
		if in.Status.LastStepTimestamp != "" {
			ts, err := time.Parse(time.RFC3339, in.Status.LastStepTimestamp)
			if err != nil {
				return err
			}
			mt := metav1.NewTime(ts)
			m.Status.LastStepTimestamp = &mt
		}
		initialStatus := m.Status.DeepCopy()
		job := &crev1alpha1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"},
			Status: crev1alpha1.JobStatus{
				WorkloadRef: &crev1alpha1.WorkloadReference{Kind: "TrainJob", Name: "w"},
				Conditions: []metav1.Condition{{
					Type:               crev1alpha1.JobSucceeded,
					Status:             metav1.ConditionTrue,
					Reason:             "WorkloadCompleted",
					LastTransitionTime: metav1.NewTime(anchor),
				}},
			},
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "w-node-0",
				Namespace: "ns",
				Labels: map[string]string{
					"jobset.sigs.k8s.io/jobset-name":           "w",
					"batch.kubernetes.io/job-completion-index": "0",
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(m, job, pod, profile).WithStatusSubresource(m, job).Build()
		fetcher := &stubLogFetcher{lines: in.Logs}
		r := &GoodputMeasurementReconciler{
			Client:     c,
			Scheme:     scheme,
			LogFetcher: fetcher,
		}

		key := "ns/m"
		throttledAt := time.Now().Add(-time.Duration(in.SampledSecondsAgo) * time.Second)
		r.mu.Lock()
		r.lastSample = map[string]time.Time{key: throttledAt}
		r.mu.Unlock()

		requeued := false
		if in.Call == "collectFinalSample" {
			r.collectFinalSample(context.Background(), m, job, anchor)
		} else {
			res, err := r.handleRunning(context.Background(), m, job)
			if err != nil {
				return err
			}
			requeued = res.RequeueAfter > 0
		}

		r.mu.Lock()
		stamp, present := r.lastSample[key]
		r.mu.Unlock()

		fresh := &crev1alpha1.GoodputMeasurement{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "m"}, fresh); err != nil {
			return err
		}

		stopTime := ""
		if m.Status.ApplicationStopTime != nil {
			stopTime = m.Status.ApplicationStopTime.UTC().Format(time.RFC3339)
		}
		sinceWindow := ""
		if fetcher.lastOpts != nil && fetcher.lastOpts.SinceTime != nil {
			sinceWindow = fetcher.lastOpts.SinceTime.UTC().Format(time.RFC3339)
		}

		// Compare marshaled bytes, not reflect.DeepEqual: the fake client
		// returns timestamps relocated to time.Local, which is a different
		// in-memory representation of the same instant.
		freshJSON, err := json.Marshal(fresh.Status)
		if err != nil {
			return err
		}
		initialJSON, err := json.Marshal(*initialStatus)
		if err != nil {
			return err
		}

		b, err := json.MarshalIndent(struct {
			CurrentStepInMemory         int    `json:"currentStepInMemory"`
			ApplicationStopTimeCappedAt string `json:"applicationStopTimeCappedAt"`
			APIStatusUntouched          bool   `json:"apiStatusUntouched"`
			ThrottleUntouched           bool   `json:"throttleUntouched"`
			AskedForRequeue             bool   `json:"askedForRequeue"`
			SinceWindowStart            string `json:"sinceWindowStart,omitempty"`
			AvgStepTimeSec              string `json:"avgStepTimeSec,omitempty"`
		}{
			CurrentStepInMemory:         m.Status.CurrentStep,
			ApplicationStopTimeCappedAt: stopTime,
			APIStatusUntouched:          string(freshJSON) == string(initialJSON),
			ThrottleUntouched:           present && stamp.Equal(throttledAt),
			AskedForRequeue:             requeued,
			SinceWindowStart:            sinceWindow,
			AvgStepTimeSec:              m.Status.AvgStepTimeSec,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
