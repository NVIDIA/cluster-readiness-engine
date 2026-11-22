// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"unicode/utf8"
)

const (
	// RollBytes is the compressed-output size at which the writer starts a new
	// shard. ADR-090 fixes it at 768 KiB.
	//
	// The bound is checked after a row is written, not before, so a shard can
	// exceed it by one row. That overshoot is what MaxRowRawBytes bounds, and
	// the two together are what keep a shard under MaxConfigMapBytes.
	RollBytes = 768 * 1024

	// MaxRowRawBytes is the largest encoded row the writer will accept.
	//
	// ADR-090 states 256 KiB of *compressed* output. Two changes here, both
	// deliberate deviations, both recorded in ADR-090's Amendments section.
	//
	// First the figure. 768 KiB of roll plus a 256 KiB row is exactly 1 MiB,
	// which is the whole object budget, leaving nothing for the plaintext data
	// block or for the output gzip has buffered but not yet emitted when the
	// roll is checked. 128 KiB leaves 128 KiB of headroom.
	//
	// Second the unit. A row's compressed contribution inside a shared stream
	// is not well defined, because it depends on everything written before it,
	// and compressing each row alone to find out doubles the CPU. Raw bytes
	// bound it from above instead: gzip's worst case expansion is about 0.03%
	// plus a fixed header, so a row under this raw bound cannot exceed it by
	// more than a rounding error once compressed. The ADR's objection to row
	// bounds is about the aggregate roll bound, where the 2.8x to 34x ratio
	// spread really does make a raw figure meaningless; a per-row backstop
	// only has to be conservative in one direction.
	MaxRowRawBytes = 128 * 1024

	// MaxTotalRawBytes bounds the encoded rows one Job may hold in memory.
	//
	// ADR-090 calls for spooling to a temporary file and sorting there. The
	// controller has no writable path to spool to: it runs with
	// readOnlyRootFilesystem true and no volumes at all, so os.CreateTemp
	// fails with EROFS. Rows are therefore sorted in memory under this budget,
	// and a producer that exceeds it stops and records a RowBudgetExceeded
	// truncation rather than growing without bound.
	//
	// The figure is sized against the PROCESS, not the Job, and that
	// distinction cost a measured defect. This counts only the encoded lines,
	// while a Builder also retains b.rows with its own copy of every string:
	// measured retention is 2.0x what the budget counts. And the budget is
	// per-Builder while the limit is shared — MaxConcurrentReconciles defaults
	// to 10, so ten Builders can be live at once. At 64 MiB that measured
	// 2,222 MiB of peak heap against the manager's 1Gi limit, 217% of it: the
	// guard that exists to contain a runaway producer was itself the OOM.
	//
	// 16 MiB holds the real producer with room to spare — a per-GPU GEMM burn
	// in at one node per Job is 14,400 rows for a 30 minute soak and 57,600
	// for two hours, 14.3 MB of encoded lines — and puts the ten-worker
	// default at roughly 320 MiB live.
	//
	// The consequence is owned rather than hidden: ADR-090's own largest
	// corpus, 480,000 rows from a per-link NCCL producer, does not fit and is
	// truncated with a RowBudgetExceeded record. That producer does not exist
	// yet, and ADR-090's Amendments section records the limit and its
	// consequence for that corpus.
	MaxTotalRawBytes = 16 * 1024 * 1024

	// MaxConfigMapBytes is the API server's per-object limit. ValidateConfigMap
	// sums len(value) across Data and BinaryData and rejects the object past
	// this. It counts decoded bytes, so base64 inflation on the wire costs
	// request body, not budget.
	MaxConfigMapBytes = 1024 * 1024

	// dataBlockReserve is held back from MaxConfigMapBytes for the plaintext
	// data block a shard carries beside its payload (format version, shard
	// index, row count, digest).
	dataBlockReserve = 4 * 1024

	// MaxShardsPerJob bounds one Job's shard set, so the %04d name suffix
	// cannot overflow and a runaway producer cannot create objects without
	// limit.
	MaxShardsPerJob = 9999

	// flushEveryRawBytes is how much encoded input may accumulate before the
	// gzip stream is flushed so the roll counter reflects it.
	//
	// A counting writer around the stream counts only what gzip has actually
	// emitted, and flate holds its window and an unemitted block, so between
	// flushes the count is a lower bound. The gap is not small: 387 KB of rows
	// compressing to 8.5 KB were observed emitting *nothing* until Close, so a
	// writer that only counted would never roll and would then produce one
	// oversized object that the API server rejects.
	//
	// Flushing after every row would make the count exact and force a block
	// boundary per row, which wrecks the ratio on a corpus of tens of
	// thousands of near-identical rows. Flushing per 64 KiB of input costs a
	// handful of boundaries per shard and bounds the overshoot instead.
	//
	// The boundaries are not free: on the 1,920 row fixture the flushes cost
	// about 11% of compressed size. That buys nothing back on any corpus that
	// exists, because none of them is near the bound — a two hour per-GPU GEMM
	// soak is 343 KiB against a 768 KiB roll, so 11% more is still one shard.
	// The ratio would only matter for the 480,000 row per-link corpus in
	// ADR-090's table, which has no producer. Correctness first.
	//
	// The arithmetic that keeps a shard inside MaxConfigMapBytes: the roll
	// fires at RollBytes measured on an accurate count, then at most
	// flushEveryRawBytes of buffered input and one further row of
	// MaxRowRawBytes can still land. Taking the worst case that neither
	// compresses at all, 768 + 64 + 128 KiB is 960 KiB, leaving 64 KiB for the
	// plaintext data block that dataBlockReserve holds back.
	flushEveryRawBytes = 64 * 1024

	// maxDroppedRecords bounds the per-row truncation ledger.
	//
	// Past the budget every further row is a dropped row, so the ledger grows
	// with the size of the overrun and not with the budget that was supposed
	// to bound the Builder. At a few hundred compressed bytes each, a few
	// hundred thousand records make a truncation object the API server
	// refuses, and then the record of what was lost is itself lost.
	//
	// 1,000 named records is far more than a reader needs to see which
	// components are affected — a node carries four to eight GPUs, so this
	// covers over a hundred nodes — and the exact total is carried separately,
	// so nothing is misreported: the ledger says "these, and this many more".
	//
	// The figure is measured, not estimated. At 2,000 records whose three
	// bounded fields are each full of high-entropy text, EncodeTruncation
	// produced 601,482 bytes against a 1,044,480 byte budget: correct, but 58%
	// consumed, and Build's contract has the caller append its own records for
	// rows lost to log pagination. Another 1,400 of those would push the object
	// over and it would not be written at all, which is the failure the cap
	// exists to prevent, moved one layer out. 1,000 measures around 300 KB and
	// leaves room for that caller.
	maxDroppedRecords = 1000

	// maxDroppedFieldBytes bounds each string copied into a truncation record.
	// A row dropped for being too large is exactly the row whose own fields may
	// be enormous, so copying them verbatim reproduces the problem being
	// recorded. 128 bytes still identifies a node and a GPU UUID.
	maxDroppedFieldBytes = 128
)

