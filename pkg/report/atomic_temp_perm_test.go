// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"os"
	"testing"
)

// TestTempPermForClosesTheWidenWindow pins the creation mode of a replacement.
//
// This is tested on its own because the window it closes cannot be seen in the
// finished file: replacing a 0600 destination ends at 0600 whether the
// temporary file was created at 0600 or created at 0644 and narrowed
// afterwards. Only the creation mode distinguishes them, and in the second
// case another local user can open the temporary file during the interval and
// keep the descriptor, which still reads the report after the rename.
func TestTempPermForClosesTheWidenWindow(t *testing.T) {
	t.Run("replacing an existing file", func(t *testing.T) {
		got := tempPermFor(true)
		if got&0o077 != 0 {
			t.Errorf("tempPermFor(true) = %v, which grants group or other access before "+
				"the destination's mode is applied", got)
		}
	})

	// A new destination keeps the default, so umask continues to decide for a
	// file on which nobody had set a policy.
	t.Run("creating a new file", func(t *testing.T) {
		if got := tempPermFor(false); got != reportFilePerm {
			t.Errorf("tempPermFor(false) = %v, want the default %v", got, reportFilePerm)
		}
	})

	// And the mode is honoured on disk, not merely chosen.
	t.Run("honoured by createTemp", func(t *testing.T) {
		dir := t.TempDir()
		f, err := createTemp(dir, "results.json", tempPermFor(true))
		if err != nil {
			t.Fatalf("createTemp: %v", err)
		}
		defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
		info, err := os.Stat(f.Name())
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("the temporary file is %v on disk", got)
		}
	})
}
