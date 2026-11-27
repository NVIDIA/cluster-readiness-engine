// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
)

// rowProducer turns one pod's log lines into per-component result rows.
//
// ADR-090 settles where rows are stored and how they leave the cluster, and
// defers "the declaration vocabulary, the ingestion controller, and the verdict
// rules" to a later record. This interface is the minimum that record's absence
// still requires: something has to get rows out of a workload, and pod stdout
// is the only channel a workload has.
//
// Selection is by name from Job.spec.componentResults.producer, which is
// deliberately the weaker of the two patterns this repository uses. Workload
// adapters and node-health detectors are chosen by which typed field is set,
// precisely so a typo cannot silently select nothing. A name is used here
// because the follow-on record replaces this with a declaration an operator
// writes in the spec, and inventing a typed field per producer now would be
// vocabulary that record has to deprecate. Until then an unknown name is a
// hard error rather than a silent no-op.
type rowProducer interface {
	// Produce parses lines into rows. The node name is supplied by the caller
	// because a harness cannot reliably know the Kubernetes node it runs on,
	// and the row's node field is what makes a result addressable to hardware.
	//
	// An error means the producer could not run at all. A line it could not
	// read is reported in the result instead, never as an error: see
	// produceResult.Rejected.
	Produce(node string, lines []string) (produceResult, error)
}

// produceResult is one pod's worth of parsed rows, plus an account of the
// lines that declared a row and could not be read.
//
// Rejected lines are counted rather than fatal, and that is the ADR-090 rule
// about pagination applied to parsing: a result set is never collapsed to zero
// rows by a loss it can describe instead. The decisive case is mundane — a pod
// killed mid-write leaves a truncated final line, which is the likeliest
// malformed row there is — and discarding the set on one of those would throw
// away a thirty-minute soak because its last line was cut short. The count and
// the first offending line go into the Job's seal, so the loss is visible
// without the surviving rows being lost with it.
type produceResult struct {
	Rows     []shard.Row
	Rejected int64
	// FirstReject describes the first line that was rejected, for the status
	// message. Only the first: the reason is almost always the same for all of
	// them, and the seal's message field is bounded.
	FirstReject string
}

// add folds one pod's result into another, keeping the first reject seen.
func (p *produceResult) add(other produceResult) {
	p.Rows = append(p.Rows, other.Rows...)
	p.Rejected += other.Rejected
	if p.FirstReject == "" {
		p.FirstReject = other.FirstReject
	}
}

// rowProducers maps a producer name to its implementation.
//
// Deliberately a small explicit map rather than an init()-registration
// mechanism like pkg/catalog's: there is one producer, the follow-on record
// changes how producers are selected, and a registration side effect would be
// harder to delete than a map entry.
var rowProducers = map[string]rowProducer{
	producerStructuredRows: structuredRowProducer{},
}

// producerStructuredRows names the producer that reads the marker-prefixed
// JSON lines described on structuredRowProducer.
const producerStructuredRows = "structured-rows"

// rowProducerFor returns the producer a Job declares.
func rowProducerFor(spec *nvcrev1alpha1.ComponentResultSpec) (rowProducer, error) {
	p, ok := rowProducers[spec.Producer]
	if !ok {
		known := make([]string, 0, len(rowProducers))
		for name := range rowProducers {
			known = append(known, name)
		}
		return nil, fmt.Errorf("unknown component-result producer %q; known producers: %s",
			spec.Producer, strings.Join(known, ", "))
	}
	return p, nil
}

// rowMarker prefixes a line a harness intends as a result row.
//
// A marker rather than a regex over prose. ADR-090 rejects "a per-GPU pattern
// to LogProfile" as "the fixed-field regex approach the customer asked to move
// away from", and the same objection applies to parsing a harness's human
// output: the format is then owned by whoever last changed a log message. A
// harness that wants to emit rows opts in by printing this prefix, everything
// else on stdout is ignored, and the two cannot collide by accident.
const rowMarker = "NVCRE_ROW "

// structuredRowProducer reads rows a harness emits deliberately, one JSON
// object per line behind rowMarker.
//
// The wire shape is the row minus its node, which the caller supplies:
//
//	NVCRE_ROW {"componentID":"GPU-abc","index":0,"name":"achievedTFLOPS",
//	           "value":1234.5,"unit":"TFLOPS","sample":0,
//	           "timestamp":"2026-11-09T00:00:00Z","outcome":"pass"}
//
// A row is the marker followed by a JSON object, and the leading brace is part
// of the declaration rather than a convenience. The marker is matched anywhere
// in the line because MPI tags every line it relays with a rank prefix
// ("[0] NVCRE_ROW {...}"), so anchoring at column zero would drop every row
// from a launcher. Matching anywhere then means prose can contain the marker
// too — "could not emit NVCRE_ROW for device 3" is a plausible line from a
// harness's own error path — and treating that as a malformed row would fail
// the Job's whole result set on a log message. Requiring the brace separates
// the two without caring what precedes the marker.
//
// Past that point an unparseable payload IS an error rather than a skip. A
// line that opens a JSON object behind the marker has declared itself a
// result, so a broken one is a defect in the harness, and silently dropping it
// would turn that defect into missing evidence — the thing this whole
// mechanism exists to make visible.
type structuredRowProducer struct{}

// wireRow is a row as a harness emits it. Node is absent by construction.
type wireRow struct {
	ComponentID string  `json:"componentID"`
	Index       int32   `json:"index"`
	Name        string  `json:"name"`
	Value       float64 `json:"value"`
	Unit        string  `json:"unit"`
	Sample      int64   `json:"sample"`
	Timestamp   string  `json:"timestamp"`
	Outcome     string  `json:"outcome"`
}

func (structuredRowProducer) Produce(node string, lines []string) (produceResult, error) {
	var out produceResult
	reject := func(format string, args ...any) {
		out.Rejected++
		if out.FirstReject == "" {
			out.FirstReject = fmt.Sprintf(format, args...)
		}
	}
	for i, line := range lines {
		_, after, found := strings.Cut(line, rowMarker)
		if !found {
			continue
		}
		payload := strings.TrimSpace(after)
		if !strings.HasPrefix(payload, "{") {
			// Prose that mentions the marker, not a declared row.
			continue
		}
		var w wireRow
		if err := json.Unmarshal([]byte(payload), &w); err != nil {
			reject("line %d declares a result row but does not parse: %v", i+1, err)
			continue
		}
		if w.ComponentID == "" || w.Name == "" {
			reject("line %d declares a result row with no componentID or name", i+1)
			continue
		}
		out.Rows = append(out.Rows, shard.Row{
			Node:        node,
			ComponentID: w.ComponentID,
			Index:       w.Index,
			Name:        w.Name,
			Value:       w.Value,
			Unit:        w.Unit,
			Sample:      w.Sample,
			Timestamp:   w.Timestamp,
			Outcome:     w.Outcome,
		})
	}
	return out, nil
}
