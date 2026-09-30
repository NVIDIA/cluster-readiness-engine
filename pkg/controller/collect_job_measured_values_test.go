// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Measured values are provisional until their measurement completes. Goodput
// freezes in its terminal write (ADR-072); bandwidth completes from the Job's
// full log, and only a Complete=True/JobSucceeded measurement holds final
// results (issue #404). Evaluating earlier would make pass/fail depend on when
// the Job controller happened to read. These cases pin both gates.
func TestCollectJobMeasuredValues(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "collect-job-measured-values",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			GoodputComplete bool   `yaml:"goodputComplete"`
			Result          string `yaml:"result"`
			AvgTFLOPSPerGPU string `yaml:"avgTFLOPSPerGPU"`
			AvgStepTimeSec  string `yaml:"avgStepTimeSec"`
			BusBW           string `yaml:"busBW"`
			AlgBW           string `yaml:"algBW"`
			// BandwidthReason is the reason of the BandwidthMeasurement's
			// Complete=True condition; empty leaves it incomplete.
			BandwidthReason string `yaml:"bandwidthReason"`
			// BandwidthJobUID is the Job UID the BandwidthMeasurement was
			// created for; empty leaves it unannotated (created before the
			// annotation existed).
			BandwidthJobUID string `yaml:"bandwidthJobUID"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		jobRef := corev1.TypedLocalObjectReference{Name: "j"}
		gm := &nvcrev1alpha1.GoodputMeasurement{
			Name: "j-goodput", Namespace: "ns",
			Spec: nvcrev1alpha1.GoodputMeasurementSpec{JobRef: jobRef},
			Status: nvcrev1alpha1.GoodputMeasurementStatus{
				Result:          in.Result,
				AvgTFLOPSPerGPU: in.AvgTFLOPSPerGPU,
				AvgStepTimeSec:  in.AvgStepTimeSec,
			},
		}
		if in.GoodputComplete {
			gm.Status.Conditions = []metav1.Condition{{
				Type:               nvcrev1alpha1.GoodputMeasurementComplete,
				Status:             metav1.ConditionTrue,
				Reason:             reasonBandwidthJobSucceeded,
				LastTransitionTime: metav1.Now(),
			}}
		}
		bm := &nvcrev1alpha1.BandwidthMeasurement{
			Name: "j-bandwidth", Namespace: "ns",
			Spec: nvcrev1alpha1.BandwidthMeasurementSpec{JobRef: jobRef},
			Status: nvcrev1alpha1.BandwidthMeasurementStatus{
				Results: []nvcrev1alpha1.BandwidthResult{{
					SizeBytes: 1 << 30, BusBW: in.BusBW, AlgBW: in.AlgBW, Samples: 1,
				}},
			},
		}
		if in.BandwidthReason != "" {
			bm.Status.Conditions = []metav1.Condition{{
				Type:               nvcrev1alpha1.BandwidthMeasurementComplete,
				Status:             metav1.ConditionTrue,
				Reason:             in.BandwidthReason,
				LastTransitionTime: metav1.Now(),
			}}
		}
		if in.BandwidthJobUID != "" {
			bm.Annotations = map[string]string{annotationJobUID: in.BandwidthJobUID}
		}
		job := &nvcrev1alpha1.Job{Name: "j", Namespace: "ns", UID: currentJobUID}

		gmIndex := func(obj client.Object) []string {
			return []string{obj.(*nvcrev1alpha1.GoodputMeasurement).Spec.JobRef.Name}
		}
		bmIndex := func(obj client.Object) []string {
			return []string{obj.(*nvcrev1alpha1.BandwidthMeasurement).Spec.JobRef.Name}
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithIndex(&nvcrev1alpha1.GoodputMeasurement{}, measurementJobRefIndexField, gmIndex).
			WithIndex(&nvcrev1alpha1.BandwidthMeasurement{}, measurementJobRefIndexField, bmIndex).
			WithObjects(gm, bm, job).Build()

		values := collectJobMeasuredValues(context.Background(), c, job)

		b, err := json.MarshalIndent(values, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
