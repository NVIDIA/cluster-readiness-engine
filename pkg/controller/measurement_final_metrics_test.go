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
	finalMetricBandwidth = "bandwidth"
	finalMetricNCCLTest  = "all_reduce"
)

type finalMetricInput struct {
	Kind            string                          `json:"kind" yaml:"kind"`
	Reason          string                          `json:"reason" yaml:"reason"`
	SeedProvisional bool                            `json:"seedProvisional" yaml:"seedProvisional"`
	Replace         bool                            `json:"replace" yaml:"replace"`
	Results         []nvcrev1alpha1.BandwidthResult `json:"results" yaml:"results"`
}

type finalMetricSample struct {
	Name     string  `json:"name"`
	Workflow string  `json:"workflow"`
	Size     string  `json:"size,omitempty"`
	Value    float64 `json:"value"`
}

// TestMeasurementFinalMetrics pins terminal gauge replacement, restart
// recovery and deletion. The no-data and unavailable-log cases start with
// provisional status and registry values, then assert that neither is exposed
// as a final measurement.
func TestMeasurementFinalMetrics(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "measurement-final-metrics", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in finalMetricInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		const ns, name, job, workflow = "final-metrics-ns", "measurement", "job", "workflow"
		clear := func() {
			cleanupMeasurementBandwidthMetrics(ns, name)
		}
		clear()
		defer clear()

		var provisional []nvcrev1alpha1.BandwidthResult
		if in.SeedProvisional {
			provisional = []nvcrev1alpha1.BandwidthResult{
				{SizeBytes: 8, AlgBW: "1", BusBW: "2", Samples: 1},
				{SizeBytes: 1024, AlgBW: "3", BusBW: "4", Samples: 1},
			}
			// One old label and one current label prove cleanup uses measurement
			// identity and removes provisional-only sizes before publishing finals.
			recordNCCLBandwidthMetrics(ns, name, job, "old-workflow", finalMetricNCCLTest, "8", 1, 2)
			recordNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, "1024", 3, 4)
		}
		scheme := newWorkflowScheme(tc.T.(*testing.T))
		ctx := context.Background()
		var frozen any
		var restore func() error
		var remove func() error
		switch in.Kind {
		case finalMetricBandwidth:
			m := &nvcrev1alpha1.BandwidthMeasurement{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: ns,
					Labels:     map[string]string{labelWorkflowTracking: workflow},
					Finalizers: []string{bandwidthMeasurementFinalizer},
				},
				Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
					JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest,
				},
			}
			if in.SeedProvisional {
				m.Status.Results = provisional
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(m).WithStatusSubresource(m).Build()
			r := &BandwidthMeasurementReconciler{Client: c}
			if _, err := r.finalizeTerminal(ctx, m, jobTerminalTime, terminalOutcome{
				reason: in.Reason, message: "terminal fixture", replace: in.Replace, results: in.Results,
			}); err != nil {
				return err
			}
			if in.Reason == reasonBandwidthLogsUnavailable && !reflect.DeepEqual(m.Status.Results, provisional) {
				return fmt.Errorf("unavailable logs changed provisional status results")
			}
			if in.Reason == reasonBandwidthNoData && len(m.Status.Results) != 0 {
				return fmt.Errorf("no-data outcome retained provisional status results")
			}
			frozen = m.Status.DeepCopy()
			restore = func() error {
				fresh := &nvcrev1alpha1.BandwidthMeasurement{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(m), fresh); err != nil {
					return err
				}
				if _, err := (&BandwidthMeasurementReconciler{Client: c}).reconcileMeasurement(ctx, fresh); err != nil {
					return err
				}
				if !reflect.DeepEqual(frozen, fresh.Status.DeepCopy()) {
					return fmt.Errorf("bandwidth restart changed frozen status")
				}
				return nil
			}
			remove = func() error { _, err := r.handleDeletion(ctx, m); return err }
		default:
			return fmt.Errorf("unknown measurement kind %q", in.Kind)
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

func gatherFinalMetricSamples(namespace, measurement, kind string) ([]finalMetricSample, error) {
	reg := prometheus.NewRegistry()
	if kind == finalMetricBandwidth {
		for _, set := range ncclBandwidthGaugeSets {
			if err := reg.Register(set.algBW); err != nil {
				return nil, err
			}
			if err := reg.Register(set.busBW); err != nil {
				return nil, err
			}
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
			out = append(out, finalMetricSample{
				Name: family.GetName(), Workflow: labels[labelWorkflow], Size: labels["message_size_bytes"],
				Value: metric.GetGauge().GetValue(),
			})
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

func TestFinalBandwidthMetricsWaitForStatusWrite(t *testing.T) {
	testFinalBandwidthMetricsStatusWriteFailure(t, false)
}

func TestFinalBandwidthMetricsCanceledStatusWrite(t *testing.T) {
	testFinalBandwidthMetricsStatusWriteFailure(t, true)
}

func testFinalBandwidthMetricsStatusWriteFailure(t *testing.T, canceled bool) {
	t.Helper()
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
	m := &nvcrev1alpha1.BandwidthMeasurement{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow}},
		Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest,
		},
	}
	recordNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, "1024", 1, 2)
	defer cleanupNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, []string{"1024"})
	c := builder.WithObjects(m).WithStatusSubresource(m).Build()
	r := &BandwidthMeasurementReconciler{Client: c}
	_, err := r.finalizeTerminal(ctx, m, jobTerminalTime, terminalOutcome{
		reason: reasonBandwidthJobSucceeded, replace: true,
		results: []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "80", BusBW: "70"}},
	})
	if !errors.Is(err, blocked) {
		t.Fatalf("got error %v, want status write failure", err)
	}
	if got := promtest.ToFloat64(ncclBusBWGauge.WithLabelValues(ns, name, job, workflow, finalMetricNCCLTest, "1024")); got != 2 {
		t.Fatalf("published final bandwidth before status persisted: %v", got)
	}
}

