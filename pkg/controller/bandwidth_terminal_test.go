// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type bandwidthTerminalInput struct {
	// JobCondition is the Job's terminal condition; empty means the Job does
	// not exist.
	JobCondition  string          `yaml:"jobCondition"`
	NoWorkloadRef bool            `yaml:"noWorkloadRef"`
	PodPhase      corev1.PodPhase `yaml:"podPhase"`
	Log           *string         `yaml:"log"`
	// LogTruncated makes the log read report that it stopped at the byte
	// limit, as a log larger than one page does.
	LogTruncated bool                            `yaml:"logTruncated"`
	Provisional  []nvcrev1alpha1.BandwidthResult `yaml:"provisional"`
	// ProvisionalTransport seeds the network names a running sample recorded.
	ProvisionalTransport []string `yaml:"provisionalTransport"`
	PendingSince         string   `yaml:"pendingSince"`
	// PendingForSeconds seeds the failed final read that many seconds before
	// the reconcile, for cases that sit on either side of the grace period.
	PendingForSeconds int `yaml:"pendingForSeconds"`
	// MeasurementTimeout sets the Job's spec.measurementTimeout.
	MeasurementTimeout string `yaml:"measurementTimeout"`
	AlreadyComplete    bool   `yaml:"alreadyComplete"`
	// MeasuredJobUID is the UID of the Job the measurement was created for;
	// the fixture Job's UID is uid-current.
	MeasuredJobUID string `yaml:"measuredJobUID"`
}

type bandwidthTerminalOutput struct {
	Requeue        bool                            `json:"requeue"`
	Conditions     []conditionOut                  `json:"conditions"`
	Results        []nvcrev1alpha1.BandwidthResult `json:"results,omitempty"`
	Transport      []string                        `json:"transport,omitempty"`
	CompletionTime string                          `json:"completionTime,omitempty"`
}

// currentJobUID is the UID of the fixture Job; a measurement annotated with
// any other UID was created for an earlier Job of the same name.
const currentJobUID = "uid-current"

// jobTerminalTime is the fixture Job's terminal transition; completionTime is
// anchored to it rather than to the reconcile clock.
var jobTerminalTime = time.Date(2026, 9, 30, 10, 5, 0, 0, time.UTC)

// TestBandwidthTerminal pins how a BandwidthMeasurement completes once its Job
// is terminal: final results replace the provisional samples, a failed final
// read is retried within the grace period and then completes without final
// results, and a completed measurement is not rewritten.
func TestBandwidthTerminal(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "bandwidth-terminal",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in bandwidthTerminalInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		m, err := bandwidthTerminalMeasurement(in)
		if err != nil {
			return err
		}
		objs := []client.Object{m, ncclBandwidthProfile()}
		statusObjs := []client.Object{m}
		if in.JobCondition != "" {
			job, err := bandwidthTerminalJob(in)
			if err != nil {
				return err
			}
			objs = append(objs, job)
			statusObjs = append(statusObjs, job)
		}
		if in.PodPhase != "" {
			objs = append(objs, &corev1.Pod{
				Name: "w-launcher-0-0", Namespace: "ns",
				Labels: map[string]string{
					"jobset.sigs.k8s.io/jobset-name":        "w",
					"jobset.sigs.k8s.io/replicatedjob-name": "launcher",
				},
				Status: corev1.PodStatus{Phase: in.PodPhase},
			})
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(objs...).WithStatusSubresource(statusObjs...).Build()

		r := &BandwidthMeasurementReconciler{
			Client: c, APIReader: c, Scheme: scheme,
			RequeueInterval: time.Second,
			LogFetcher:      staticLogFetcher{log: in.Log, truncated: in.LogTruncated},
		}
		key := types.NamespacedName{Name: "m", Namespace: "ns"}
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			return err
		}

		got := &nvcrev1alpha1.BandwidthMeasurement{}
		if err := c.Get(context.Background(), key, got); err != nil {
			return err
		}
		out := bandwidthTerminalOutput{
			Requeue: res.RequeueAfter > 0, Results: got.Status.Results, Transport: got.Status.Transport,
		}
		for _, cond := range got.Status.Conditions {
			out.Conditions = append(out.Conditions, conditionOut{
				Type: cond.Type, Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message,
			})
		}
		if ct := got.Status.CompletionTime; ct != nil {
			// A confirmed-deleted Job has no terminal time to anchor to.
			if ct.Time.Equal(jobTerminalTime) {
				out.CompletionTime = ct.UTC().Format(time.RFC3339)
			} else {
				out.CompletionTime = "reconcile time"
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func bandwidthTerminalMeasurement(in bandwidthTerminalInput) (*nvcrev1alpha1.BandwidthMeasurement, error) {
	m := &nvcrev1alpha1.BandwidthMeasurement{
		Name: "m", Namespace: "ns",
		Finalizers: []string{bandwidthMeasurementFinalizer},
		Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
			JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
			LogProfileRef: "nccl-bandwidth",
		},
		Status: nvcrev1alpha1.BandwidthMeasurementStatus{
			Results: in.Provisional, Transport: in.ProvisionalTransport,
		},
	}
	if in.MeasuredJobUID != "" {
		m.Annotations = map[string]string{annotationJobUID: in.MeasuredJobUID}
	}
	if in.PendingSince != "" || in.PendingForSeconds > 0 {
		since := time.Now().Add(-time.Duration(in.PendingForSeconds) * time.Second)
		if in.PendingSince != "" {
			var err error
			if since, err = time.Parse(time.RFC3339, in.PendingSince); err != nil {
				return nil, fmt.Errorf("pendingSince: %w", err)
			}
		}
		m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
			Type: nvcrev1alpha1.BandwidthMeasurementMeasuring, Status: metav1.ConditionFalse,
			Reason: reasonBandwidthFinalReadPending, Message: "Job succeeded; waiting to read its complete log",
			LastTransitionTime: metav1.NewTime(since),
		})
	}
	if in.AlreadyComplete {
		m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
			Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue,
			Reason: reasonBandwidthJobSucceeded, Message: "Job succeeded",
			LastTransitionTime: metav1.NewTime(jobTerminalTime),
		})
	}
	return m, nil
}