// Truncation reasons.
//
// The ComponentTruncation CRD enum in api/v1alpha1 is a SUPERSET of these: it
// also carries the reasons the controller raises rather than the encoder
// (LogUnpageable, ShardWritePartial, ProducerUnavailable, SealTimedOut). These
// four must stay a subset of it, or a seal carrying one is refused by the API
// server. Nothing checks the containment in either direction.
const (
	// ReasonRowTooLarge is a single row whose encoded form exceeds
	// MaxRowRawBytes. It cannot be written to an empty shard either, so
	// retrying it would hold the Workflow open forever.
	ReasonRowTooLarge = "RowTooLarge"
	// ReasonRowBudgetExceeded is a producer that emitted more than
	// MaxTotalRawBytes of rows.
	ReasonRowBudgetExceeded = "RowBudgetExceeded"
	// ReasonShardLimitExceeded is a row set that would need more than
	// MaxShardsPerJob shards.
	ReasonShardLimitExceeded = "ShardLimitExceeded"
	// ReasonRowUnencodable is a row carrying a value JSON cannot represent,
	// which in practice means NaN or an infinity.
	ReasonRowUnencodable = "RowUnencodable"
)

// Dropped records one row that could not be stored, with enough of the row's
// identity to say which component lost evidence.
type Dropped struct {
	Node        string `json:"node"`
	ComponentID string `json:"componentID"`
	Index       int32  `json:"index"`
	Name        string `json:"name"`
	Sample      int64  `json:"sample"`
	Reason      string `json:"reason"`
	RawBytes    int    `json:"rawBytes"`
	// Count is how many rows this component lost for this reason. The ledger
	// holds one record per component rather than one per row, so that a
	// bounded ledger still names every affected component; Count carries the
	// magnitude that collapsing them would otherwise lose. Name and Sample are
	// the first lost row's, as an example rather than an enumeration.
	Count int64 `json:"count"`
	// Message is free text explaining a loss the record cannot express on its
	// own. The encoder's own drops leave it empty: the reason and the row
	// identity say everything about them. It is here for the records the
	// controller appends, where the reason alone does not tell a bundle reader
	// why a read came back short, and the bundle is the only thing that leaves
	// the cluster.
	Message string `json:"message,omitempty"`
}

