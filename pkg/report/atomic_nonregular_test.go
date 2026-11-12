// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestWriteFileAtomicKeepsNonRegularDestinations pins that a destination which
// is not a regular file is written in place.
//
// Moving every report path onto temp-file-plus-rename silently dropped a shape
// that used to work. `--results-file /dev/stdout` and a FIFO consumed by
// another process both went through os.WriteFile before, and under a rename
// they fail: a rename REPLACES the destination, so the device node or pipe is
// swapped for a regular file. Running as root, `--junit-file /dev/null` would
// have replaced /dev/null for every other process on the machine.
//
// Atomicity has nothing to offer here either. It exists so a reader never sees
// a half-written report, and a reader of a stream consumes bytes as they
// arrive; there is no file for it to observe in two states.
//
// An assertion test rather than a golden one: the subject is the destination's
// mode after the call, which is a single fact about the filesystem and not
// structured output.
func TestWriteFileAtomicKeepsNonRegularDestinations(t *testing.T) {
	t.Run("fifo", func(t *testing.T) {
		fifo := filepath.Join(t.TempDir(), "pipe")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("this platform cannot create a FIFO: %v", err)
		}

		// Opened for reading concurrently, because opening a FIFO for writing
		// blocks until a reader arrives. This is also the real usage: the
		// point of pointing a report at a pipe is that something is draining
		// it.
		//
		// Every wait below is bounded, and that is not incidental. When this
		// guard regresses the write renames over the FIFO instead of opening
		// it, so the reader waits for a writer that never comes. An unbounded
		// receive here turned a failure into a hang that only ended at Go's
		// ten-minute test timeout, which in CI reads as a stuck job rather
		// than as this assertion failing.
		const wait = 15 * time.Second

		read := make(chan string, 1)
		go func() {
			b, err := os.ReadFile(fifo)
			if err != nil {
				read <- "read failed: " + err.Error()
				return
			}
			read <- string(b)
		}()

		wrote := make(chan error, 1)
		go func() { wrote <- WriteFileAtomic(fifo, []byte("hello")) }()

		select {
		case err := <-wrote:
			if err != nil {
				t.Fatalf("writing to a FIFO failed: %v", err)
			}
		case <-time.After(wait):
			t.Fatalf("WriteFileAtomic did not return within %s for a FIFO", wait)
		}

		// Checked before the payload: if the FIFO was replaced, the read below
		// can never complete and the mode is the thing that explains why.
		info, err := os.Lstat(fifo)
		if err != nil {
			t.Fatalf("lstat the FIFO: %v", err)
		}
		if info.Mode().IsRegular() {
			t.Fatal("the FIFO was replaced by a regular file, so the reader on the " +
				"other end is now waiting on a pipe nothing will ever write to")
		}

		select {
		case got := <-read:
			if got != "hello" {
				t.Errorf("the FIFO's reader got %q, want %q", got, "hello")
			}
		case <-time.After(wait):
			t.Errorf("the FIFO's reader saw nothing within %s", wait)
		}
	})

	// /dev/null rather than a temp char device, because creating one needs
	// root and this is the destination an operator actually passes.
	t.Run("character device", func(t *testing.T) {
		if _, err := os.Lstat(os.DevNull); err != nil {
			t.Skipf("no %s on this platform: %v", os.DevNull, err)
		}
		if err := WriteFileAtomic(os.DevNull, []byte("discarded")); err != nil {
			t.Fatalf("writing to %s failed: %v", os.DevNull, err)
		}
		info, err := os.Lstat(os.DevNull)
		if err != nil {
			t.Fatalf("lstat %s: %v", os.DevNull, err)
		}
		if info.Mode().IsRegular() {
			t.Fatalf("%s was replaced by a regular file", os.DevNull)
		}
	})
}
