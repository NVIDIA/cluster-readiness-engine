// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// These pin the defects ADR-090 names on the export path, none of which had
// any test coverage: WriteJSON indexed reports[0] unguarded, and both report
// writers truncated the destination before writing, so an interrupted run left
// a file that exists and parses as far as it got.
//
// They also pin the three behaviours the atomic write changes relative to
// os.WriteFile — file mode under a umask, writing through a symlink, and what
// happens when the containing directory cannot be written — because each is a
// user-visible change that would otherwise only be found in the field.

func TestWriteJSONRejectsEmptyReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.json")

	// Before the guard this was a panic on reports[0], not an error.
	err := WriteJSON(path, nil)
	if err == nil {
		t.Fatal("expected an error for an empty report slice, got nil")
	}
	if !errors.Is(err, errNoReports) {
		t.Fatalf("expected errNoReports, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("a rejected write must not create the destination, stat gave %v", statErr)
	}
}

// TestWriteJUnitRejectsEmptyReports pins that the two writers agree about what
// zero reports means. WriteJUnit used to write <testsuites></testsuites>, which
// a CI system reads as a clean run — the "exit zero with no evidence" outcome
// the --wait guard exists to prevent.
func TestWriteJUnitRejectsEmptyReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.xml")
	err := WriteJUnit(path, nil)
	if err == nil {
		t.Fatal("expected an error for an empty report slice, got nil")
	}
	if !errors.Is(err, errNoReports) {
		t.Fatalf("expected errNoReports, got %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("a rejected write must not create the destination")
	}
}

func TestWriteJSONShapeFollowsReportCount(t *testing.T) {
	dir := t.TempDir()

	// One report is an object, several are an array. Consumers of
	// results.json depend on that distinction (ADR-071), so it is pinned
	// rather than left to the next person to infer.
	single := filepath.Join(dir, "one.json")
	if err := WriteJSON(single, []*CertReport{{Name: "a"}}); err != nil {
		t.Fatalf("write one: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(readFile(t, single), &obj); err != nil {
		t.Fatalf("one report should encode as an object: %v", err)
	}

	many := filepath.Join(dir, "two.json")
	if err := WriteJSON(many, []*CertReport{{Name: "a"}, {Name: "b"}}); err != nil {
		t.Fatalf("write two: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(readFile(t, many), &arr); err != nil {
		t.Fatalf("two reports should encode as an array: %v", err)
	}
	if len(arr) != 2 {
		t.Fatalf("expected two entries, got %d", len(arr))
	}
}

// TestWriteFileAtomicNeverExposesAPartialFile is the test for the property the
// change exists to provide.
//
// The other tests here check artefacts — the final listing, the final mode —
// and every one of them passes against a plain os.WriteFile. This one does not:
// it writes a payload large enough that a truncating write is observable, and
// asserts a concurrent reader only ever sees the whole old content or the whole
// new content.
func TestWriteFileAtomicNeverExposesAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")

	old := bytes.Repeat([]byte("o"), 64*1024)
	fresh := bytes.Repeat([]byte("n"), 8*1024*1024)
	if err := os.WriteFile(path, old, reportFilePerm); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad []byte
	var badOnce sync.Once

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path) // #nosec G304 -- path is built by the test
			if err != nil {
				// A reader must never find the destination missing either.
				badOnce.Do(func() { bad = []byte("destination vanished: " + err.Error()) })
				return
			}
			if !bytes.Equal(got, old) && !bytes.Equal(got, fresh) {
				badOnce.Do(func() { bad = got })
				return
			}
		}
	})

	if err := writeFileAtomic(path, fresh); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("write: %v", err)
	}
	close(stop)
	wg.Wait()

	if bad != nil {
		t.Fatalf("a reader observed neither the old nor the new content: %d bytes, prefix %.32q", len(bad), bad)
	}
	if got := readFile(t, path); !bytes.Equal(got, fresh) {
		t.Fatal("the destination does not hold the new content")
	}
}

func TestWriteFileAtomicLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")

	if err := WriteJSON(path, []*CertReport{{Name: "a"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertOnlyFile(t, dir, "results.json")
}

// TestWriteFileAtomicRejectsADirectoryDestination pins that the error names
// the real problem. Left to the rename, it surfaces on macOS as "file exists",
// which reads as a stale temporary file.
func TestWriteFileAtomicRejectsADirectoryDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := writeFileAtomic(path, []byte("x"))
	if err == nil {
		t.Fatal("expected an error writing onto a directory")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("the error should name the destination as a directory, got %v", err)
	}
	assertOnlyFile(t, dir, "results.json")
}

// TestWriteFileAtomicCleansUpAfterAFailedRename covers the window the deferred
// cleanup exists for.
//
// Every other way this function fails does so before the temporary file is
// created, so the cleanup was unreachable and deleting it outright left the
// suite green. Failing the rename is the only step that leaves a temporary
// file behind, and a leaked one accumulates beside every report a long-running
// CI job writes.
func TestWriteFileAtomicCleansUpAfterAFailedRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	if err := os.WriteFile(path, []byte("old"), reportFilePerm); err != nil {
		t.Fatalf("seed: %v", err)
	}

	original := renameFile
	renameFile = func(string, string) error { return errors.New("rename refused") }
	defer func() { renameFile = original }()

	err := writeFileAtomic(path, []byte("new"))
	if err == nil {
		t.Fatal("expected the write to fail when the rename does")
	}
	// The previous report must survive, and nothing may be left beside it.
	if got := string(readFile(t, path)); got != "old" {
		t.Fatalf("destination holds %q; a failed write must leave the previous report", got)
	}
	assertOnlyFile(t, dir, "results.json")
}

func TestWriteFileAtomicReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")

	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WriteJSON(path, []*CertReport{{Name: "fresh"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(readFile(t, path)); strings.Contains(got, "stale") {
		t.Fatal("the stale contents survived the write")
	}
	assertOnlyFile(t, dir, "results.json")
}

// TestWriteFileAtomicHonoursUmask pins that the change did not widen report
// permissions.
//
// The call this replaced was os.WriteFile(path, data, 0644), whose mode is
// masked by the process umask. Creating at 0600 and then chmod'ing to 0644
// would have replaced "0644 &^ umask" with an unconditional 0644, so under the
// umask 077 common on hardened CI runners a results file carrying node names
// and per-node hardware verdicts would have become world-readable.
// Both halves are asserted, and the second is why.
//
// A single case under umask 077 asserting 0600 is vacuous: 0644 &^ 077 and
// 0600 &^ 077 are both 0600, so changing createTemp's open mode to 0600 —
// which is the whole thing createTemp exists to avoid — passed it. The umask
// 022 case is the one that pins the mode as 0644; the 077 case pins that it is
// still masked.
func TestWriteFileAtomicHonoursUmask(t *testing.T) {
	for _, tc := range []struct {
		umask int
		want  os.FileMode
	}{
		{0o077, 0o600}, // masked
		{0o022, 0o644}, // the mode itself
	} {
		// Umask is process-global, so this test must not run in parallel with
		// anything that creates files.
		old := syscall.Umask(tc.umask)
		dir := t.TempDir()
		path := filepath.Join(dir, "results.json")
		err := writeFileAtomic(path, []byte("x"))
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(path)
		}
		syscall.Umask(old)

		if err != nil {
			t.Fatalf("umask %#o: %v", tc.umask, err)
		}
		if got := info.Mode().Perm(); got != tc.want {
			t.Fatalf("umask %#o gave mode %v, want %v (os.WriteFile(0644) is the behaviour being matched)",
				tc.umask, got, tc.want)
		}
	}
}

// TestWriteFileAtomicRequiresAWritableDirectory pins that the weaker
// guarantee is refused rather than delivered silently.
//
// An earlier version fell back to an in-place os.WriteFile here, so that a
// read-only directory with a writable destination kept working. That fallback
// returned nil while performing a truncating write, so the caller believed it
// had the no-partial-file guarantee when it did not — and it never even fired
// for a read-only filesystem, because that returns EROFS and
// os.IsPermission(EROFS) is false. The error now names the requirement.
func TestWriteFileAtomicRequiresAWritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	if err := os.WriteFile(path, []byte("old"), 0o666); err != nil { // #nosec G306 -- the test needs a world-writable destination
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := writeFileAtomic(path, []byte("new"))
	if err == nil {
		t.Fatal("expected an error: a non-writable directory cannot be written atomically")
	}
	if !strings.Contains(err.Error(), "writing atomically requires") {
		t.Fatalf("the error should name the requirement, got %v", err)
	}
	// And the previous report must be untouched, not half-replaced.
	if got := string(readFile(t, path)); got != "old" {
		t.Fatalf("destination holds %q; a refused write must leave the previous report", got)
	}
}

func TestWriteJUnitIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.xml")

	if err := WriteJUnit(path, []*CertReport{{Name: "a", Result: resultPassed}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := string(readFile(t, path)); !strings.HasPrefix(got, "<?xml") {
		t.Fatalf("expected an XML header, got %.40q", got)
	}
	assertOnlyFile(t, dir, "results.xml")
}

// TestWriteFileAtomicFollowsSymlinks pins that a symlinked results path still
// updates the file it names, and that the link survives.
//
// os.WriteFile wrote through a symlink. Rename does not — it replaces the link
// with a regular file and leaves the real target holding the old report, while
// the command reports success. Each sub-case below is a shape that a previous
// attempt at this got WRONG, writing to the wrong file silently:
//
//   - "dangling" fails EvalSymlinks, because that stats the whole path; the
//     first attempt ignored the error and clobbered the link.
//   - "relative through a symlinked dir" is the case where joining against
//     the link's lexical parent lands in a DIFFERENT real directory. The
//     decoy directory exists, so a lexical join writes there and the test
//     fails on content rather than on an error.
//   - "dangling chain" is where following one hop returns a path that is
//     itself a link, so the rename destroys the intermediate link.
func TestWriteFileAtomicFollowsSymlinks(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		link := filepath.Join(dir, "results.json")
		if err := os.WriteFile(target, []byte("old"), reportFilePerm); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := writeFileAtomic(link, []byte("new")); err != nil {
			t.Fatalf("write: %v", err)
		}
		assertTargetAndLink(t, target, link, "new")
	})

	t.Run("dangling", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		link := filepath.Join(dir, "results.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if err := writeFileAtomic(link, []byte("new")); err != nil {
			t.Fatalf("write: %v", err)
		}
		assertTargetAndLink(t, target, link, "new")
	})

	t.Run("relative through a symlinked dir", func(t *testing.T) {
		dir := t.TempDir()
		// real/inner holds the link; real/out is where "../out/x" means.
		// A decoy at <dir>/out is where a lexical join would wrongly land.
		for _, d := range []string{"real/inner", "real/out", "out"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
				t.Fatalf("mkdir %s: %v", d, err)
			}
		}
		if err := os.Symlink(filepath.Join(dir, "real", "inner"), filepath.Join(dir, "linkdir")); err != nil {
			t.Fatalf("symlink dir: %v", err)
		}
		link := filepath.Join(dir, "linkdir", "results.json")
		if err := os.Symlink("../out/results.json", link); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if err := writeFileAtomic(link, []byte("new")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := string(readFile(t, filepath.Join(dir, "real", "out", "results.json"))); got != "new" {
			t.Fatalf("real/out holds %q, want the new content", got)
		}
		if _, err := os.Lstat(filepath.Join(dir, "out", "results.json")); !os.IsNotExist(err) {
			t.Fatal("the write landed in the decoy directory; the relative target was joined lexically")
		}
	})

	t.Run("dangling chain", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "a.json")
		b := filepath.Join(dir, "b.json")
		c := filepath.Join(dir, "c.json")
		if err := os.Symlink(b, a); err != nil {
			t.Fatalf("symlink a: %v", err)
		}
		if err := os.Symlink(c, b); err != nil {
			t.Fatalf("symlink b: %v", err)
		}

		if err := writeFileAtomic(a, []byte("new")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := string(readFile(t, c)); got != "new" {
			t.Fatalf("the end of the chain holds %q, want the new content", got)
		}
		for _, link := range []string{a, b} {
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s is no longer a symlink; the chain was clobbered", link)
			}
		}
	})

	// A chain that never terminates must be refused, not guessed at. This is
	// what keeps the worst case safe rather than a silent write to the wrong
	// file.
	t.Run("cycle is refused", func(t *testing.T) {
		dir := t.TempDir()
		x := filepath.Join(dir, "x.json")
		y := filepath.Join(dir, "y.json")
		if err := os.Symlink(y, x); err != nil {
			t.Fatalf("symlink x: %v", err)
		}
		if err := os.Symlink(x, y); err != nil {
			t.Fatalf("symlink y: %v", err)
		}

		err := writeFileAtomic(x, []byte("new"))
		if err == nil {
			t.Fatal("a symlink cycle must be refused")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("the error should name the cycle, got %v", err)
		}
	})
}

func assertTargetAndLink(t *testing.T, target, link, want string) {
	t.Helper()
	if got := string(readFile(t, target)); got != want {
		t.Fatalf("the link's target holds %q, want %q", got, want)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- path is built by the test
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// assertOnlyFile fails when dir holds anything other than want. It is how the
// tests check that no ".tmp-*" sibling survived.
func assertOnlyFile(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("directory holds %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directory holds %v, want exactly %v", got, want)
		}
	}
}
