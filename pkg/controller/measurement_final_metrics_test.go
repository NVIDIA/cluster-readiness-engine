// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

const (
	finalMetricBandwidth    = "bandwidth"
	finalMetricGoodput      = "goodput"
	finalMetricNCCLTest     = "all_reduce"
	finalMetricWorkflowKind = "Workflow"
)

type finalMetricInput struct {
	Kind            string                                 `yaml:"kind"`
	Reason          string                                 `yaml:"reason"`
	LegacyOwner     bool                                   `yaml:"legacyOwner"`
	SeedProvisional bool                                   `yaml:"seedProvisional"`
	Results         []nvcrev1alpha1.BandwidthResult        `yaml:"results"`
	Goodput         nvcrev1alpha1.GoodputMeasurementStatus `yaml:"goodput"`
}

type finalMetricSample struct {
	Name     string  `json:"name"`
	Workflow string  `json:"workflow"`
	Size     string  `json:"size,omitempty"`
	Value    float64 `json:"value"`
}

// TestMeasurementFinalMetrics pins the whole terminal gauge lifecycle rather
// than the metric setters in isolation. Every fixture ends by deleting a
// measurement whose Job no longer exists, as happens between Workflow runs.
func TestMeasurementFinalMetrics(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "measurement-final-metrics", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in finalMetricInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		const ns, name, job, workflow = "final-metrics-ns", "measurement", "job", "workflow"
		clear := func() {
			labels := prometheus.Labels{labelNamespace: ns, labelMeasurement: name}
			for _, gauge := range finalMetricGauges() {
				gauge.DeletePartialMatch(labels)
			}
		}
		clear()
		defer clear()
		if in.SeedProvisional {
			// A stale workflow label must not evade cleanup, nor may provisional
			// sizes which the final log does not contain survive replacement.
			recordNCCLBandwidthMetrics(ns, name, job, "old-workflow", finalMetricNCCLTest, "8", 1, 2)
			recordNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, "1024", 3, 4)
			recordGoodputMetrics(ns, name, job, "old-workflow", goodputMetricValues{GoodputRatio: .1, TrainingTime: 9})
		}
		scheme := newWorkflowScheme(tc.T.(*testing.T))
		ctx := context.Background()
		var restore, remove func() error
		switch in.Kind {
		case finalMetricBandwidth:
			m := &nvcrev1alpha1.BandwidthMeasurement{
				Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow}, Finalizers: []string{bandwidthMeasurementFinalizer},
				Spec: nvcrev1alpha1.BandwidthMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
			r := &BandwidthMeasurementReconciler{Client: c}
			if _, err := r.finalizeTerminal(ctx, m, jobTerminalTime, terminalOutcome{reason: in.Reason, message: "terminal fixture", replace: true, results: in.Results}); err != nil {
				return err
			}
			frozen := m.Status.DeepCopy()
			restore = func() error {
				fresh := &nvcrev1alpha1.BandwidthMeasurement{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(m), fresh); err != nil {
					return err
				}
				restarted := &BandwidthMeasurementReconciler{Client: c}
				if _, err := restarted.reconcileMeasurement(ctx, fresh); err != nil {
					return err
				}
				if !reflect.DeepEqual(frozen, &fresh.Status) {
					return fmt.Errorf("bandwidth restart changed frozen status")
				}
				return nil
			}
			remove = func() error { _, err := r.handleDeletion(ctx, m); return err }
		case finalMetricGoodput:
			m := &nvcrev1alpha1.GoodputMeasurement{
				Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow}, Finalizers: []string{goodputMeasurementFinalizer},
				Spec:   nvcrev1alpha1.GoodputMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}},
				Status: in.Goodput,
			}
			if in.LegacyOwner {
				m.Labels = nil
				controller := true
				m.OwnerReferences = []metav1.OwnerReference{{APIVersion: nvcrev1alpha1.GroupVersion.String(), Kind: finalMetricWorkflowKind, Name: workflow, Controller: &controller}}
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
			r := &GoodputMeasurementReconciler{Client: c}
			if err := r.setComplete(ctx, m, jobTerminalTime, in.Reason, "terminal fixture"); err != nil {
				return err
			}
			frozen := m.Status.DeepCopy()
			restore = func() error {
				fresh := &nvcrev1alpha1.GoodputMeasurement{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(m), fresh); err != nil {
					return err
				}
				restarted := &GoodputMeasurementReconciler{Client: c}
				if _, err := restarted.reconcileMeasurement(ctx, fresh); err != nil {
					return err
				}
				if !reflect.DeepEqual(frozen, &fresh.Status) {
					return fmt.Errorf("goodput restart changed frozen status")
				}
				return nil
			}
			remove = func() error { _, err := r.handleDeletion(ctx, m); return err }
		default:
			return fmt.Errorf("unknown kind %q", in.Kind)
		}
		terminal, err := gatherFinalMetricSamples(ns, name, in.Kind)
		if err != nil {
			return err
		}
		clear() // A restarted manager has an empty metric registry.
		if err := restore(); err != nil {
			return err
		}
		recovered, err := gatherFinalMetricSamples(ns, name, in.Kind)
		if err != nil {
			return err
		}
		if err := remove(); err != nil {
			return err
		}
		deleted, err := gatherFinalMetricSamples(ns, name, in.Kind)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(struct {
			Terminal  []finalMetricSample `json:"terminal"`
			Recovered []finalMetricSample `json:"recovered"`
			Deleted   []finalMetricSample `json:"deleted"`
		}{terminal, recovered, deleted}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func finalMetricGauges() []*prometheus.GaugeVec {
	return []*prometheus.GaugeVec{
		ncclAlgBWGauge, ncclBusBWGauge, ncclAlgBWDeprecatedGauge, ncclBusBWDeprecatedGauge,
		goodputRatioGauge, goodputAvgTFLOPSGauge, goodputAvgStepTimeGauge,
		goodputRescheduleTimeGauge, goodputResumeTimeGauge, goodputCheckpointSaveTimeGauge,
		goodputLostWorkTimeGauge, goodputTrainingTimeGauge, goodputWarmupTimeGauge, goodputNonWarmupTimeGauge,
	}
}

func gatherFinalMetricSamples(namespace, measurement, kind string) ([]finalMetricSample, error) {
	reg := prometheus.NewRegistry()
	gauges := finalMetricGauges()
	if kind == finalMetricBandwidth {
		gauges = gauges[:4]
	} else {
		gauges = gauges[4:]
	}
	for _, gauge := range gauges {
		if err := reg.Register(gauge); err != nil {
			return nil, err
		}
	}
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	out := make([]finalMetricSample, 0)
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string)
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			if labels[labelNamespace] != namespace || labels[labelMeasurement] != measurement {
				continue
			}
			out = append(out, finalMetricSample{Name: family.GetName(), Workflow: labels[labelWorkflow], Size: labels["message_size_bytes"], Value: metric.GetGauge().GetValue()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Workflow != out[j].Workflow {
			return out[i].Workflow < out[j].Workflow
		}
		return out[i].Size < out[j].Size
	})
	return out, nil
}

