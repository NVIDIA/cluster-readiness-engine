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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// handleSucceeded and handleFailed build from the stored status and never read
// logs again, so everything written in the last sampling window was lost. A
// script that logged to iteration 100 was recorded as reaching step 90.
//
// handleRunning throttles reads so status updates do not re-trigger one, and
// the final read lands moments after the previous sample. finalSample therefore
// has to clear the sample time first, or the fix does nothing. These two cases
// pin both halves.
func TestGoodputFinalSample(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "goodput-final-sample",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Call              string `yaml:"call"`
			SampledSecondsAgo int    `yaml:"sampledSecondsAgo"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := crev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		m := &crev1alpha1.GoodputMeasurement{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "ns"},
			Spec: crev1alpha1.GoodputMeasurementSpec{
				JobRef:        corev1.TypedLocalObjectReference{Name: "j"},
				LogProfileRef: "does-not-resolve",
			},
		}
		job := &crev1alpha1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"}}

		c := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(m, job).WithStatusSubresource(m, job).Build()
		r := &GoodputMeasurementReconciler{Client: c, Scheme: scheme}

		key := "ns/m"
		r.mu.Lock()
		r.lastSample = map[string]time.Time{
			key: time.Now().Add(-time.Duration(in.SampledSecondsAgo) * time.Second),
		}
		r.mu.Unlock()

		requeued := false
		if in.Call == "finalSample" {
			r.finalSample(context.Background(), m, job)
		} else {
			res, err := r.handleRunning(context.Background(), m, job)
			if err != nil {
				return err
			}
			requeued = res.RequeueAfter > 0
		}

		r.mu.Lock()
		_, stillThrottled := r.lastSample[key]
		r.mu.Unlock()

		b, err := json.MarshalIndent(struct {
			ThrottleCleared bool `json:"throttleCleared"`
			AskedForRequeue bool `json:"askedForRequeue"`
		}{!stillThrottled, requeued}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
