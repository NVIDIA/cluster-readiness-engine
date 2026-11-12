// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// reportFilePerm is the mode a report file is left with. Reports are output
// files meant to be readable by whatever consumes them.
const reportFilePerm os.FileMode = 0o644

// writeFileAtomic writes data to path so that a reader never observes a
// partial file.
//
// os.WriteFile truncates the destination and then writes, so a crash, a full
// disk or a cancelled run leaves a file that exists, parses as far as it got,
// and is indistinguishable from a complete one. For a results file that is the
// primary evidence of a certification, and for a bundle whose manifest
// certifies its contents by digest, that failure mode is not acceptable
// (ADR-090).
//
// The temporary file is created in the destination's own directory because
// rename is only atomic within a filesystem; /tmp is frequently a different
// one. It carries a random suffix rather than the literal ".tmp" ADR-090
// names, so two writers racing on one path cannot corrupt each other's
// temporary file.
//
// The directory entry itself is not fsynced, so a crash of the machine, as
// opposed to a crash of this process, can still lose the rename. That is a
// deliberate limit on the claim: what is guaranteed is that no reader ever
// sees a partially written report, not that a written report survives a power
// cut. Closing the second gap means opening and syncing the parent directory
// on every write, and nothing in the certification flow asks for durability
// across a machine failure.
// WriteFileAtomic writes data to path so that a reader never observes a
// partial file. See writeFileAtomic for the guarantee and its limits.
//
// Exported for the bundle writer, which needs exactly this for every file it
// lays down beside the manifest that certifies them by digest: a file that was
// hashed while complete and is then truncated by a crash would make the
// manifest disagree with the disk, and the reader would refuse a bundle that
// was only ever half written.
func WriteFileAtomic(path string, data []byte) error {
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) (err error) {
	// Follow a symlink to its target before choosing anything else.
	//
	// os.WriteFile wrote through a symlink; rename does not, it replaces the
	// link with a regular file and leaves the real target holding the old
	// report, while the command still prints that results were written. A
	// symlinked results path is ordinary in CI, so silently writing somewhere
	// else and reporting success is the worst available behaviour.
	resolved, err := resolveDestination(path)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	path = resolved

	if info, statErr := os.Lstat(path); statErr == nil {
		if info.IsDir() {
			// Caught here so the error names the problem. Left to the rename, it
			// surfaces on macOS as "file exists", which reads as a stale temporary
			// file rather than as the destination being a directory.
			return fmt.Errorf("write %s: destination is a directory", path)
		}
		if !info.Mode().IsRegular() {
			// A character device, a FIFO or a socket. Written in place, which
			// is what os.WriteFile did before this function existed.
			//
			// Atomicity is not merely unnecessary here, it is wrong in two
			// ways. There is no partial read to protect: a reader of a stream
			// consumes what is written as it arrives and never sees a
			// half-written file. And a rename would REPLACE the destination
			// with a regular file, so `--results-file /dev/stdout` would stop
			// writing to the terminal and, running as root, `--junit-file
			// /dev/null` would replace the device node for everything else on
			// the machine. Both of those used to work.
			return os.WriteFile(path, data, reportFilePerm)
		}
	}

	dir := filepath.Dir(path)
	info, statErr := os.Stat(dir)
	if statErr != nil {
		return fmt.Errorf("write %s: directory %s is not usable: %w", path, dir, statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("write %s: %s is not a directory", path, dir)
	}

	tmp, err := createTemp(dir, filepath.Base(path))
	if err != nil {
		// Deliberately no fallback to an in-place write.
		//
		// An earlier version fell back when this returned a permission error,
		// to keep working for a run whose output directory is not writable but
		// whose destination file is. Three things killed it. The premise is
		// speculative and no such setup is known. It did not even cover the
		// case it was written for: a read-only filesystem returns EROFS, and
		// os.IsPermission(EROFS) is false, so the fallback never fired there.
		// And it silently delivered a truncating, non-atomic write while
		// returning nil, so the caller — and the guarantee in this function's
		// own doc comment — believed a reader could never see a partial file
		// when on that path it could.
		//
		// A weaker guarantee delivered silently is worse than an error that
		// names the requirement, so the error names it.
		return fmt.Errorf(
			"write %s: could not create a temporary file in %s, which writing atomically requires: %w",
			path, dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			// Best effort: the write already failed, and a leftover temporary
			// file is a smaller problem than masking the original error.
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file for %s: %w", path, err)
	}
	// Sync before rename: without it the rename can land while the data is
	// still only in the page cache, which is the partial-file problem moved
	// rather than solved.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", path, err)
	}
	if err = renameFile(tmpName, path); err != nil {
		return fmt.Errorf("rename temporary file into %s: %w", path, err)
	}
	return nil
}

// renameFile is os.Rename, indirected so a test can fail the last step.
//
// Every other way to fail this function does so before the temporary file
// exists — the symlink resolve, the directory checks, the create — which left
// the whole window the deferred cleanup exists for unreachable: deleting the
// cleanup outright passed the suite. A seam here is the smallest way to make
// that window observable.
var renameFile = os.Rename

// maxSymlinkHops bounds how far resolveDestination will follow a chain of
// symbolic links, mirroring what the kernel does before it returns ELOOP.
const maxSymlinkHops = 32

// resolveDestination returns the real path a destination names, following
// symbolic links.
//
// os.WriteFile wrote through a symlink to its target. Rename cannot: it
// unlinks the destination name and puts the temporary file's inode there, so
// the link is replaced by a regular file and the real target keeps the old
// report while the command reports success. A symlinked results path is
// ordinary in CI, so the link has to be followed rather than clobbered.
//
// Two earlier attempts got this wrong, both in ways that wrote to the wrong
// file silently, and the fixes for both are visible here:
//
//   - A relative target such as "../out/x.json" must be joined against the
//     directory the link RESOLVES THROUGH, not against its lexical parent.
//     filepath.Join cleans ".." lexically, so joining against the lexical
//     parent lands in a different real directory whenever any component of
//     the path is itself a link. That is what the EvalSymlinks on the
//     directory below is for, and why it is done per hop.
//   - A chain must be followed to its end. Resolving one hop returned a path
//     that was itself a symlink, and the rename then destroyed the
//     intermediate link while never creating the file the chain names.
//
// EvalSymlinks alone cannot do this, because it stats the whole path and so
// fails on a DANGLING link — precisely the case of a link whose target the
// first write is meant to create. Only the link's directory has to exist.
//
// The bound is what makes this safe rather than merely correct: if the chain
// does not terminate at a non-symlink within maxSymlinkHops, whether through a
// cycle or sheer depth, this REFUSES rather than returning a guess. The worst
// case is then an error naming the problem, not a silent write to the wrong
// file.
//
// There is an unavoidable race between resolving and renaming: the link could
// be repointed in between. That is inherent to any resolve-then-rename and is
// not worth guarding on a path the operator chose for its own output.
func resolveDestination(path string) (string, error) {
	// The fast path, and the only one that runs for a destination that already
	// exists or is not a link at all.
	if full, err := filepath.EvalSymlinks(path); err == nil {
		return full, nil
	}

	for range maxSymlinkHops {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// Not a link, or does not exist: the common case of writing a
			// brand new file. Resolve the directory so the temporary file and
			// the rename land on the same real filesystem, and keep the name.
			if dir, dirErr := filepath.EvalSymlinks(filepath.Dir(path)); dirErr == nil {
				return filepath.Join(dir, filepath.Base(path)), nil
			}
			return path, nil
		}

		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read symbolic link %s: %w", path, err)
		}
		if !filepath.IsAbs(target) {
			// Against the link's RESOLVED directory, which is the whole point.
			dir, dirErr := filepath.EvalSymlinks(filepath.Dir(path))
			if dirErr != nil {
				return "", fmt.Errorf("resolve the directory of symbolic link %s: %w", path, dirErr)
			}
			target = filepath.Join(dir, target)
		}
		path = target
	}

	return "", fmt.Errorf(
		"destination is a chain of more than %d symbolic links, or a cycle; name the file it points at instead",
		maxSymlinkHops)
}

// createTemp opens a uniquely named file in dir with reportFilePerm.
//
// os.CreateTemp is not used because it opens at 0600 and the mode would then
// have to be fixed with chmod, which is not subject to the process umask. The
// call this replaced was os.WriteFile(path, data, 0644), whose mode IS masked,
// so a chmod would have turned 0644 &^ umask into an unconditional 0644: under
// `umask 077`, common on hardened CI runners, a results file carrying node
// names and per-node hardware verdicts would have gone from -rw------- to
// -rw-r--r--. Opening with the mode keeps the masking, and the file's
// permissions stay exactly what they were before this change.
func createTemp(dir, base string) (*os.File, error) {
	var lastErr error
	for range 10 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, fmt.Errorf("generate temporary file name: %w", err)
		}
		name := filepath.Join(dir, base+".tmp-"+hex.EncodeToString(suffix[:]))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, reportFilePerm) // #nosec G304 -- name is derived from the caller's destination
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("could not create a temporary file in %s: %w", dir, lastErr)
}

// errNoReports reports a write call with nothing to write. WriteJSON indexed
// reports[0] unguarded, so an empty slice panicked rather than erroring.
var errNoReports = errors.New("no reports to write")
