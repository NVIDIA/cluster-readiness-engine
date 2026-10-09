// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type statusCollectorConfig struct {
	Elected  *bool `yaml:"elected"`
	NoSource bool  `yaml:"noSource"`
	Mutate   *struct {
		Delete []struct {
			Kind      string `yaml:"kind"`
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"delete"`
		SetLabels []struct {
			Kind      string            `yaml:"kind"`
			Name      string            `yaml:"name"`
			Namespace string            `yaml:"namespace"`
			Labels    map[string]string `yaml:"labels"`
		} `yaml:"setLabels"`
	} `yaml:"mutate"`
}

type statusMetricSample struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

func TestLifecycleStatusCollector(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "lifecycle-status-collector",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		scheme := newWorkflowScheme(tc.T.(*testing.T))
		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}

		var cfg statusCollectorConfig
		if raw, ok := tc.Inputs["input_config.yaml"]; ok {
			if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
				return err
			}
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		collector := newLifecycleStatusCollector()
		if !cfg.NoSource {
			collector.setSource(c, electedChannel(cfg.Elected == nil || *cfg.Elected))
		}

		before, err := gatherStatusSamples(collector)
		if err != nil {
			return err
		}
		if cfg.Mutate == nil {
			data, err := json.MarshalIndent(struct {
				Metrics []statusMetricSample `json:"metrics"`
			}{Metrics: before}, "", "  ")
			if err != nil {
				return err
			}
			tc.Actual = string(data) + "\n"
			return nil
		}

		if err := applyStatusCollectorMutate(context.Background(), c, cfg); err != nil {
			return err
		}
		after, err := gatherStatusSamples(collector)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(struct {
			Before []statusMetricSample `json:"before"`
			After  []statusMetricSample `json:"after"`
		}{Before: before, After: after}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func applyStatusCollectorMutate(ctx context.Context, c client.Client, cfg statusCollectorConfig) error {
	for _, del := range cfg.Mutate.Delete {
		obj, err := statusCollectorObject(del.Kind)
		if err != nil {
			return err
		}
		obj.SetName(del.Name)
		obj.SetNamespace(del.Namespace)
		if err := c.Delete(ctx, obj); err != nil {
			return fmt.Errorf("delete %s %s/%s: %w", del.Kind, del.Namespace, del.Name, err)
		}
	}
	for _, patch := range cfg.Mutate.SetLabels {
		obj, err := statusCollectorObject(patch.Kind)
		if err != nil {
			return err
		}
		key := client.ObjectKey{Name: patch.Name, Namespace: patch.Namespace}
		if err := c.Get(ctx, key, obj); err != nil {
			return fmt.Errorf("get %s %s/%s: %w", patch.Kind, patch.Namespace, patch.Name, err)
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		for k, v := range patch.Labels {
			if v == "" {
				delete(labels, k)
			} else {
				labels[k] = v
			}
		}
		obj.SetLabels(labels)
		if err := c.Update(ctx, obj); err != nil {
			return fmt.Errorf("update labels on %s %s/%s: %w", patch.Kind, patch.Namespace, patch.Name, err)
		}
	}
	return nil
}

func statusCollectorObject(kind string) (client.Object, error) {
	switch kind {
	case testKindCertification:
		return &nvcrev1alpha1.Certification{}, nil
	case "Workflow":
		return &nvcrev1alpha1.Workflow{}, nil
	case kindJob:
		return &nvcrev1alpha1.Job{}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

func electedChannel(elected bool) <-chan struct{} {
	ch := make(chan struct{})
	if elected {
		close(ch)
	}
	return ch
}

func gatherStatusSamples(c prometheus.Collector) ([]statusMetricSample, error) {
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return nil, err
	}
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	samples := make([]statusMetricSample, 0)
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, lp := range metric.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			samples = append(samples, statusMetricSample{
				Name:   family.GetName(),
				Labels: labels,
				Value:  metric.GetGauge().GetValue(),
			})
		}
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Name != samples[j].Name {
			return samples[i].Name < samples[j].Name
		}
		return canonicalLabels(samples[i].Labels) < canonicalLabels(samples[j].Labels)
	})
	return samples, nil
}

func canonicalLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}
