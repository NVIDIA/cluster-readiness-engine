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
		const payload = "hello"

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
		go func() { wrote <- WriteFileAtomic(fifo, []byte(payload)) }()

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
			if got != payload {
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

// TestWriteFileAtomicPreservesExistingPermissions pins that replacing a file
// does not widen its mode.
//
// A rename publishes the temporary file's permissions, and the temporary file
// is created 0644. os.WriteFile, which this replaced, left an existing file's
// mode alone, so a results file an operator had restricted to 0600 came back
// world-readable after the next run. Results are exactly the artifact worth
// restricting, which makes this a disclosure rather than an inconvenience.
// replacement is the payload these cases write.
const replacement = "new"

func TestWriteFileAtomicPreservesExistingPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{
		0o600, 0o640, 0o400,
		// 0000 must not be read as "no destination". Treating a zero mode as
		// absence replaced the file at the default and made an unreadable
		// report readable.
		0o000,
		// A mode equal to reportFilePerm must still be applied explicitly.
		// Skipping the chmod because the two matched left umask deciding, so
		// under a restrictive umask an existing 0644 file came back 0600.
		0o644,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "results.json")
			if err := os.WriteFile(path, []byte("old"), mode); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// Guard against a umask having already widened the seed, which
			// would make the assertion below vacuous.
			if before, err := os.Stat(path); err != nil {
				t.Fatalf("stat seed: %v", err)
			} else if before.Mode().Perm() != mode {
				t.Skipf("the filesystem gave the seed %v, not %v", before.Mode().Perm(), mode)
			}

			if err := WriteFileAtomic(path, []byte(replacement)); err != nil {
				t.Fatalf("write: %v", err)
			}

			after, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if got := after.Mode().Perm(); got != mode {
				t.Errorf("permissions changed from %v to %v; a reader who could not see "+
					"the previous file's contents can see these", mode, got)
			}
			// Only when the mode leaves the owner able to read it. A 0000
			// destination is a case this test exists for, and reading it back
			// fails for anyone but root, which would be the test's limitation
			// rather than the code's.
			if mode&0o400 != 0 {
				b, err := os.ReadFile(path)
				if err != nil || string(b) != replacement {
					t.Errorf("content not replaced: %q err=%v", b, err)
				}
			}
		})
	}

	// A file that does not exist yet gets the default, not 0000.
	t.Run("new file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "results.json")
		if err := WriteFileAtomic(path, []byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() == 0 {
			t.Error("a new report file has no permissions at all")
		}
	})
}

// TestSymlinkedStreamResolvesToItself pins that a stream reached through a
// symlink is not put through symlink resolution.
//
// This is the shape of `--results-file /dev/stdout` on Linux: /dev/stdout
// links to /proc/self/fd/1, which when stdout is a pipe links to
// "pipe:[12345]" — a kernel label, not a path. EvalSymlinks fails on it, and
// resolving the chain by hand reads that label as a relative target and joins
// it under /proc/<pid>/fd, producing a path that does not exist. The caller
// then takes it for a new file, tries to create a temporary file in
// /proc/<pid>/fd, and the write fails having produced nothing.
//
// Tested with a symlink to a FIFO because that is portable and exercises the
// same decision: the destination is not a regular file, so resolution has
// nothing to achieve and must not be attempted.
func TestSymlinkedStreamResolvesToItself(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this platform cannot create a FIFO: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("this platform cannot create a symlink: %v", err)
	}

	got, err := resolveDestination(link)
	if err != nil {
		t.Fatalf("resolveDestination: %v", err)
	}
	if got != link {
		t.Errorf("resolveDestination(%q) = %q; a stream must come back untouched, "+
			"because resolving it is what turns /dev/stdout into a path under "+
			"/proc/<pid>/fd that nothing can create a file in", link, got)
	}

	// And the write still reaches the reader on the other end.
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
	go func() { wrote <- WriteFileAtomic(link, []byte("streamed")) }()

	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("writing through a symlinked FIFO failed: %v", err)
		}
	case <-time.After(wait):
		t.Fatalf("WriteFileAtomic did not return within %s", wait)
	}
	select {
	case s := <-read:
		if s != "streamed" {
			t.Errorf("the reader got %q", s)
		}
	case <-time.After(wait):
		t.Error("the FIFO's reader saw nothing")
	}
}
