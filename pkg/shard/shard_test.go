// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shard

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// buildInput is the schema of a build case's input.yaml.
//
// Rows are given either explicitly, which pins the wire form and the emission
// order, or by generate, which produces a corpus large enough to cross a roll
// boundary without a golden file holding thousands of rows.
type buildInput struct {
	Options  Options       `yaml:"options"`
	Rows     []Row         `yaml:"rows"`
	Generate *generateSpec `yaml:"generate"`
}

// Options is decoded straight from the case's input.yaml. sigs.k8s.io/yaml
// converts to JSON and unmarshals with encoding/json, which matches untagged
// fields case-insensitively, so the fixtures write rollBytes, maxRowRawBytes,
// maxTotalRawBytes and maxShardsPerJob.

type generateSpec struct {
	Nodes      int    `yaml:"nodes"`
	Components int    `yaml:"components"`
	Samples    int    `yaml:"samples"`
	Name       string `yaml:"name"`
	// PadBytes inflates each row's name so a case can reach a size bound
	// without generating a huge number of rows.
	PadBytes int `yaml:"padBytes"`
}

// buildOutput is the golden projection.
//
// Shard digests and byte sizes are deliberately absent: gzip's exact output is
// not a contract across Go releases, so pinning it would make these goldens
// fail on a toolchain bump rather than on a behaviour change. What is pinned
// is the shape the design promises — how many shards, how many rows in each,
// which rows in what order, and what was dropped. Digest self-consistency and
// byte-for-byte reproducibility are asserted in the test body instead, where a
// failure names the property rather than a hex string.
type buildOutput struct {
	// ShardCount is pinned for every case. Where each shard ENDS is not: that
	// is a function of gzip's exact output and of the flush cadence, which is
	// an internal tuning knob. So the per-shard row counts are emitted only
	// when a case produced a single shard, where they are just "every row
	// landed". For a case that rolls, the contract is asserted as properties
	// in the test body instead — contiguous indices, no empty shard, and every
	// non-final shard at or over the roll bound — so a compression change
	// fails as the property it broke rather than as a row count.
	ShardCount   int        `json:"shardCount"`
	Shards       []shardOut `json:"shards,omitempty"`
	TotalRows    int64      `json:"totalRows"`
	Dropped      []Dropped  `json:"dropped,omitempty"`
	DroppedTotal int64      `json:"droppedTotal,omitempty"`
	// NDJSON is the decoded payload, present only when the corpus is small
	// enough to read. It is what pins the row schema's field order and the
	// two-decimal value format.
	NDJSON []string `json:"ndjson,omitempty"`
}

type shardOut struct {
	Index int   `json:"index"`
	Rows  int64 `json:"rows"`
}

const ndjsonGoldenLimit = 24

// testMetric is the metric name the fixtures use. The literal in the tampered
// payload in TestDecodeRowsRejectsTamperedPayload is deliberately spelled out
// rather than built from this, because that test is asserting on exact bytes.
const (
	testMetric    = "achievedTFLOPS"
	testOutcome   = "pass"
	testComponent = "GPU-000"
	testUnit      = "TFLOPS"
	testNode      = "node-000"
	testTimestamp = "2026-11-03T00:00:00Z"
	testShardName = "shard-0000"
)

