// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shard

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	// compressutil is the repository's shared gzip helper. It is named rather
	// than aliased to "gzip" because builder.go in this same package imports
	// the standard library's gzip under that name, and one identifier meaning
	// two things inside one package is a trap.
	compressutil "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
)

const (
	// FormatVersion is the shard payload's schema version. A reader that does
	// not recognise it refuses the shard rather than guessing.
	FormatVersion = 1

	// RowsKey is the binaryData key holding gzipped NDJSON rows, following the
	// naming of pkg/noderesults' succeeded-nodes.csv.gz.
	RowsKey = "rows.ndjson.gz"

	// TruncationKey is the binaryData key holding gzipped NDJSON records of
	// rows that could not be stored. It lives on its own ConfigMap, outside
	// the shard set, so the sealed shard count stays a clean equality.
	TruncationKey = "truncation.ndjson.gz"

	// Plaintext data keys. These sit in Data rather than BinaryData so a
	// reader, or a human with kubectl, can check a shard without decompressing
	// it. They count against the same per-object budget, which dataBlockReserve
	// holds back.
	FormatVersionKey = "formatVersion"
	ShardIndexKey    = "shardIndex"
	RowCountKey      = "rowCount"
	DigestKey        = "sha256"
)

// ErrNoRows reports a ConfigMap carrying no rows payload at all, which is
// different from a payload that failed to decode. Distinguishing them is the
// point: FailedNodesFromRef returns nil for both, so an unreadable ConfigMap
// is indistinguishable from nothing to report, and ADR-090 states that
// conflation is not acceptable on the primary evidence path.
var ErrNoRows = errors.New("configmap carries no rows payload")

// Data returns the plaintext data block for a shard.
func (s Shard) Data() map[string]string {
	return map[string]string{
		FormatVersionKey: strconv.Itoa(FormatVersion),
		ShardIndexKey:    strconv.Itoa(s.Index),
		RowCountKey:      strconv.FormatInt(s.Rows, 10),
		DigestKey:        s.SHA256,
	}
}

// BinaryData returns the binaryData block for a shard.
func (s Shard) BinaryData() map[string][]byte {
	return map[string][]byte{RowsKey: s.Payload}
}

// DecodeRows reads the rows out of a shard ConfigMap.
//
// It verifies the payload against the digest the writer recorded, so a shard
// edited in the cluster is caught here rather than being carried into a bundle
// and certified by its manifest.
func DecodeRows(cm *corev1.ConfigMap) ([]Row, error) {
	if cm == nil {
		return nil, ErrNoRows
	}
	raw := cm.BinaryData[RowsKey]
	if len(raw) == 0 {
		return nil, ErrNoRows
	}
	// The three checks below are required, not conditional on the object
	// carrying the key they read.
	//
	// Making them conditional let an edit that replaced the payload AND
	// deleted the sha256 key pass clean, which is the one edit someone
	// tampering would actually make. Shard.Data writes these three on every
	// shard, so a payload without them was not written by this package and
	// must not be read as though it were.
	//
	// shardIndex is deliberately NOT among them, and is advisory here. It is
	// written by Shard.Data but neither required nor range-checked, and the
	// digest covers the payload only, not the data block — so a renumbered or
	// duplicated index reads clean. Bundle assembly is what orders files by
	// it, so that caller must verify it equals the shard's expected position;
	// this function cannot, because it sees one shard at a time and does not
	// know how many there should be.
	version := cm.Data[FormatVersionKey]
	if version == "" {
		return nil, fmt.Errorf("shard %s carries a payload but no %s", cm.Name, FormatVersionKey)
	}
	if version != strconv.Itoa(FormatVersion) {
		return nil, fmt.Errorf("shard %s has format version %s, want %d", cm.Name, version, FormatVersion)
	}
	wantDigest := cm.Data[DigestKey]
	if wantDigest == "" {
		return nil, fmt.Errorf("shard %s carries a payload but no %s", cm.Name, DigestKey)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != wantDigest {
		return nil, fmt.Errorf("shard %s payload digest is %s, recorded as %s", cm.Name, got, wantDigest)
	}

	decoded, err := decompressBounded(raw)
	if err != nil {
		return nil, fmt.Errorf("decompress shard %s: %w", cm.Name, err)
	}
	rows, err := DecodeNDJSON(decoded)
	if err != nil {
		return nil, fmt.Errorf("decode shard %s: %w", cm.Name, err)
	}
	wantRows := cm.Data[RowCountKey]
	if wantRows == "" {
		return nil, fmt.Errorf("shard %s carries a payload but no %s", cm.Name, RowCountKey)
	}
	n, convErr := strconv.ParseInt(wantRows, 10, 64)
	if convErr != nil {
		return nil, fmt.Errorf("shard %s has unparseable row count %q: %w", cm.Name, wantRows, convErr)
	}
	if n != int64(len(rows)) {
		return nil, fmt.Errorf("shard %s holds %d rows, recorded as %d", cm.Name, len(rows), n)
	}
	return rows, nil
}

// maxDecompressedBytes bounds the plaintext a single shard may expand to.
//
// The payload comes from a ConfigMap, which anything with write access to the
// namespace can replace, and gzip expands highly repetitive input by three
// orders of magnitude. Decompressing without a bound lets a 1 MiB object
// become gigabytes of heap in a process with a 1Gi limit.
//
// 32 MiB, not 256. Honest data at ADR-090's own worst measured ratio of 34x
// puts a shard at the roll bound at about 26 MiB, so this covers it. The
// earlier 256 MiB was both unnecessary and worse than its own figure: with
// io.ReadAll the growing slice and its reallocation held 573 MiB of heap for
// one read, 2.25x the bound, in a process limited to 1Gi.
const maxDecompressedBytes = 32 * 1024 * 1024

// decompressBounded gunzips through a limit, and reports a payload that hits
// it rather than returning the truncated prefix as though it were the whole.
func decompressBounded(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("open gzip reader: %w", err)
	}
	defer func() { _ = zr.Close() }()

	// Pre-sized rather than io.ReadAll, which doubles its slice as it grows
	// and so holds the old and new arrays at once — the reason the previous
	// bound cost more than twice its stated figure. One byte of headroom, so a
	// payload that hits the limit is distinguishable from one that is exactly
	// the limit.
	var out bytes.Buffer
	out.Grow(min(len(raw)*8, maxDecompressedBytes+1))
	if _, err := io.Copy(&out, io.LimitReader(zr, maxDecompressedBytes+1)); err != nil {
		return nil, fmt.Errorf("read gzip data: %w", err)
	}
	if out.Len() > maxDecompressedBytes {
		return nil, fmt.Errorf("payload expands past %d bytes, refusing to decompress further", maxDecompressedBytes)
	}
	return out.Bytes(), nil
}

