// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"
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
	// Budget against the encoded form. encoding/json replaces every invalid
	// byte with U+FFFD, and strings.ToValidUTF8 does the same for each
	// isolated byte while collapsing a contiguous invalid run to one rune.
	// Either way the replacement is three bytes, so measuring the original
	// Go string can let the note the API server receives exceed 1,024 bytes.
	note := strings.ToValidUTF8(fmt.Sprintf(format, args...), "\uFFFD")
	if len(note) <= maxEventNoteBytes {
		return note
	}
	end := maxEventNoteBytes - len(eventNoteTruncationSuffix)
	// A valid rune may still straddle the cutoff. Walk back at most one rune.
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