// Shard is one ConfigMap's worth of compressed rows.
type Shard struct {
	// Index is the shard's position in the Job's set, from zero.
	Index int
	// Payload is the gzipped NDJSON for binaryData.
	Payload []byte
	// Rows is how many rows Payload holds.
	Rows int64
	// SHA256 is the hex digest of Payload. It is carried in the shard's
	// plaintext data block so a reader can tell a shard mutated in the cluster
	// from one mutated on disk after the bundle was written.
	SHA256 string
}

// Result is everything one Job's rows produced.
type Result struct {
	// Shards are in emission order; Shards[i].Index == i.
	Shards []Shard
	// Rows is the number of rows actually stored, across every shard.
	Rows int64
	// Dropped names rows that could not be stored, up to maxDroppedRecords of
	// them. Never collapsed to a count alone: a run that lost evidence has to
	// be able to say whose.
	Dropped []Dropped
	// DroppedTotal is how many rows were dropped in all, including any beyond
	// the ledger's bound. It is the exact figure; len(Dropped) is the named
	// subset. Keeping both means a bounded ledger never makes the loss look
	// smaller than it was.
	DroppedTotal int64
}

// Options tunes a Builder. The zero value means "use the ADR defaults", so
// callers that do not care can pass Options{}.
type Options struct {
	// RollBytes overrides RollBytes.
	RollBytes int
	// MaxRowRawBytes overrides MaxRowRawBytes.
	MaxRowRawBytes int
	// MaxTotalRawBytes overrides MaxTotalRawBytes.
	MaxTotalRawBytes int
	// MaxShardsPerJob overrides MaxShardsPerJob. Settable mainly so the limit
	// can be reached in a test without encoding ten thousand shards; a branch
	// that cannot be exercised is a branch nothing defends.
	MaxShardsPerJob int
}

func (o Options) rollBytes() int {
	if o.RollBytes > 0 {
		return o.RollBytes
	}
	return RollBytes
}

func (o Options) maxRowRawBytes() int {
	if o.MaxRowRawBytes > 0 {
		return o.MaxRowRawBytes
	}
	return MaxRowRawBytes
}

func (o Options) maxTotalRawBytes() int {
	if o.MaxTotalRawBytes > 0 {
		return o.MaxTotalRawBytes
	}
	return MaxTotalRawBytes
}

func (o Options) maxShardsPerJob() int {
	if o.MaxShardsPerJob > 0 {
		return o.MaxShardsPerJob
	}
	return MaxShardsPerJob
}

// Builder accumulates a Job's rows and encodes them into shards.
//
// It is not safe for concurrent use; one Builder belongs to one Job attempt.
type Builder struct {
	opts         Options
	lines        [][]byte
	rows         []Row
	rawBytes     int
	dropped      []Dropped
	droppedTotal int64
	// droppedAt indexes dropped by component, so a second loss from the same
	// component increments its record rather than appending a new one.
	droppedAt map[componentKey]int
}

