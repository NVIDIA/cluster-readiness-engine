// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// ResolveCertification is the half of the egress path that talks to the
// cluster: it resolves each category's shards by label selector, gunzips them,
// and checks what it found against what the run sealed.
//
// The shards here are real. Each case builds them through pkg/shard's own
// encoder, so the bytes under test are the bytes the controller writes, and a
// change to the encoding that broke the reader would fail here rather than on
// a customer's bundle.
//
// The completeness check is what most of these cases are about. A bundle short
// of its rows would still carry a manifest certifying it as whole, so a
// mismatch between the resolved and sealed counts has to refuse the write
// outright. ADR-090 is explicit that this must not follow FailedNodesFromRef,
// which returns nil on a missing reference, a failed read and a decode error
// alike, making an unreadable ConfigMap indistinguishable from nothing to
// report.
//
// One limit worth stating: the fake client does not implement List pagination,
// so these cases exercise the walk's decode, ordering and accounting but not
// its Continue handling. Paging matters at a thousand shards and is not
// reachable in a unit test.
func TestResolveCertification(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Namespace  string `yaml:"namespace"`
			Categories []struct {
				Domain  string `yaml:"domain"`
				Variant string `yaml:"variant"`
				// WorkflowMissing drops the Workflow object, so its namespace
				// cannot be resolved.
				WorkflowMissing bool `yaml:"workflowMissing"`
				// NoWorkflowRef leaves the category without a reference while
				// still sealing counts.
				NoWorkflowRef bool `yaml:"noWorkflowRef"`
				// WorkflowNamespace overrides where this category's shards
				// live, for the two-namespaces case.
				WorkflowNamespace string `yaml:"workflowNamespace"`
				// Selector overrides the sealed selector, for a malformed one.
				Selector string `yaml:"selector"`
				// Rows are the rows actually written into shards.
				Rows []string `yaml:"rows"`
				// SealShards and SealRows override the sealed counts, so a
				// case can seal more than it stored.
				SealShards *int32 `yaml:"sealShards"`
				SealRows   *int64 `yaml:"sealRows"`
				Final      bool   `yaml:"final"`
				// DroppedRows writes a truncation sibling.
				DroppedRows int64 `yaml:"droppedRows"`
			} `yaml:"categories"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		if in.Namespace == "" {
			in.Namespace = "nvcre"
		}

		scheme := runtime.NewScheme()
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return err
		}

		cert := &nvcrev1alpha1.Certification{Name: "gpu-cluster-cert", Namespace: in.Namespace}
		objs := []client.Object{}

		for i, cat := range in.Categories {
			wfName := fmt.Sprintf("wf-%d", i)
			wfUID := fmt.Sprintf("uid-%04d", i)
			shardNS := in.Namespace
			if cat.WorkflowNamespace != "" {
				shardNS = cat.WorkflowNamespace
			}

			built, err := buildShards(cat.Rows, cat.DroppedRows)
			if err != nil {
				return err
			}
			for _, s := range built.Shards {
				cm := &corev1.ConfigMap{
					Name:      fmt.Sprintf("shard-%s-job-%04d", wfUID, s.Index),
					Namespace: shardNS,
					Labels:    map[string]string{labelWorkflowUID: wfUID},
				}
				cm.Data = s.Data()
				cm.BinaryData = s.BinaryData()
				objs = append(objs, cm)
			}
			if payload, err := shard.EncodeTruncation(built.Dropped); err != nil {
				return err
			} else if payload != nil {
				objs = append(objs, &corev1.ConfigMap{
					Name:       fmt.Sprintf("shard-%s-job-truncation", wfUID),
					Namespace:  shardNS,
					Labels:     map[string]string{labelWorkflowUID: wfUID},
					BinaryData: map[string][]byte{shard.TruncationKey: payload},
				})
			}

			selector := labelWorkflowUID + "=" + wfUID
			if cat.Selector != "" {
				selector = cat.Selector
			}
			sealShards := int32(len(built.Shards)) //nolint:gosec // bounded by the case fixture
			if cat.SealShards != nil {
				sealShards = *cat.SealShards
			}
			sealRows := built.Rows
			if cat.SealRows != nil {
				sealRows = *cat.SealRows
			}

			cs := nvcrev1alpha1.CertificationCategoryStatus{
				Domain:  cat.Domain,
				Variant: cat.Variant,
				ComponentResults: &nvcrev1alpha1.WorkflowComponentResults{
					ShardCount:        sealShards,
					RowCount:          sealRows,
					Selector:          selector,
					Final:             cat.Final,
					TruncatedRowCount: built.DroppedTotal,
					Truncated:         built.DroppedTotal > 0,
				},
			}
			if !cat.NoWorkflowRef {
				cs.WorkflowRef = &nvcrev1alpha1.WorkflowReference{
					Name: wfName, Namespace: in.Namespace,
				}
			}
			cert.Status.CategoryStatuses = append(cert.Status.CategoryStatuses, cs)

			if !cat.WorkflowMissing {
				wf := &nvcrev1alpha1.Workflow{Name: wfName, Namespace: in.Namespace}
				wf.UID = types.UID(wfUID)
				wf.Status.Namespace = shardNS
				objs = append(objs, wf)
			}
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

		comps, err := ResolveCertification(context.Background(), c, cert)

		out := struct {
			Error      string             `json:"error,omitempty"`
			Components []resolvedReport   `json:"components"`
			Truncs     []TruncationRecord `json:"truncations,omitempty"`
		}{Components: []resolvedReport{}}
		if err != nil {
			out.Error = err.Error()
		}
		for _, comp := range comps {
			out.Components = append(out.Components, resolvedReport{
				FileName:          comp.FileName(),
				ShardCount:        comp.ShardCount,
				RowCount:          comp.RowCount,
				Final:             comp.Final,
				TruncatedRowCount: comp.TruncatedRowCount,
				Rows:              splitLines(comp.NDJSON),
			})
		}
		out.Truncs = CollectTruncations(comps)

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// labelWorkflowUID mirrors the label the controller stamps on a shard. Spelled
// out rather than imported, because pkg/controller imports this package's
// sibling and a test dependency the other way would invert the layering.
const labelWorkflowUID = "nvcre.nvidia.com/workflow-uid"

type resolvedReport struct {
	FileName          string   `json:"fileName"`
	ShardCount        int32    `json:"shardCount"`
	RowCount          int64    `json:"rowCount"`
	Final             bool     `json:"final"`
	TruncatedRowCount int64    `json:"truncatedRowCount,omitempty"`
	Rows              []string `json:"rows"`
}

// buildShards encodes the case's rows through the real encoder, and optionally
// forces rows to be dropped so a truncation sibling exists.
func buildShards(rows []string, dropped int64) (shard.Result, error) {
	b, err := shard.NewBuilder(shard.Options{})
	if err != nil {
		return shard.Result{}, err
	}
	for _, line := range rows {
		var r shard.Row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return shard.Result{}, fmt.Errorf("fixture row %q: %w", line, err)
		}
		if err := b.Add(r); err != nil {
			return shard.Result{}, err
		}
	}
	// A dropped row is forced by adding one the encoder cannot represent: a
	// NaN value is unencodable, which is exactly how a real producer's bad
	// value is accounted for.
	for range dropped {
		if err := b.Add(shard.Row{
			Node: "gpu-node-9", ComponentID: "GPU-dead", Name: "achievedTFLOPS",
			Value: math.NaN(),
		}); err != nil {
			return shard.Result{}, err
		}
	}
	return b.Build()
}

func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, "\n")
}
