// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// FuzzFailureLogExcerpt asserts the invariants failureLogExcerpt must hold for
// any input. Its argument is Job.status.failureLog.tail, which is the verbatim
// stdout of a user-supplied training container, so it is the one string in the
// report path that an untrusted workload controls byte for byte.
//
// This complements rather than replaces TestFailureLogExcerptLimits: the golden
// test pins exact output for chosen inputs, while this pins the properties that
// must survive inputs nobody chose. The properties are the same ones asserted at
// the top of that test, plus the sanitizer guarantee that no control or bidi
// rune reaches an operator's terminal.
func FuzzFailureLogExcerpt(f *testing.F) {
	f.Add("")
	f.Add("plain line\n")
	f.Add("\x1b[31mred\x1b[0m")
	f.Add("a\x9bbad-csi")
	f.Add("before\u202eafter\u2069") // bidi override + pop isolate
	f.Add("\xff\xfe invalid utf8")
	f.Add("tab\there\r\nmix\rcr")
	f.Add(strings.Repeat("x", 5000))
	f.Add(strings.Repeat("é", failureLogHumanMaxBytes))
	f.Add(strings.Repeat("line\n", failureLogHumanMaxLines*2))

	f.Fuzz(func(t *testing.T, tail string) {
		lines, _ := failureLogExcerpt(tail)

		require.LessOrEqual(t, len(lines), failureLogHumanMaxLines)
		for _, line := range lines {
			require.True(t, utf8.ValidString(line), "line is not valid UTF-8: %q", line)
			require.LessOrEqual(t, len(line), wrappedTextMaxLineBytes, "line exceeds byte backstop: %q", line)
			for _, r := range line {
				require.Falsef(t,
					r < ' ' || (r >= 0x7f && r <= 0x9f) || unicode.Is(unicode.Bidi_Control, r),
					"unsanitized rune %U survived into %q", r, line)
			}
		}
	})
}