// NewBuilder returns a Builder using opts, with the ADR defaults for any field
// opts leaves at zero.
//
// It returns an error for options that cannot produce a writable shard. The
// roll bound, the flush cadence and the row cap together decide the largest
// shard the writer can emit, and only the first and last are settable, so a
// caller raising the row cap alone silently invalidates the arithmetic that
// keeps a shard inside the object budget. Catching it here beats catching it
// after a Job's rows have been gathered, where the only remaining answer is to
// fail a result set that was collected correctly.
func NewBuilder(opts Options) (*Builder, error) {
	worst := opts.rollBytes() + flushEveryRawBytes + opts.maxRowRawBytes() + dataBlockReserve
	if worst > MaxConfigMapBytes {
		return nil, fmt.Errorf(
			"shard options cannot fit a ConfigMap: roll bound %d plus flush cadence %d plus row cap %d plus %d reserved is %d, over the %d byte budget",
			opts.rollBytes(), flushEveryRawBytes, opts.maxRowRawBytes(), dataBlockReserve, worst, MaxConfigMapBytes)
	}
	return &Builder{opts: opts}, nil
}

// Add encodes a row and holds it for Build.
//
// Every row that cannot be stored is dropped and recorded rather than returned
// as an error, because an error here fails the Job's whole result set. That
// covers size, budget, and values JSON cannot represent.
//
// The last of those is not hypothetical. A harness that divides a work total
// by an elapsed time produces NaN when a device reports no elapsed time, and
// an infinity when it reports zero. JSON has no literal for either, so the
// encoder rejects the row. Letting that surface as an error would mean one
// device's arithmetic sinks every other device's results and the Job requeues
// on a row that can never be written.
//
// An error from Add therefore means the encoder failed on something other than
// the value, which is a bug rather than data.
func (b *Builder) Add(r Row) error {
	if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) {
		b.drop(r, ReasonRowUnencodable, 0)
		return nil
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode row for %s/%s: %w", r.Node, r.ComponentID, err)
	}
	line = append(line, '\n')

	if len(line) > b.opts.maxRowRawBytes() {
		b.drop(r, ReasonRowTooLarge, len(line))
		return nil
	}
	if b.rawBytes+len(line) > b.opts.maxTotalRawBytes() {
		// Every row past the budget is recorded, not just the first, so the
		// truncation set names each component that lost evidence rather than
		// only saying that something was lost.
		//
		// Which rows survive is decided here, at Add, and so follows the
		// producer's emission order rather than the normative sort order that
		// Build applies afterwards. That is inherent to a budget enforced
		// while reading: the sort cannot run until every row is in hand, which
		// is the thing the budget exists to avoid.
		b.drop(r, ReasonRowBudgetExceeded, len(line))
		return nil
	}

	b.rawBytes += len(line)
	b.lines = append(b.lines, line)
	b.rows = append(b.rows, r)
	return nil
}

// drop records a row that could not be stored.
//
// Two bounds apply, and both exist because the truncation ledger was otherwise
// the one path into a ConfigMap with no size limit on it at all. A producer
// that overruns the row budget has every subsequent row recorded here, so the
// ledger grows with the overrun rather than with the budget: a run emitting
// millions of rows past the budget would retain millions of records and then
// try to write them as a single object the API server refuses. The object that
// says which components lost evidence must not be the object that cannot be
// written.
//
// So each record's copied strings are truncated, since they are already a
// reference to lost evidence rather than the evidence itself, and the ledger
// stops growing past maxDroppedRecords, counting the remainder instead. What
// is kept is one named record per component for the first several thousand
// losses, which is what a reader actually needs, plus an exact total.
func (b *Builder) drop(r Row, reason string, raw int) {
	b.droppedTotal++

	// Keyed on the component, not on arrival.
	//
	// Keeping the first N records in arrival order looks equivalent and is
	// not. The only reason that fires at scale, RowBudgetExceeded, is recorded
	// here in the producer's emission order, so the kept records are
	// contiguous in that order and all come from the handful of components
	// straddling the budget boundary. Measured on a 480,000-row corpus: 416
	// nodes lost rows and an arrival-ordered ledger named 3 of them, 17
	// components out of 3,328. ADR-090 requires a dropped row to be counted
	// "with its component reference", and 0.5% of them carried one.
	//
	// One entry per component, with a count, names every affected component in
	// the same object budget. The per-reason total is unchanged, and
	// DroppedTotal is still exact.
	key := componentKey{node: r.Node, componentID: r.ComponentID, index: r.Index}
	if at, seen := b.droppedAt[key]; seen {
		b.dropped[at].Count++
		return
	}
	if len(b.dropped) >= maxDroppedRecords {
		return
	}
	if b.droppedAt == nil {
		b.droppedAt = make(map[componentKey]int)
	}
	b.droppedAt[key] = len(b.dropped)
	rec := droppedRecord(r, reason, raw)
	rec.Count = 1
	b.dropped = append(b.dropped, rec)
}

