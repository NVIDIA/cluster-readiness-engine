// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podlogs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Page is one bounded read of a container log.
type Page struct {
	Lines []string

	// Truncated reports that the read stopped at LimitBytes, so the log may
	// continue past Lines.
	Truncated bool

	// PartialTail reports that the byte limit cut the last element of Lines
	// short. It is only ever true together with Truncated.
	PartialTail bool
}

// PageFetcher is a PodLogFetcher that can also report whether a read was cut
// off at LimitBytes. ReadAll needs that to page through a log larger than one
// read; a fetcher without it is read once.
type PageFetcher interface {
	FetchLogsPage(ctx context.Context, namespace, podName string, opts LogOptions) (Page, error)
}

const (
	// DefaultMaxPages bounds how many reads ReadAll issues for one log: 64
	// pages of DefaultMaxLogBytes is 512 MB, far past any NCCL test log.
	DefaultMaxPages = 64

	// maxPageGrowth bounds how far ReadAll raises LimitBytes when a single
	// second of output does not fit in one page.
	maxPageGrowth = 4
)

// ErrLogUnpageable is returned when a log cannot be read in full: it outgrew
// the page bound, one second of output outgrew the largest page, a line is
// longer than the scanner accepts, or its lines carry no timestamps to resume
// from. Retrying fails the same way.
var ErrLogUnpageable = errors.New("log cannot be read in full")

// ReadAllOptions configures ReadAll.
type ReadAllOptions struct {
	Container string

	// PageBytes caps each read. Zero means DefaultMaxLogBytes.
	PageBytes int64

	// MaxPages bounds the number of reads. Zero means DefaultMaxPages.
	MaxPages int
}

// ReadAll reads a container's current log from its first line to its last and
// passes each complete line to visit exactly once, in log order.
//
// A single read is capped server-side and keeps the head of the log, so a log
// larger than one page is read in pages. Each page after the first resumes
// from the timestamp of the last complete line. SinceTime is second-precise on
// the wire, so the kubelet returns every line at or after the start of that
// second; the lines already visited from that second are exactly the first
// ones of the new page, in the same order, and are skipped by count. A page
// cut mid-line drops the partial line so it is read whole next time.
//
// Built for logs that are no longer written to, such as a finished workload.
// A log that rotates between pages can lose or repeat lines.
func ReadAll(ctx context.Context, fetcher PodLogFetcher, namespace, podName string, opts ReadAllOptions, visit func(line string)) error {
	pageBytes := opts.PageBytes
	if pageBytes <= 0 {
		pageBytes = DefaultMaxLogBytes
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	pager, ok := fetcher.(PageFetcher)
	if !ok {
		lines, err := fetcher.FetchLogs(ctx, namespace, podName, LogOptions{Container: opts.Container, LimitBytes: pageBytes})
		if err != nil {
			return err
		}
		for _, line := range lines {
			visit(line)
		}
		return nil
	}

	var (
		since time.Time
		// seen holds the timestamps of visited lines at or after since, the
		// lines the next page repeats.
		seen  []time.Time
		limit = pageBytes
	)
	for range maxPages {
		read := LogOptions{Container: opts.Container, LimitBytes: limit}
		if !since.IsZero() {
			read.SinceTime = new(metav1.NewTime(since))
		}
		page, err := pager.FetchLogsPage(ctx, namespace, podName, read)
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("%w: %w", ErrLogUnpageable, err)
		}
		if err != nil {
			return err
		}

		lines := page.Lines
		if page.PartialTail && len(lines) > 0 {
			lines = lines[:len(lines)-1]
		}
		if !since.IsZero() {
			lines = lines[min(countAtOrAfter(seen, since), len(lines)):]
		}

		var last time.Time
		for _, line := range lines {
			ts, ok := LineTimestamp(line)
			if ok {
				seen = append(seen, ts)
				last = ts
			} else if page.Truncated {
				return fmt.Errorf("%w: line without a timestamp in a truncated page", ErrLogUnpageable)
			}
			visit(line)
		}

		if !page.Truncated {
			return nil
		}
		if len(lines) == 0 {
			// One second of output fills the whole page: nothing new can be
			// reached from here without a larger one.
			if limit >= pageBytes*maxPageGrowth {
				return fmt.Errorf("%w: more than %d bytes logged within one second", ErrLogUnpageable, limit)
			}
			limit *= 2
			continue
		}

		if next := last.Truncate(time.Second); next.After(since) {
			since = next
			seen = dropBefore(seen, since)
		}
	}
	return fmt.Errorf("%w: more than %d pages", ErrLogUnpageable, maxPages)
}

// LineTimestamp parses the RFC3339Nano timestamp the kubelet prefixes to each
// log line when timestamps are requested.
func LineTimestamp(line string) (time.Time, bool) {
	prefix, _, found := strings.Cut(line, " ")
	if !found {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

func countAtOrAfter(ts []time.Time, since time.Time) int {
	n := 0
	for _, t := range ts {
		if !t.Before(since) {
			n++
		}
	}
	return n
}

func dropBefore(ts []time.Time, since time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if !t.Before(since) {
			kept = append(kept, t)
		}
	}
	return kept
}