// A failed status write must not publish values which readers cannot find in
// persisted status. These are single-value ordering guards, not snapshots.
func TestFinalMetricsWaitForStatusWrite(t *testing.T) {
	testFinalMetricsStatusWriteFailure(t, false)
}

func TestFinalMetricsCanceledStatusWrite(t *testing.T) {
	testFinalMetricsStatusWriteFailure(t, true)
}

func testFinalMetricsStatusWriteFailure(t *testing.T, canceled bool) {
	t.Helper()
	for _, kind := range []string{finalMetricBandwidth, finalMetricGoodput} {
		t.Run(kind, func(t *testing.T) {
			const ns, name, job, workflow = "final-write-fails", "measurement", "job", "workflow"
			ctx := context.Background()
			scheme := newWorkflowScheme(t)
			blocked := errors.New("status unavailable")
			if canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				blocked = context.Canceled
			}
			builder := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
					if canceled {
						return ctx.Err()
					}
					return blocked
				},
			})
			if kind == finalMetricBandwidth {
				m := &nvcrev1alpha1.BandwidthMeasurement{
					Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow},
					Spec: nvcrev1alpha1.BandwidthMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest},
				}
				recordNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, "1024", 1, 2)
				defer cleanupNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, []string{"1024"})
				c := builder.WithObjects(m).WithStatusSubresource(m).Build()
				r := &BandwidthMeasurementReconciler{Client: c}
				_, err := r.finalizeTerminal(ctx, m, jobTerminalTime, terminalOutcome{reason: reasonBandwidthJobSucceeded, replace: true, results: []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "80", BusBW: "70"}}})
				if !errors.Is(err, blocked) {
					t.Fatalf("got error %v, want status write failure", err)
				}
				if got := promtest.ToFloat64(ncclBusBWGauge.WithLabelValues(ns, name, job, workflow, finalMetricNCCLTest, "1024")); got != 2 {
					t.Fatalf("published final bandwidth before status persisted: %v", got)
				}
			} else {
				m := &nvcrev1alpha1.GoodputMeasurement{
					Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow},
					Spec:   nvcrev1alpha1.GoodputMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}},
					Status: nvcrev1alpha1.GoodputMeasurementStatus{Result: "0.75"},
				}
				recordGoodputMetrics(ns, name, job, workflow, goodputMetricValues{GoodputRatio: 0.1})
				defer cleanupGoodputMetrics(ns, name, job, workflow)
				c := builder.WithObjects(m).WithStatusSubresource(m).Build()
				r := &GoodputMeasurementReconciler{Client: c}
				err := r.setComplete(ctx, m, jobTerminalTime, "JobSucceeded", "fixture")
				if !errors.Is(err, blocked) {
					t.Fatalf("got error %v, want status write failure", err)
				}
				if got := promtest.ToFloat64(goodputRatioGauge.WithLabelValues(ns, name, job, workflow)); got != 0.1 {
					t.Fatalf("published final goodput before status persisted: %v", got)
				}
			}
		})
	}
}