// componentKey identifies one component for the truncation ledger.
type componentKey struct {
	node        string
	componentID string
	index       int32
}

// droppedRecord builds one truncation record, with the copied strings bounded.
func droppedRecord(r Row, reason string, raw int) Dropped {
	return Dropped{
		Node:        truncateField(r.Node),
		ComponentID: truncateField(r.ComponentID),
		Index:       r.Index,
		Name:        truncateField(r.Name),
		Sample:      r.Sample,
		Reason:      reason,
		RawBytes:    raw,
	}
}

// truncateField bounds a string copied into a truncation record. A row that
// was dropped for being too large is precisely the row whose own fields may be
// enormous, so copying them verbatim reproduces the problem in the ledger.
//
// The cut lands on a rune boundary. Slicing at a byte offset can split a
// multi-byte rune, and json.Marshal silently rewrites the resulting invalid
// bytes to U+FFFD, so a metric name carrying any non-ASCII text would be
// quietly corrupted in the one record whose job is to say what was lost.
func truncateField(s string) string {
	if len(s) <= maxDroppedFieldBytes {
		return s
	}
	cut := maxDroppedFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// Build sorts the accumulated rows into the normative emission order and
// encodes them into shards.
//
// The output is a pure function of the rows added: the sort key is total, the
// gzip level is fixed, and the gzip header carries no timestamp. Re-running
// Build on the same rows produces byte-identical shards, which is what lets a
// failed write be retried by rewriting the same object, and what lets the
// bundle manifest's per-file sha256 mean anything.
func (b *Builder) Build() (Result, error) {
	// Clone rather than hand back the Builder's own slice. ADR-090 has the
	// caller append its own truncation records to this set, for rows lost to
	// log pagination, and an append into the Builder's spare capacity is
	// overwritten by the next Add without any error anywhere.
	res := Result{Dropped: slices.Clone(b.dropped), DroppedTotal: b.droppedTotal}
	if len(b.lines) == 0 {
		return res, nil
	}

	// Sort rows and lines together: the line is the row's encoded form, so
	// they have to stay paired.
	order := make([]int, len(b.rows))
	for i := range order {
		order[i] = i
	}
	// The normative key first, then the encoded bytes as the terminal
	// tiebreak. Two rows that tie on the key are ordered by what will actually
	// be stored, so the emission order is a function of the shard's own bytes
	// rather than of a field-by-field proxy for them — and the sort stays
	// total however Row grows.
	slices.SortFunc(order, func(x, y int) int {
		if c := rowCompare(b.rows[x], b.rows[y]); c != 0 {
			return c
		}
		return bytes.Compare(b.lines[x], b.lines[y])
	})

	enc, err := newShardEncoder()
	if err != nil {
		return Result{}, err
	}
	for i, idx := range order {
		if err := enc.write(b.lines[idx]); err != nil {
			return Result{}, err
		}
		rolled, err := enc.shouldRoll(b.opts.rollBytes())
		if err != nil {
			return Result{}, err
		}
		if !rolled {
			continue
		}
		shard, err := enc.finish(len(res.Shards), b.opts)
		if err != nil {
			return Result{}, err
		}
		res.Shards = append(res.Shards, shard)
		res.Rows += shard.Rows
		// Stop only when the limit is reached AND rows remain. Testing it
		// without that second condition reports a complete result set as
		// truncated whenever the row count happens to land exactly on the
		// limit.
		if len(res.Shards) >= b.opts.maxShardsPerJob() {
			// Name every row that is being abandoned, rather than emitting one
			// anonymous record. Result.Dropped promises a reader can say whose
			// evidence was lost, and a record with no component on it cannot.
			//
			// Recorded onto the Result, never back onto the Builder: Build is
			// documented as a pure function of the rows added, and the golden
			// test calls it twice to check determinism, so mutating the
			// ledger here would make the second call report twice the loss.
			//
			// There is deliberately no "and rows remain" condition. An earlier
			// version carried one, justified as preventing a complete result
			// set from being reported as truncated when the row count lands
			// exactly on the limit. That justification was wrong and the guard
			// was dead: when the last row has just been written the loop below
			// iterates over nothing, and the early return skips only a
			// freshly created zero-row encoder that the trailing
			// `if enc.rows > 0` would have discarded anyway. Both paths return
			// an identical Result, confirmed over 165 corpora.
			for _, j := range order[i+1:] {
				res.DroppedTotal++
				if len(res.Dropped) >= maxDroppedRecords {
					continue
				}
				res.Dropped = append(res.Dropped,
					droppedRecord(b.rows[j], ReasonShardLimitExceeded, len(b.lines[j])))
			}
			return res, nil
		}
		if enc, err = newShardEncoder(); err != nil {
			return Result{}, err
		}
	}
	if enc.rows > 0 {
		shard, err := enc.finish(len(res.Shards), b.opts)
		if err != nil {
			return Result{}, err
		}
		res.Shards = append(res.Shards, shard)
		res.Rows += shard.Rows
	}
	return res, nil
}

// shardEncoder is one shard's gzip stream plus the counter that decides when
// to roll.
type shardEncoder struct {
	buf           bytes.Buffer
	cw            *countingWriter
	zw            *gzip.Writer
	rows          int64
	rawSinceFlush int
}

func newShardEncoder() (*shardEncoder, error) {
	e := &shardEncoder{}
	e.cw = &countingWriter{w: &e.buf}
	// BestCompression matches pkg/controller/compress/gzip.go, which the
	// node-result ConfigMaps already use. The Writer's zero Header.ModTime is
	// what keeps the output deterministic; do not set Name or Comment either.
	zw, err := gzip.NewWriterLevel(e.cw, gzip.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("create gzip writer: %w", err)
	}
	e.zw = zw
	return e, nil
}

func (e *shardEncoder) write(line []byte) error {
	if _, err := e.zw.Write(line); err != nil {
		return fmt.Errorf("write row to shard: %w", err)
	}
	e.rows++
	e.rawSinceFlush += len(line)
	return nil
}

// shouldRoll reports whether this shard has reached the roll bound.
//
// It flushes first when enough input has accumulated since the last flush,
// because the counter only sees what gzip has emitted and flate buffers.
// Without the flush the count can sit at zero for an entire shard, the roll
// never fires, and the stream ends as one object too large to write.
func (e *shardEncoder) shouldRoll(rollBytes int) (bool, error) {
	if e.rawSinceFlush >= flushEveryRawBytes {
		if err := e.zw.Flush(); err != nil {
			return false, fmt.Errorf("flush shard: %w", err)
		}
		e.rawSinceFlush = 0
	}
	return e.cw.n >= rollBytes, nil
}

func (e *shardEncoder) finish(index int, opts Options) (Shard, error) {
	if err := e.zw.Close(); err != nil {
		return Shard{}, fmt.Errorf("close gzip writer: %w", err)
	}
	payload := e.buf.Bytes()
	if len(payload)+dataBlockReserve > MaxConfigMapBytes {
		// Unreachable: NewBuilder rejects options whose roll bound, flush
		// cadence and row cap could sum past the budget, and gzip expands
		// incompressible input by well under a kilobyte at these sizes.
		// Asserted rather than trusted, because the API server's rejection is
		// a 422 Invalid carrying field.TooLong, not the 413 a caller would
		// think to handle, and a silently short result set is the one outcome
		// ADR-090 exists to prevent.
		//
		// The message reports the bounds actually in force, not the package
		// defaults; reporting the defaults would point whoever debugs this at
		// a knob that was not the one set.
		return Shard{}, fmt.Errorf(
			"shard %d is %d bytes, over the %d byte ConfigMap budget: roll bound %d and row cap %d are misconfigured",
			index, len(payload), MaxConfigMapBytes-dataBlockReserve, opts.rollBytes(), opts.maxRowRawBytes())
	}
	sum := sha256.Sum256(payload)
	return Shard{
		Index:   index,
		Payload: payload,
		Rows:    e.rows,
		SHA256:  hex.EncodeToString(sum[:]),
	}, nil
}

// countingWriter counts the bytes written through it. It wraps the gzip
// stream's output, so it counts compressed bytes, which is what the API
// server's limit is measured in.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}
