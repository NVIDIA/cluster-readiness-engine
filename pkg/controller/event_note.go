// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"unicode/utf8"
)

const (
	maxEventNoteBytes         = 1024
	eventNoteTruncationSuffix = "... [truncated]"
)

// formatEventNote bounds the formatted note to the events/v1 API limit.
// Status messages remain intact; only the recorder's copy is shortened.
// Callers must pass the result through a literal "%s" recorder format.
func formatEventNote(format string, args ...any) string {
	note := fmt.Sprintf(format, args...)
	if len(note) <= maxEventNoteBytes {
		return note
	}
	end := maxEventNoteBytes - len(eventNoteTruncationSuffix)
	// Only shorten further when a valid rune actually crosses the cutoff.
	// Invalid continuation-byte runs are binary data, not an unbounded rune.
	for start := end - 1; start >= 0 && end-start < utf8.UTFMax; start-- {
		if !utf8.RuneStart(note[start]) {
			continue
		}
		_, size := utf8.DecodeRuneInString(note[start:])
		if start+size > end {
			end = start
		}
		break
	}
	return note[:end] + eventNoteTruncationSuffix
}