func TestBuild(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "build",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in buildInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		rows := in.Rows
		if in.Generate != nil {
			rows = append(rows, generateRows(*in.Generate)...)
		}

		res, err := buildRows(in.Options, rows)
		if err != nil {
			return err
		}

		// Every golden case is also a determinism case: the bundle manifest's
		// per-file sha256 is meaningless if the same rows do not produce the
		// same bytes, and a failed shard write is retried by rewriting the
		// same object.
		again, err := buildRows(in.Options, rows)
		if err != nil {
			return err
		}
		if len(again.Shards) != len(res.Shards) {
			return fmt.Errorf("rebuild produced %d shards, first build produced %d",
				len(again.Shards), len(res.Shards))
		}
		for i := range res.Shards {
			if !bytes.Equal(res.Shards[i].Payload, again.Shards[i].Payload) {
				return fmt.Errorf("shard %d is not byte-identical on rebuild", i)
			}
		}

		// Roll behaviour is asserted as properties here rather than pinned in
		// the golden. Where a shard ends is a function of gzip's exact output
		// and of the flush cadence, which is an internal tuning knob: pinning
		// the per-shard row counts makes a compression change or a cadence
		// change fail as a row count, naming neither. These three hold for any
		// encoder that rolls correctly.
		for i, s := range res.Shards {
			if s.Index != i {
				return fmt.Errorf("shard at position %d reports index %d", i, s.Index)
			}
			if s.Rows == 0 {
				return fmt.Errorf("shard %d is empty; a shard is only sealed when it holds rows", i)
			}
			// Every shard but the last was sealed because it reached the
			// bound, so it must have got there.
			if i < len(res.Shards)-1 && len(s.Payload) < in.Options.rollBytes() {
				return fmt.Errorf("shard %d sealed at %d bytes, under the %d byte roll bound",
					i, len(s.Payload), in.Options.rollBytes())
			}
		}

		out := buildOutput{
			ShardCount:   len(res.Shards),
			TotalRows:    res.Rows,
			Dropped:      res.Dropped,
			DroppedTotal: res.DroppedTotal,
		}
		var decoded []Row
		for _, s := range res.Shards {
			if len(res.Shards) == 1 {
				out.Shards = append(out.Shards, shardOut{Index: s.Index, Rows: s.Rows})
			}

			sum := sha256.Sum256(s.Payload)
			if got := hex.EncodeToString(sum[:]); got != s.SHA256 {
				return fmt.Errorf("shard %d records digest %s but its payload hashes to %s",
					s.Index, s.SHA256, got)
			}
			if over := len(s.Payload) + dataBlockReserve; over > MaxConfigMapBytes {
				return fmt.Errorf("shard %d is %d bytes, over the ConfigMap budget", s.Index, over)
			}

			// Round trip through the ConfigMap shape the controller writes, so
			// the decode path is covered by the same cases as the encode path.
			cm := &corev1.ConfigMap{Data: s.Data(), BinaryData: s.BinaryData()}
			cm.Name = fmt.Sprintf("shard-%04d", s.Index)
			got, decErr := DecodeRows(cm)
			if decErr != nil {
				return fmt.Errorf("decode shard %d: %w", s.Index, decErr)
			}
			if int64(len(got)) != s.Rows {
				return fmt.Errorf("shard %d decoded %d rows, recorded %d", s.Index, len(got), s.Rows)
			}
			decoded = append(decoded, got...)
		}

		if int64(len(decoded)) != res.Rows {
			return fmt.Errorf("decoded %d rows across shards, Result reports %d", len(decoded), res.Rows)
		}
		if len(decoded) <= ndjsonGoldenLimit {
			for _, r := range decoded {
				b, mErr := json.Marshal(r)
				if mErr != nil {
					return mErr
				}
				out.NDJSON = append(out.NDJSON, string(b))
			}
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func buildRows(opts Options, rows []Row) (Result, error) {
	b, err := NewBuilder(opts)
	if err != nil {
		return Result{}, err
	}
	for _, r := range rows {
		if addErr := b.Add(r); addErr != nil {
			return Result{}, addErr
		}
	}
	return b.Build()
}

// generateRows produces a deterministic corpus. Rows are emitted in an order
// deliberately unlike the normative one — samples outermost, components
// descending — so a case that pins emission order is testing the sort rather
// than the generator.
func generateRows(g generateSpec) []Row {
	name := g.Name
	if name == "" {
		name = testMetric
	}
	if g.PadBytes > 0 {
		name += strings.Repeat("x", g.PadBytes)
	}
	var out []Row
	for s := range g.Samples {
		for c := g.Components - 1; c >= 0; c-- {
			for n := range g.Nodes {
				out = append(out, Row{
					Node:        fmt.Sprintf("node-%03d", n),
					ComponentID: fmt.Sprintf("GPU-%03d-%03d", n, c),
					Index:       int32(c), //nolint:gosec // bounded by the case's component count
					Name:        name,
					Value:       1000 + float64(c) + float64(s)/100,
					Unit:        testUnit,
					Sample:      int64(s),
					Timestamp:   fmt.Sprintf("2026-11-03T00:%02d:%02dZ", s/60%60, s%60),
					Outcome:     testOutcome,
				})
			}
		}
	}
	return out
}

// TestDecodeRowsRejectsTamperedPayload pins the property the shard's own
// digest exists for: a payload edited in the cluster is caught on read, not
// carried into a bundle and certified by its manifest.
func TestDecodeRowsRejectsTamperedPayload(t *testing.T) {
	res, err := buildRows(Options{}, []Row{{
		Node: testNode, ComponentID: testComponent, Index: 0,
		Name: testMetric, Value: 1234.5, Unit: testUnit,
		Sample: 0, Timestamp: testTimestamp, Outcome: testOutcome,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.Shards) != 1 {
		t.Fatalf("expected one shard, got %d", len(res.Shards))
	}

	tampered, err := gzip.GzipString(`{"node":"node-000","componentID":"GPU-000","index":0,` +
		`"name":"achievedTFLOPS","value":9999.00,"unit":"TFLOPS","sample":0,` +
		`"timestamp":"2026-11-03T00:00:00Z","outcome":"pass"}` + "\n")
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}

	cm := &corev1.ConfigMap{Data: res.Shards[0].Data(), BinaryData: map[string][]byte{RowsKey: tampered}}
	cm.Name = testShardName
	if _, err := DecodeRows(cm); err == nil {
		t.Fatal("expected a digest mismatch, got nil")
	}
}

// TestDecodeRowsDistinguishesMissingFromCorrupt pins the distinction ADR-090
// requires on the evidence path: an unreadable shard must not look like a
// shard with nothing in it.
//
// Both directions are asserted. Checking only that the corrupt case is not
// ErrNoRows leaves the sentinel itself untested, so replacing it with a plain
// error would pass — and the sentinel is exactly what a caller uses to tell
// "this Job owed nothing" from "this shard cannot be read".
func TestDecodeRowsDistinguishesMissingFromCorrupt(t *testing.T) {
	for name, cm := range map[string]*corev1.ConfigMap{
		"nil configmap":      nil,
		"no binary data":     {},
		"empty rows payload": {BinaryData: map[string][]byte{RowsKey: {}}},
	} {
		if _, err := DecodeRows(cm); !errors.Is(err, ErrNoRows) {
			t.Fatalf("%s: expected ErrNoRows, got %v", name, err)
		}
	}

	corrupt := &corev1.ConfigMap{
		Data:       map[string]string{FormatVersionKey: "1", DigestKey: digestOf([]byte("not gzip")), RowCountKey: "1"},
		BinaryData: map[string][]byte{RowsKey: []byte("not gzip")},
	}
	corrupt.Name = "shard-0000"
	_, err := DecodeRows(corrupt)
	if err == nil {
		t.Fatal("expected an error for a corrupt payload")
	}
	if errors.Is(err, ErrNoRows) {
		t.Fatal("a corrupt payload must not report as a missing one")
	}
}

// TestDecodeRowsRequiresItsIntegrityKeys pins that the three checks are not
// conditional on the object carrying the key they read.
//
// Making them conditional is the natural way to write them and it is wrong:
// the edit someone tampering would actually make is to replace the payload and
// delete the digest, and that passed clean. Shard.Data writes all four keys on
// every shard, so a payload without them was not written by this package.
func TestDecodeRowsRequiresItsIntegrityKeys(t *testing.T) {
	res, err := buildRows(Options{}, []Row{{
		Node: testNode, ComponentID: testComponent, Index: 0, Name: testMetric,
		Value: 1234.5, Unit: testUnit, Sample: 0,
		Timestamp: testTimestamp, Outcome: testOutcome,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	shard := res.Shards[0]

	for _, missing := range []string{FormatVersionKey, DigestKey, RowCountKey} {
		data := shard.Data()
		delete(data, missing)
		cm := &corev1.ConfigMap{Data: data, BinaryData: shard.BinaryData()}
		cm.Name = testShardName
		if _, decErr := DecodeRows(cm); decErr == nil {
			t.Fatalf("a shard with no %s must be refused, not decoded", missing)
		}
	}

	for _, tc := range []struct{ key, value, why string }{
		{FormatVersionKey, "99", "a future format version must not be read as this one"},
		{RowCountKey, "7", "a row count that disagrees with the payload must be refused"},
	} {
		data := shard.Data()
		data[tc.key] = tc.value
		cm := &corev1.ConfigMap{Data: data, BinaryData: shard.BinaryData()}
		cm.Name = testShardName
		if _, decErr := DecodeRows(cm); decErr == nil {
			t.Fatalf("%s: %s", tc.key, tc.why)
		}
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestAddDropsValuesJSONCannotRepresent pins that a non-finite value costs its
// own row and nothing else.
//
// This is not a golden case because it cannot be: the fixtures are YAML parsed
// through JSON, and JSON has no literal for NaN or an infinity, which is the
// same reason the encoder rejects such a row in the first place.
//
// Before the guard, Add returned an error for these, and an error from Add
// fails the Job's entire result set and requeues it on a row that can never be
// written. A harness dividing a work total by an elapsed time produces exactly
// these values when a device reports zero elapsed time.
func TestAddDropsValuesJSONCannotRepresent(t *testing.T) {
	good := Row{
		Node: testNode, ComponentID: testComponent, Index: 0, Name: testMetric,
		Value: 1200, Unit: testUnit, Sample: 0,
		Timestamp: testTimestamp, Outcome: testOutcome,
	}
	bad := func(v float64, id string) Row {
		r := good
		r.ComponentID = id
		r.Value = v
		return r
	}

	b, err := NewBuilder(Options{})
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	for _, r := range []Row{
		good,
		bad(math.NaN(), "GPU-001"),
		bad(math.Inf(1), "GPU-002"),
		bad(math.Inf(-1), "GPU-003"),
	} {
		if addErr := b.Add(r); addErr != nil {
			t.Fatalf("Add must not fail the result set for one bad value: %v", addErr)
		}
	}
	res, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if res.Rows != 1 {
		t.Fatalf("expected the one finite row to be stored, got %d", res.Rows)
	}
	if len(res.Dropped) != 3 {
		t.Fatalf("expected three dropped rows, got %d", len(res.Dropped))
	}
	for _, d := range res.Dropped {
		if d.Reason != ReasonRowUnencodable {
			t.Fatalf("expected reason %s, got %s", ReasonRowUnencodable, d.Reason)
		}
		// The record has to name the component, or an operator cannot tell
		// which device lost its evidence.
		if d.ComponentID == "" {
			t.Fatal("a dropped row must name its component")
		}
	}
}

// TestNewBuilderRejectsOptionsThatCannotFitAConfigMap pins the validation that
// makes finish()'s assertion unreachable.
//
// Only the roll bound and the row cap are settable; the flush cadence is a
// constant. So raising the row cap alone silently invalidates the arithmetic
// that keeps a shard inside the object budget, and the failure used to arrive
// after a Job's rows were already gathered, where the only answer left was to
// discard a result set that had been collected correctly.
func TestNewBuilderRejectsOptionsThatCannotFitAConfigMap(t *testing.T) {
	if _, err := NewBuilder(Options{MaxRowRawBytes: 512 * 1024}); err == nil {
		t.Fatal("a row cap that breaks the shard budget must be refused at construction")
	}
	// The defaults, and a lowered cap, must still be accepted.
	for _, opts := range []Options{{}, {MaxRowRawBytes: 256}, {RollBytes: 2048}} {
		if _, err := NewBuilder(opts); err != nil {
			t.Fatalf("%+v should be accepted: %v", opts, err)
		}
	}
}

// TestDroppedLedgerNamesEveryComponent pins the property an arrival-ordered
// ledger did not have.
//
// Keeping the first N records in arrival order looks equivalent to keeping N
// components and is not. RowBudgetExceeded is recorded in the producer's
// emission order, so the kept records are contiguous in that order and all
// come from the few components straddling the budget boundary. Measured on a
// 480,000-row corpus, an arrival-ordered ledger named 3 of the 416 nodes that
// lost rows. ADR-090 requires a dropped row to be counted with its component
// reference, and 0.5% of them carried one.
//
// Here every component loses rows, and far more rows than components, so an
// arrival-ordered ledger would name a handful while this must name them all.
func TestDroppedLedgerNamesEveryComponent(t *testing.T) {
	const (
		components    = 64
		samplesEach   = 50
		expectedTotal = components * samplesEach
	)
	// A budget of one byte drops everything, and a long name exercises the
	// per-field truncation at the same time.
	longName := strings.Repeat("n", 4096)
	b, err := NewBuilder(Options{MaxTotalRawBytes: 1})
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	// Sample-major, so the arrival order interleaves components the way a real
	// sweep does and a prefix of it would still miss most of them.
	for s := range samplesEach {
		for c := range components {
			if addErr := b.Add(Row{
				Node:        testNode,
				ComponentID: fmt.Sprintf("GPU-%03d", c),
				Index:       int32(c), //nolint:gosec // bounded by components
				Name:        longName,
				Value:       1, Unit: testUnit, Sample: int64(s),
				Timestamp: testTimestamp, Outcome: testOutcome,
			}); addErr != nil {
				t.Fatalf("add: %v", addErr)
			}
		}
	}
	res, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// Every component is named exactly once, and its Count carries the
	// magnitude that one-record-per-component would otherwise lose.
	if len(res.Dropped) != components {
		t.Fatalf("ledger names %d components, want all %d", len(res.Dropped), components)
	}
	seen := map[string]bool{}
	var counted int64
	for _, d := range res.Dropped {
		if seen[d.ComponentID] {
			t.Fatalf("component %s appears more than once", d.ComponentID)
		}
		seen[d.ComponentID] = true
		if d.Count != samplesEach {
			t.Fatalf("component %s records Count %d, want %d", d.ComponentID, d.Count, samplesEach)
		}
		counted += d.Count
		if len(d.Name) > maxDroppedFieldBytes+3 {
			t.Fatalf("a record copied a %d byte name; fields must be truncated", len(d.Name))
		}
	}
	if counted != expectedTotal || res.DroppedTotal != expectedTotal {
		t.Fatalf("counts sum to %d and DroppedTotal is %d, want %d for both",
			counted, res.DroppedTotal, expectedTotal)
	}

	// And the encoded form has to fit the object it is written to.
	payload, err := EncodeTruncation(res.Dropped)
	if err != nil {
		t.Fatalf("encode truncation: %v", err)
	}
	if len(payload)+dataBlockReserve > MaxConfigMapBytes {
		t.Fatalf("truncation payload is %d bytes, over the ConfigMap budget", len(payload))
	}
}

// TestDroppedLedgerCapsAtMaxDroppedRecords pins that the ledger is still
// bounded when the number of distinct components exceeds the cap, and that the
// cap never makes the loss look smaller than it was.
func TestDroppedLedgerCapsAtMaxDroppedRecords(t *testing.T) {
	const extra = 200
	b, err := NewBuilder(Options{MaxTotalRawBytes: 1})
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	total := maxDroppedRecords + extra
	for c := range total {
		if addErr := b.Add(Row{
			Node:        testNode,
			ComponentID: fmt.Sprintf("GPU-%06d", c),
			Index:       0,
			Name:        testMetric,
			Value:       1, Unit: testUnit, Sample: 0,
			Timestamp: testTimestamp, Outcome: testOutcome,
		}); addErr != nil {
			t.Fatalf("add: %v", addErr)
		}
	}
	res, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if len(res.Dropped) != maxDroppedRecords {
		t.Fatalf("ledger holds %d records, want it capped at %d", len(res.Dropped), maxDroppedRecords)
	}
	if res.DroppedTotal != int64(total) {
		t.Fatalf("DroppedTotal is %d, want the exact %d", res.DroppedTotal, total)
	}
}

// TestEncodeTruncationRefusesAnOversizedLedger pins the backstop.
//
// The Builder's own cap makes this unreachable through Add, so the assertion
// had no coverage and deleting it changed nothing observable. It still has to
// hold, because Build's contract lets a caller append its own records to the
// ledger for rows lost to log pagination, and nothing bounds that append. An
// unwritable truncation object is a silent failure; a returned error is not.
func TestEncodeTruncationRefusesAnOversizedLedger(t *testing.T) {
	// Genuinely random fields. A patterned generator is not enough: a
	// repeating byte sequence compresses so well that 20,000 records came to
	// 176 KB and the assertion did not fire. gzip cannot do better than about
	// 0.82 on random text over a JSON-safe alphabet, which is what makes the
	// ledger's size track its record count.
	field := func() string {
		const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		raw := make([]byte, maxDroppedFieldBytes)
		if _, err := rand.Read(raw); err != nil {
			t.Fatalf("rand: %v", err)
		}
		for i, c := range raw {
			raw[i] = alphabet[int(c)%len(alphabet)]
		}
		return string(raw)
	}
	oversized := make([]Dropped, 20000)
	for i := range oversized {
		oversized[i] = Dropped{
			Node: field(), ComponentID: field(), Name: field(),
			Index:  int32(i % 8), //nolint:gosec // bounded by the modulus
			Sample: int64(i), Reason: ReasonRowBudgetExceeded, RawBytes: 177,
		}
	}

	payload, err := EncodeTruncation(oversized)
	if err == nil {
		t.Fatalf("a %d byte ledger must be refused, not written", len(payload))
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Fatalf("the error should name the budget, got %v", err)
	}
}

// TestBuildShardLimitNamesEveryAbandonedRow exercises the shard-count limit.
//
// This is a Go test rather than a golden case, and the reason is worth
// recording. Tripping the limit needs at least three shards, and a shard
// boundary cannot occur inside less than flushEveryRawBytes of input, because
// the roll counter only sees what gzip has emitted. So the smallest corpus
// that reaches the limit is on the order of a thousand rows, and its abandoned
// set is several hundred records. Snapshotting that proves nothing a property
// does not, and buries the two things that matter: that every abandoned row is
// NAMED, and that DroppedTotal is exact.
//
// The branch had two defects while nothing could reach it. It fired even when
// the row count landed exactly on the limit and nothing was lost, and when
// something was lost it emitted a single record with no node, component or
// metric on it — exactly what Result.Dropped promises never to do.
func TestBuildShardLimitNamesEveryAbandonedRow(t *testing.T) {
	rows := generateRows(generateSpec{Nodes: 4, Components: 8, Samples: 60, PadBytes: 24})

	// Unlimited first, to learn how many shards this corpus really needs.
	full, err := buildRows(Options{RollBytes: 2048}, rows)
	if err != nil {
		t.Fatalf("unlimited build: %v", err)
	}
	if len(full.Shards) < 3 {
		t.Fatalf("fixture produces %d shards; it must exceed the limit under test", len(full.Shards))
	}
	if full.DroppedTotal != 0 {
		t.Fatalf("unlimited build dropped %d rows", full.DroppedTotal)
	}

	const limit = 2
	capped, err := buildRows(Options{RollBytes: 2048, MaxShardsPerJob: limit}, rows)
	if err != nil {
		t.Fatalf("capped build: %v", err)
	}

	if len(capped.Shards) != limit {
		t.Fatalf("produced %d shards, want the limit of %d", len(capped.Shards), limit)
	}
	// Nothing may be silently lost: stored plus dropped must account for the
	// whole corpus.
	if capped.Rows+capped.DroppedTotal != int64(len(rows)) {
		t.Fatalf("stored %d + dropped %d != %d rows in",
			capped.Rows, capped.DroppedTotal, len(rows))
	}
	if capped.DroppedTotal == 0 {
		t.Fatal("the limit was reached but nothing was recorded as dropped")
	}
	// Every abandoned row is named. The whole point of the fix.
	if int64(len(capped.Dropped)) != capped.DroppedTotal {
		t.Fatalf("named %d of %d dropped rows", len(capped.Dropped), capped.DroppedTotal)
	}
	for _, d := range capped.Dropped {
		if d.Reason != ReasonShardLimitExceeded {
			t.Fatalf("reason is %s, want %s", d.Reason, ReasonShardLimitExceeded)
		}
		if d.Node == "" || d.ComponentID == "" || d.Name == "" {
			t.Fatalf("an abandoned row was recorded without its identity: %+v", d)
		}
	}

	// Build stays pure: the second call must agree, which it cannot if the
	// branch records onto the Builder.
	b, err := NewBuilder(Options{RollBytes: 2048, MaxShardsPerJob: limit})
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	for _, r := range rows {
		if addErr := b.Add(r); addErr != nil {
			t.Fatalf("add: %v", addErr)
		}
	}
	first, err := b.Build()
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := b.Build()
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if first.DroppedTotal != second.DroppedTotal || len(first.Dropped) != len(second.Dropped) {
		t.Fatalf("Build is not idempotent: first dropped %d/%d, second %d/%d",
			len(first.Dropped), first.DroppedTotal, len(second.Dropped), second.DroppedTotal)
	}

	// A corpus whose shard count lands exactly on the limit must not be
	// reported as truncated.
	//
	// Note what this does and does not prove. With the limit set to the
	// corpus's own unlimited shard count, the in-loop branch is never reached
	// at all: the final shard is emitted by the trailing flush after the loop
	// ends. So this asserts the outcome, not the branch. The branch itself has
	// no "and rows remain" guard precisely because such a guard was proven
	// dead; see the comment in Build.
	exact, err := buildRows(Options{RollBytes: 2048, MaxShardsPerJob: len(full.Shards)}, rows)
	if err != nil {
		t.Fatalf("exact-limit build: %v", err)
	}
	if exact.DroppedTotal != 0 {
		t.Fatalf("a complete result set was reported as truncated: %d dropped", exact.DroppedTotal)
	}
	if len(exact.Shards) != len(full.Shards) {
		t.Fatalf("exact-limit build produced %d shards, want %d", len(exact.Shards), len(full.Shards))
	}
}

// TestRowCompareIsTotalBeyondTheNormativeKey pins that shard bytes are a
// function of the row SET and not of the order a producer emitted them.
//
// Nothing defended this. Truncating rowCompare to stop after Sample escaped
// every test and every golden, because no fixture contained two rows that tie
// on all five leading keys — so the totality the long comment in row.go argues
// for was asserted nowhere. The bundle manifest's per-file sha256 and the
// rewrite-on-retry path both rest on it.
//
// Signed zero gets its own case: numeric comparison ties -0.0 with +0.0 while
// the encoder renders them differently, which is exactly how a numeric-only
// comparison reintroduced order dependence.
func TestRowCompareIsTotalBeyondTheNormativeKey(t *testing.T) {
	base := Row{
		Node: testNode, ComponentID: testComponent, Index: 0, Name: testMetric,
		Value: 1200, Unit: testUnit, Sample: 0,
		Timestamp: testTimestamp, Outcome: testOutcome,
	}
	vary := func(mutate func(*Row)) Row {
		r := base
		mutate(&r)
		return r
	}

	for name, other := range map[string]Row{
		"value":       vary(func(r *Row) { r.Value = 1300 }),
		"signed zero": vary(func(r *Row) { r.Value = math.Copysign(0, -1) }),
		"unit":        vary(func(r *Row) { r.Unit = "GB/s" }),
		"timestamp":   vary(func(r *Row) { r.Timestamp = "2026-11-03T00:00:09Z" }),
		"outcome":     vary(func(r *Row) { r.Outcome = "fail" }),
	} {
		first := vary(func(r *Row) {})
		if name == "signed zero" {
			// Make the pair genuinely a pair of zeros, so only the sign differs.
			first.Value = 0
		}

		forward, err := buildRows(Options{}, []Row{first, other})
		if err != nil {
			t.Fatalf("%s: forward build: %v", name, err)
		}
		reverse, err := buildRows(Options{}, []Row{other, first})
		if err != nil {
			t.Fatalf("%s: reverse build: %v", name, err)
		}

		if len(forward.Shards) != 1 || len(reverse.Shards) != 1 {
			t.Fatalf("%s: expected one shard each, got %d and %d",
				name, len(forward.Shards), len(reverse.Shards))
		}
		if forward.Shards[0].SHA256 != reverse.Shards[0].SHA256 {
			t.Fatalf("%s: insertion order changed the shard digest (%s vs %s); "+
				"rowCompare is not total on this field",
				name, forward.Shards[0].SHA256[:16], reverse.Shards[0].SHA256[:16])
		}
	}
}

// TestTruncateFieldCutsOnARuneBoundary pins that bounding a truncation
// record's strings does not corrupt them.
//
// Slicing at a byte offset splits a multi-byte rune, and json.Marshal rewrites
// the resulting invalid bytes to U+FFFD without erroring — so a metric name
// carrying any non-ASCII text would be silently mangled in the one record
// whose purpose is to say what was lost.
func TestTruncateFieldCutsOnARuneBoundary(t *testing.T) {
	// The rune must be THREE bytes for byte 128 to land mid-rune: 128 is not a
	// multiple of 3, but it is a multiple of 2, so a two-byte rune like é
	// (U+00E9) puts a rune boundary exactly at 128 and the test never
	// exercises the straddle it exists for. An earlier version of this fixture
	// used é and was vacuous: deleting truncateField's whole rune-boundary
	// loop passed it.
	s := strings.Repeat("世", 200)
	if utf8.RuneStart(s[maxDroppedFieldBytes]) {
		t.Fatalf("fixture does not straddle a rune at byte %d; the test would be vacuous",
			maxDroppedFieldBytes)
	}
	got := truncateField(s)

	if !utf8.ValidString(got) {
		t.Fatalf("truncated to invalid UTF-8: %q", got)
	}
	if len(got) > maxDroppedFieldBytes+3 {
		t.Fatalf("truncated to %d bytes, want at most %d", len(got), maxDroppedFieldBytes+3)
	}
	// And it must survive the encoder without being rewritten.
	b, err := json.Marshal(Dropped{Name: got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(b, []byte("�")) {
		t.Fatal("the encoder replaced bytes in the truncated field")
	}

	// A short string is returned untouched.
	if short := truncateField("abc"); short != "abc" {
		t.Fatalf("a short field was altered: %q", short)
	}
}

// TestBuildDroppedDoesNotAliasTheBuilder pins that a caller can add its own
// truncation records to the Result.
//
// ADR-090 requires exactly that for rows lost to log pagination. Handing back
// the Builder's own slice let the caller's append land in its spare capacity,
// where the next Add overwrote it with no error anywhere.
func TestBuildDroppedDoesNotAliasTheBuilder(t *testing.T) {
	b, err := NewBuilder(Options{})
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	for i := range 3 {
		if addErr := b.Add(Row{
			Node: testNode, ComponentID: testComponent, Index: 0, Name: testMetric,
			Value: math.NaN(), Unit: testUnit, Sample: int64(i),
			Timestamp: testTimestamp, Outcome: testOutcome,
		}); addErr != nil {
			t.Fatalf("add: %v", addErr)
		}
	}
	res, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	res.Dropped = append(res.Dropped, Dropped{Node: "caller", Reason: "LogUnpageable"})
	mine := res.Dropped[len(res.Dropped)-1]

	// One more drop into the Builder must not reach through into the Result.
	if addErr := b.Add(Row{
		Node: "node-999", ComponentID: "GPU-999", Name: testMetric,
		Value: math.NaN(), Unit: testUnit, Timestamp: testTimestamp, Outcome: testOutcome,
	}); addErr != nil {
		t.Fatalf("add: %v", addErr)
	}
	if got := res.Dropped[len(res.Dropped)-1]; got != mine {
		t.Fatalf("the caller's record was overwritten by a later Add: %+v", got)
	}
}

// TestTruncationRoundTrip pins that dropped-row records survive the encode and
// decode the truncation ConfigMap uses.
func TestTruncationRoundTrip(t *testing.T) {
	if b, err := EncodeTruncation(nil); err != nil || b != nil {
		t.Fatalf("empty input should encode to nil, got %v, %v", b, err)
	}
	in := []Dropped{
		{Node: testNode, ComponentID: testComponent, Index: 0, Name: testMetric,
			Sample: 7, Reason: ReasonRowTooLarge, RawBytes: 200000},
	}
	b, err := EncodeTruncation(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeTruncation(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0] != in[0] {
		t.Fatalf("round trip changed the record: %+v", got)
	}
}
