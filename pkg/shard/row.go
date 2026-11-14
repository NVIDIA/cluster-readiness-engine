// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shard encodes per-component result rows into gzipped NDJSON shards
// sized to fit inside a ConfigMap (ADR-090).
//
// It is deliberately free of any Kubernetes client dependency. The controller
// writes shards and nvcrectl reads them, so the schema and the encoding live
// where both can reach them, the same split pkg/noderesults already uses for
// the node-result ConfigMaps.
package shard

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
)

// valuePrecision is the number of decimal places a row's value carries.
//
// ADR-090 fixes it at two. numstr.Format is deliberately not reused: it emits
// six places for status strings, and a row's byte form has to match the figure
// the record states, because the bundle's per-file sha256 is computed over
// these bytes.
const valuePrecision = 2

// Row is one measurement of one metric on one component at one sample.
//
// Field order is the wire order and is load bearing: encoding/json emits
// struct fields in declaration order, and the bundle manifest's sha256 is
// taken over the bytes this produces.
type Row struct {
	// Node is the Kubernetes node name the component sits in.
	Node string `json:"node"`
	// ComponentID identifies the component within the node. For a GPU it is
	// the UUID, which is stable across reboots and renumbering.
	ComponentID string `json:"componentID"`
	// Index is the component's device index within the node. It addresses a
	// row to a component and is NOT a sample counter; see Sample (ADR-091).
	Index int32 `json:"index"`
	// Name is the metric name.
	Name string `json:"name"`
	// Value is the measurement, carried to valuePrecision decimal places.
	Value float64 `json:"value"`
	// Unit is the measurement's unit.
	Unit string `json:"unit"`
	// Sample is a monotonic ordinal starting at zero, scoped to one Job
	// attempt. Every component on the node shares the ordinal for a given
	// sweep, which is what makes "component 3 fell behind at sample 900 while
	// the others held" answerable from the rows (ADR-091).
	Sample int64 `json:"sample"`
	// Timestamp is the wall clock time of the sample, RFC 3339.
	Timestamp string `json:"timestamp"`
	// Outcome is the producer's verdict for this row.
	Outcome string `json:"outcome"`
}

// rowWire mirrors Row with the value pre-formatted, so the encoded digits are
// fixed rather than whatever shortest-round-trip form json.Marshal picks for a
// float64. Without this, 1500.50 encodes as 1500.5 and a row's byte form
// depends on the magnitude of its value.
type rowWire struct {
	Node        string          `json:"node"`
	ComponentID string          `json:"componentID"`
	Index       int32           `json:"index"`
	Name        string          `json:"name"`
	Value       json.RawMessage `json:"value"`
	Unit        string          `json:"unit"`
	Sample      int64           `json:"sample"`
	Timestamp   string          `json:"timestamp"`
	Outcome     string          `json:"outcome"`
}

// formatValue renders a value as the fixed-precision decimal the row carries.
//
// It is not valid JSON for a non-finite value: JSON has no literal for NaN or
// an infinity, and FormatFloat spells them "NaN", "+Inf" and "-Inf". Builder.Add
// drops such a row before it reaches here. A caller marshalling a Row directly
// gets an error from the encoder, which is the right answer for a path that
// has no truncation ledger to record the loss in.
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', valuePrecision, 64)
}

// MarshalJSON encodes the row with a fixed-precision value.
func (r Row) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(rowWire{
		Node:        r.Node,
		ComponentID: r.ComponentID,
		Index:       r.Index,
		Name:        r.Name,
		Value:       json.RawMessage(formatValue(r.Value)),
		Unit:        r.Unit,
		Sample:      r.Sample,
		Timestamp:   r.Timestamp,
		Outcome:     r.Outcome,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal result row: %w", err)
	}
	return b, nil
}

// UnmarshalJSON decodes a row written by MarshalJSON. The alias avoids
// recursing back into Row's own marshaller.
func (r *Row) UnmarshalJSON(b []byte) error {
	type plain Row
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("unmarshal result row: %w", err)
	}
	*r = Row(p)
	return nil
}

// rowCompare orders rows for emission.
//
// ADR-090 makes (node, componentID, index, name) normative, on the grounds
// that it compresses multi-node shards and makes shard bytes deterministic for
// the same input. Determinism is the claim that needs care, because the
// bundle manifest's per-file sha256 rests on it and a retried shard write
// depends on rewriting byte-identical content.
//
// Those four fields are nowhere near a total order. A 30 minute soak at 1 Hz
// produces 1,800 rows sharing all four, so sample is appended, which also
// compresses slightly better because the fastest varying field then sorts
// last. That is still not enough: slices.SortFunc is not stable, so any two
// rows equal on all five keys but differing in payload make the output depend
// on the order the producer happened to emit them. That is not hypothetical
// for a producer that leaves sample at zero, or for rows gathered from more
// than one source.
//
// This comparison stops at the normative key. Build breaks any remaining tie
// with bytes.Compare on the two rows' encoded lines, which is strictly better
// than continuing through the fields here. Comparing fields is a proxy for the
// bytes and the proxy leaked twice: a numeric value comparison ties -0.0 with
// +0.0 while the encoder renders "-0.00" and "0.00", and formatting the value
// to compare it allocated two strings per comparison across roughly 920,000
// comparisons. Comparing the stored bytes is exact, allocation-free, total by
// construction, and stays correct if a field is ever added to Row.
func rowCompare(a, b Row) int {
	if c := cmp.Compare(a.Node, b.Node); c != 0 {
		return c
	}
	if c := cmp.Compare(a.ComponentID, b.ComponentID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Index, b.Index); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return cmp.Compare(a.Sample, b.Sample)
}