func TestFinalBandwidthMetricsPublishConflictWinner(t *testing.T) {
	const ns, name, job, workflow = "final-conflict", "measurement", "job", "workflow"
	ctx := context.Background()
	scheme := newWorkflowScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: nvcrev1alpha1.GroupVersion.Group, Resource: "measurements"}, name, errors.New("another terminal writer won"))
		},
	})
	winner := &nvcrev1alpha1.BandwidthMeasurement{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{labelWorkflowTracking: workflow}},
		Spec: nvcrev1alpha1.BandwidthMeasurementSpec{
			JobRef: corev1.TypedLocalObjectReference{Name: job}, TestType: finalMetricNCCLTest,
		},
		Status: nvcrev1alpha1.BandwidthMeasurementStatus{
			Conditions: []metav1.Condition{{Type: nvcrev1alpha1.BandwidthMeasurementComplete, Status: metav1.ConditionTrue, Reason: reasonBandwidthJobSucceeded}},
			Results:    []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "40", BusBW: "33"}},
		},
	}
	candidate := winner.DeepCopy()
	candidate.Status = nvcrev1alpha1.BandwidthMeasurementStatus{}
	c := builder.WithObjects(winner).WithStatusSubresource(winner).Build()
	r := &BandwidthMeasurementReconciler{Client: c}
	defer cleanupNCCLBandwidthMetrics(ns, name, job, workflow, finalMetricNCCLTest, []string{"1024"})
	_, err := r.finalizeTerminal(ctx, candidate, jobTerminalTime, terminalOutcome{
		reason: reasonBandwidthJobSucceeded, replace: true,
		results: []nvcrev1alpha1.BandwidthResult{{SizeBytes: 1024, AlgBW: "80", BusBW: "70"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := promtest.ToFloat64(ncclBusBWGauge.WithLabelValues(ns, name, job, workflow, finalMetricNCCLTest, "1024")); got != 33 {
		t.Fatalf("got %v, want persisted conflict winner 33", got)
	}
}
