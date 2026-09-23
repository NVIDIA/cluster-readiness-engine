// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// labelStampingAdapter wraps the real TrainJob adapter and stamps labels on the
// object it builds. Today's TrainJobAdapter.Build sets only name and namespace,
// so without a stand-in for an adapter that sets object labels of its own the
// collision rules in BuildObject could not be exercised at all.
type labelStampingAdapter struct {
	*TrainJobAdapter
	labels map[string]string
}

func (a labelStampingAdapter) Build(
	name, namespace string, spec *nvcrev1alpha1.WorkloadSpec,
) (client.Object, error) {
	obj, err := a.TrainJobAdapter.Build(name, namespace, spec)
	if err != nil {
		return nil, err
	}
	if len(a.labels) > 0 {
		obj.SetLabels(maps.Clone(a.labels))
	}
	return obj, nil
}

// labelBounds generates the label shapes whose point is their size. Writing 33
// labels or a 254-character key prefix out by hand would bury the one number
// each case is about, so the cases name the number instead. It mirrors
// generateNodes in the integration configs.
type labelBounds struct {
	// LabelCount adds this many distinct filler labels, for the maxProperties
	// cap at 32 and 33.
	LabelCount int `json:"labelCount,omitempty"`
	// ValueLength adds one label whose value is this long, for the 63- and
	// 64-character label value limit.
	ValueLength int `json:"valueLength,omitempty"`
	// KeyPrefixLength adds one label whose DNS-subdomain prefix is this long,
	// for the 253- and 254-character prefix limit.
	KeyPrefixLength int `json:"keyPrefixLength,omitempty"`
	// KeyNameLength adds one label whose name segment is this long, for the
	// 63- and 64-character name limit.
	KeyNameLength int `json:"keyNameLength,omitempty"`
}

func (b labelBounds) apply(labels map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, labels)
	for i := range b.LabelCount {
		out[fmt.Sprintf("filler-%04d", i)] = "x"
	}
	if b.ValueLength > 0 {
		out["long-value"] = strings.Repeat("v", b.ValueLength)
	}
	if b.KeyPrefixLength > 0 {
		out[strings.Repeat("p", b.KeyPrefixLength)+"/name"] = "x"
	}
	if b.KeyNameLength > 0 {
		out[strings.Repeat("n", b.KeyNameLength)] = "x"
	}
	return out
}

// buildObjectInput is the shape of each build-object case's input.yaml.
type buildObjectInput struct {
	// Workload is the discriminated union the adapter builds from.
	Workload nvcrev1alpha1.WorkloadSpec `json:"workload"`
	// Labels are the requested workloadMetadata labels. Omitting the key
	// entirely models a Job with no workloadMetadata at all, which must build
	// exactly as it did before this field existed.
	Labels map[string]string `json:"labels,omitempty"`
	// OmitMetadata forces a nil *WorkloadMetadata even when labels are given,
	// so the absent-metadata path is reachable explicitly.
	OmitMetadata bool `json:"omitMetadata,omitempty"`
	// AdapterLabels are stamped by the stub adapter before the merge.
	AdapterLabels map[string]string `json:"adapterLabels,omitempty"`
	Bounds        labelBounds       `json:"bounds,omitzero"`
}

// buildObjectResult is the golden view. Labels is the built object's whole
// label map, so a case can tell "requested label added" apart from "label map
// replaced", which is what would happen if the merge assigned a fresh map over
// the adapter's. SourcesUnchanged pins that neither the requested map nor the
// adapter's map was mutated in place.
type buildObjectResult struct {
	Error            string            `json:"error,omitempty"`
	Labels           map[string]string `json:"labels"`
	SourcesUnchanged bool              `json:"sourcesUnchanged"`
}

