// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// setComplete is the single terminal status write and its first-write-wins
// guard is the freeze: once Complete is True on the live object, no replay —
// fresh-cache or stale-cache — may overwrite the frozen status (ADR-072).
// These cases pin the guard itself: a replay against a live Complete=True
// object performs no write at all (unchanged status bytes, unchanged
// resourceVersion), including the stale-cache path where the first attempt
// conflicts and the guard fires on the refreshed object.
func TestGoodputSetComplete(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "goodput-set-complete",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// LiveComplete seeds the live object with an already-landed
			// terminal write (Complete=True, frozen values).
			LiveComplete bool `yaml:"liveComplete"`
			// StaleInMemory hands setComplete a copy taken before the live
			// object gained Complete, forcing the conflict-then-refresh path.
			StaleInMemory bool `yaml:"staleInMemory"`
			// AttemptedResult is the result this pass computed and would write.
			AttemptedResult string `yaml:"attemptedResult"`
			Anchor          string `yaml:"anchor"`
			Reason          string `yaml:"reason"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		anchor, err := time.Parse(time.RFC3339, in.Anchor)
		if err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := crev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		ctx := context.Background()
		key := types.NamespacedName{Namespace: "ns", Name: "m"}
		frozenAt := metav1.NewTime(time.Date(2026, 1, 22, 10, 5, 0, 0, time.UTC))

		m := &crev1alpha1.GoodputMeasurement{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
			Status: crev1alpha1.GoodputMeasurementStatus{
				Result:          "0.900000",
				TrainingTimeSec: "295.000000",
			},
		}
		if in.LiveComplete {
			m.Status.CompletionTime = &frozenAt
			meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
				Type:    crev1alpha1.GoodputMeasurementComplete,
				Status:  metav1.ConditionTrue,
				Reason:  "JobSucceeded",
				Message: "Referenced Job completed successfully",
			})
		}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(m).WithStatusSubresource(m).Build()
		r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}

		// The copy handed to setComplete: fetched fresh, or — for the
		// stale-cache replay — taken before the live object gained Complete.
		arg := &crev1alpha1.GoodputMeasurement{}
		if err := c.Get(ctx, key, arg); err != nil {
			return err
		}
		if in.StaleInMemory {
			live := &crev1alpha1.GoodputMeasurement{}
			if err := c.Get(ctx, key, live); err != nil {
				return err
			}
			live.Status.CompletionTime = &frozenAt
			meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{
				Type:    crev1alpha1.GoodputMeasurementComplete,
				Status:  metav1.ConditionTrue,
				Reason:  "JobSucceeded",
				Message: "Referenced Job completed successfully",
			})
			if err := c.Status().Update(ctx, live); err != nil {
				return err
			}
		}

		before := &crev1alpha1.GoodputMeasurement{}
		if err := c.Get(ctx, key, before); err != nil {
			return err
		}
		beforeStatus, err := json.Marshal(before.Status)
		if err != nil {
			return err
		}

		// The replay computed different values than the frozen ones.
		if in.AttemptedResult != "" {
			arg.Status.Result = in.AttemptedResult
			arg.Status.AvgStepTimeSec = "9.999999"
		}
		callErr := r.setComplete(ctx, arg, anchor, in.Reason, "test message")

		after := &crev1alpha1.GoodputMeasurement{}
		if err := c.Get(ctx, key, after); err != nil {
			return err
		}
		afterStatus, err := json.Marshal(after.Status)
		if err != nil {
			return err
		}

		completionTime := ""
		if after.Status.CompletionTime != nil {
			completionTime = after.Status.CompletionTime.UTC().Format(time.RFC3339)
		}
		completeReason := ""
		if cond := meta.FindStatusCondition(after.Status.Conditions, crev1alpha1.GoodputMeasurementComplete); cond != nil {
			completeReason = cond.Reason
		}
		measuringStatus := ""
		if cond := meta.FindStatusCondition(after.Status.Conditions, crev1alpha1.GoodputMeasurementMeasuring); cond != nil {
			measuringStatus = string(cond.Status)
		}

		b, err := json.MarshalIndent(struct {
			Errored                  bool   `json:"errored"`
			LiveResourceVersionMoved bool   `json:"liveResourceVersionMoved"`
			LiveStatusBytesUnchanged bool   `json:"liveStatusBytesUnchanged"`
			LiveResult               string `json:"liveResult"`
			LiveCompletionTime       string `json:"liveCompletionTime"`
			LiveCompleteReason       string `json:"liveCompleteReason"`
			LiveMeasuringStatus      string `json:"liveMeasuringStatus,omitempty"`
			LiveAvgStepTimeSec       string `json:"liveAvgStepTimeSec,omitempty"`
		}{
			Errored:                  callErr != nil,
			LiveResourceVersionMoved: before.ResourceVersion != after.ResourceVersion,
			LiveStatusBytesUnchanged: string(beforeStatus) == string(afterStatus),
			LiveResult:               after.Status.Result,
			LiveCompletionTime:       completionTime,
			LiveCompleteReason:       completeReason,
			LiveMeasuringStatus:      measuringStatus,
			LiveAvgStepTimeSec:       after.Status.AvgStepTimeSec,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