func bandwidthTerminalJob(in bandwidthTerminalInput) (*nvcrev1alpha1.Job, error) {
	job := &nvcrev1alpha1.Job{
		Name: "j", Namespace: "ns", UID: currentJobUID,
		Status: nvcrev1alpha1.JobStatus{Conditions: []metav1.Condition{{
			Type: in.JobCondition, Status: metav1.ConditionTrue,
			Reason: "Test", LastTransitionTime: metav1.NewTime(jobTerminalTime),
		}}},
	}
	if !in.NoWorkloadRef {
		job.Status.WorkloadRef = &nvcrev1alpha1.WorkloadReference{Kind: "TrainJob", Name: "w"}
	}
	if in.MeasurementTimeout != "" {
		d, err := time.ParseDuration(in.MeasurementTimeout)
		if err != nil {
			return nil, fmt.Errorf("measurementTimeout: %w", err)
		}
		job.Spec.MeasurementTimeout = &metav1.Duration{Duration: d}
	}
	return job, nil
}

// ncclBandwidthProfile mirrors the chart's nccl-bandwidth LogProfile.
func ncclBandwidthProfile() *nvcrev1alpha1.LogProfile {
	return &nvcrev1alpha1.LogProfile{
		Name: "nccl-bandwidth",
		Spec: nvcrev1alpha1.LogProfileSpec{
			Timestamp:      nvcrev1alpha1.TimestampSpec{Layout: "2006-01-02T15:04:05.999999999Z"},
			WorkerStrategy: &nvcrev1alpha1.WorkerStrategySpec{Type: "Single", ReplicatedJobName: "launcher"},
			Patterns: nvcrev1alpha1.LogPatternSet{
				BandwidthResult: &nvcrev1alpha1.EventPattern{
					Regex: `^\s*(?P<size>\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`,
				},
				NetworkTransport: &nvcrev1alpha1.EventPattern{
					Regex: `NCCL INFO Using network (?P<transport>.+)`,
				},
			},
		},
	}
}

// staticLogFetcher serves one fixed log for every pod, or fails when there is
// none.
type staticLogFetcher struct {
	log       *string
	truncated bool
}

func (f staticLogFetcher) FetchLogsPage(ctx context.Context, namespace, podName string, opts podlogs.LogOptions) (podlogs.Page, error) {
	lines, err := f.FetchLogs(ctx, namespace, podName, opts)
	return podlogs.Page{Lines: lines, Truncated: f.truncated}, err
}

func (f staticLogFetcher) FetchLogs(_ context.Context, _, podName string, _ podlogs.LogOptions) ([]string, error) {
	if f.log == nil {
		return nil, fmt.Errorf("no log for pod %s", podName)
	}
	return strings.Split(strings.TrimSuffix(*f.log, "\n"), "\n"), nil
}