// TestBuildObject drives the one construction seam the Job controller and
// dry-run rendering share. The cases cover the labels the documented
// integrations need on the submitted object (Kueue's
// kueue.x-k8s.io/queue-name and KAI's kai.scheduler/queue), the
// insert/same/conflict rule against adapter-produced labels, the
// controller-owned keys that may not be set, and the syntax and size bounds
// the CRD schema enforces at admission and this mirrors for offline render.
func TestBuildObject(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "build-object",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in buildObjectInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		requested := in.Bounds.apply(in.Labels)
		var md *nvcrev1alpha1.WorkloadMetadata
		if !in.OmitMetadata && len(requested) > 0 {
			md = MetadataFrom(requested)
		}

		requestedBefore := maps.Clone(requested)
		adapterBefore := maps.Clone(in.AdapterLabels)

		adapter := labelStampingAdapter{
			TrainJobAdapter: &TrainJobAdapter{},
			labels:          in.AdapterLabels,
		}

		result := buildObjectResult{}
		obj, err := BuildObject(adapter, "wl", "ns", &in.Workload, md)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Labels = obj.GetLabels()
		}
		result.SourcesUnchanged = maps.Equal(requested, requestedBefore) &&
			maps.Equal(in.AdapterLabels, adapterBefore)

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// labelCompositionInput is the shape of each label-composition case. It drives
// the three map helpers together, because that is how the Certification and
// WorkloadRun paths use them: merge the levels, then insert the derived queue
// label under the insert/same/conflict rule.
type labelCompositionInput struct {
	Base    map[string]string `json:"base,omitempty"`
	Overlay map[string]string `json:"overlay,omitempty"`
	// InsertKey and InsertValue model the gang-scheduler queue invariant.
	InsertKey   string      `json:"insertKey,omitempty"`
	InsertValue string      `json:"insertValue,omitempty"`
	Bounds      labelBounds `json:"bounds,omitzero"`
}

// labelCompositionResult records the composed map, whether validation accepted
// it, and that neither input map was written through. Merged is nil (JSON null)
// when the composition produced nothing, which is what keeps an unconfigured
// path rendering with no workloadMetadata at all rather than an empty object.
type labelCompositionResult struct {
	Merged           map[string]string `json:"merged"`
	MetadataNil      bool              `json:"metadataNil"`
	InsertError      string            `json:"insertError,omitempty"`
	ValidationError  string            `json:"validationError,omitempty"`
	SourcesUnchanged bool              `json:"sourcesUnchanged"`
}

// TestLabelComposition covers precedence between metadata levels, the absence
// of any deletion syntax (an empty string is a value, not a removal), the
// clone semantics that keep a composition from writing back into the API
// object it read, and the syntax and size bounds. The bounds cases are the Go
// mirror of the CRD schema: admission covers everything arriving through the
// API, but offline `nvcrectl render` never reaches an API server and a map
// composed from several levels is not the map any single object was admitted
// with.
func TestLabelComposition(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "label-composition",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in labelCompositionInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		base := in.Bounds.apply(in.Base)
		if len(in.Base) == 0 && in.Bounds == (labelBounds{}) {
			base = nil
		}
		baseBefore := maps.Clone(base)
		overlayBefore := maps.Clone(in.Overlay)

		result := labelCompositionResult{}
		merged := MergeLabels(base, in.Overlay)

		if in.InsertKey != "" {
			inserted, err := InsertConsistentLabel(
				merged, in.InsertKey, in.InsertValue, "labels")
			if err != nil {
				result.InsertError = err.Error()
			} else {
				merged = inserted
			}
		}

		if result.InsertError == "" {
			if err := ValidateLabels(merged, "labels"); err != nil {
				result.ValidationError = err.Error()
			}
		}

		result.Merged = merged
		result.MetadataNil = MetadataFrom(merged) == nil
		result.SourcesUnchanged = maps.Equal(base, baseBefore) &&
			maps.Equal(in.Overlay, overlayBefore)

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestLabelsOfCopies pins that LabelsOf hands back a copy rather than the API
// object's own map. It is a single-value invariant with no structured output
// worth snapshotting: if it ever regressed, every merge in the Certification
// and WorkloadRun paths would start editing the resource it read from.
func TestLabelsOfCopies(t *testing.T) {
	md := &nvcrev1alpha1.WorkloadMetadata{
		Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{"team": "infra"},
	}

	got := LabelsOf(md)
	got["team"] = "mutated"
	got["added"] = "x"

	if md.Labels["team"] != "infra" {
		t.Errorf("LabelsOf result aliases the source: team = %q, want %q",
			md.Labels["team"], "infra")
	}
	if _, present := md.Labels["added"]; present {
		t.Error("LabelsOf result aliases the source: writing a new key reached the source map")
	}
	if LabelsOf(nil) != nil {
		t.Error("LabelsOf(nil) = non-nil, want nil so an absent field stays absent")
	}
	if LabelsOf(&nvcrev1alpha1.WorkloadMetadata{}) != nil {
		t.Error("LabelsOf(empty) = non-nil, want nil so an empty map stays absent")
	}
}