// DecodeNDJSON parses newline-delimited rows.
func DecodeNDJSON(b []byte) ([]Row, error) {
	var rows []Row
	sc := bufio.NewScanner(bytes.NewReader(b))
	// The ceiling is the object budget, not MaxRowRawBytes.
	//
	// Sizing it from the row cap couples the reader to a writer-side default
	// that Options lets a caller raise: a Builder with a larger cap produced a
	// well formed shard, with a correct digest and row count, that this
	// function then refused with "token too long". The evidence reached the
	// cluster and became permanently unreadable, and the failure surfaced at
	// bundle time rather than at write time.
	//
	// No row a Builder can admit reaches this bound, whatever the options:
	// NewBuilder refuses any combination whose worst case leaves a shard over
	// the budget, so a single row's raw form is always well under it.
	sc.Buffer(make([]byte, 0, 64*1024), MaxConfigMapBytes)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var r Row
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan rows: %w", err)
	}
	return rows, nil
}

// EncodeTruncation compresses dropped-row records for the truncation
// ConfigMap. It returns nil when there is nothing to record, so a caller can
// skip creating the object entirely.
//
// The result is asserted against the object budget. This was the one path into
// a ConfigMap with no bound on it: the ledger grows with the size of a
// producer's overrun, and a large enough one produced an object the API server
// refuses, so the record of what was lost became the thing that was lost. The
// Builder now bounds both the record count and the strings inside each record,
// which makes this assertion unreachable; it is kept because an unwritable
// truncation object is a silent failure and a returned error is not.
func EncodeTruncation(dropped []Dropped) ([]byte, error) {
	if len(dropped) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	for _, d := range dropped {
		b, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("encode truncation record: %w", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	out, err := compressutil.GzipString(buf.String())
	if err != nil {
		return nil, fmt.Errorf("compress truncation records: %w", err)
	}
	if len(out)+dataBlockReserve > MaxConfigMapBytes {
		return nil, fmt.Errorf(
			"truncation record for %d dropped rows is %d bytes, over the %d byte ConfigMap budget",
			len(dropped), len(out), MaxConfigMapBytes-dataBlockReserve)
	}
	return out, nil
}

// DecodeTruncation reads dropped-row records written by EncodeTruncation.
func DecodeTruncation(b []byte) ([]Dropped, error) {
	if len(b) == 0 {
		return nil, nil
	}
	decoded, err := decompressBounded(b)
	if err != nil {
		return nil, fmt.Errorf("decompress truncation records: %w", err)
	}
	var out []Dropped
	sc := bufio.NewScanner(bytes.NewReader(decoded))
	sc.Buffer(make([]byte, 0, 4*1024), MaxConfigMapBytes)
	for line := 1; sc.Scan(); line++ {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var d Dropped
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan truncation records: %w", err)
	}
	return out, nil
}