// On conflict, the persisted first terminal writer wins. The candidate used
// before the conflict must not overwrite the winning values in Prometheus.
func TestFinalMetricsPublishConflictWinner(t *testing.T) {
	for _, kind := range []string{finalMetricBandwidth, finalMetricGoodput} {
		t.Run(kind, func(t *testing.T) {
			const ns, name, job, workflow = "final-conflict", "measurement", "job", "workflow"
			ctx := context.Background()
			scheme := newWorkflowScheme(t)
			builder := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return apierrors.NewConflict(schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: "measurements"}, name, errors.New("another terminal writer won"))
				},
			})
			if kind == finalMetricBandwidth {
				winner := &nvcrev1alpha1.BandwidthMeasurement{
					Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow},
					Spec:   nvcrev1alpha1.BandwidthMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest},
					Status: nvcrev1alpha1.BandwidthMeasurementStatus{Conditions: []metav1.Condition{{Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue, Reason: reasonBandwidthJobSucceeded}}, Results: []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "40", BusBW: "33"}}},
				}
				candidate := winner.DeepCopy()
				candidate.Status = nvcrev1alpha1.BandwidthMeasurementStatus{}
				c := builder.WithObjects(winner).WithStatusSubresource(winner).Build()
				r := &BandwidthMeasurementReconciler{Client: c}
				defer cleanupNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, []string{"1024"})
				_, err := r.finalizeTerminal(ctx, candidate, jobTerminalTime, terminalOutcome{reason: reasonBandwidthJobSucceeded, replace: true, results: []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "80", BusBW: "70"}}})
				if err != nil {
					t.Fatal(err)
				}
				if got := promtest.ToFloat64(ncclBusBWGauge.WithLabelValues(ns, name, job, workflow, finalMetricNCCLTest, "1024")); got != 33 {
					t.Fatalf("got %v, want persisted conflict winner 33", got)
				}
			} else {
				winner := &nvcrev1alpha1.GoodputMeasurement{
					Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow},
					Spec:   nvcrev1alpha1.GoodputMeasurementSpec{JobRef: corev1.TypedLocalObjectReference{Name: job}},
					Status: nvcrev1alpha1.GoodputMeasurementStatus{Result: "0.25", Conditions: []metav1.Condition{{Type: nvcrev1alpha1.GoodputMeasurementComplete, Status: metav1.ConditionTrue, Reason: "JobFailed"}}},
				}
				candidate := winner.DeepCopy()
				candidate.Status = nvcrev1alpha1.GoodputMeasurementStatus{Result: "0.75"}
				c := builder.WithObjects(winner).WithStatusSubresource(winner).Build()
				r := &GoodputMeasurementReconciler{Client: c}
				defer cleanupGoodputMetrics(ns, name, job, workflow)
				if err := r.setComplete(ctx, candidate, jobTerminalTime, "JobSucceeded", "fixture"); err != nil {
					t.Fatal(err)
				}
				if got := promtest.ToFloat64(goodputRatioGauge.WithLabelValues(ns, name, job, workflow)); got != 0.25 {
					t.Fatalf("got %v, want persisted conflict winner 0.25", got)
				}
			}
		})
	}
}
