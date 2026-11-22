// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNoManifest is returned when a directory holds no manifest.
//
// Its own error so a caller can tell "this is not a bundle" from "this bundle
// is corrupt", which are different things to report to a user, without either
// becoming a reason to read the directory anyway.
var ErrNoManifest = errors.New("no " + ManifestName + " in bundle")

// Verify checks a bundle against its manifest and returns the manifest.
//
// Three rules, all normative in ADR-090, and each catches something the others
// cannot:
//
//  1. A missing manifest means refuse the bundle, with no directory-glob
//     fallback. The manifest is written last, so its absence means the write
//     did not finish; globbing the directory instead would read a partial
//     bundle and report success.
//
//  2. A manifest that fails to parse, or whose file count does not match what
//     is on disk, is a hard error rather than a degraded read. Both
//     directions are checked: a file the manifest does not list is as much a
//     disagreement as one it lists that is absent.
//
//  3. Every file the manifest lists is hashed, and a digest mismatch refuses
//     the bundle. This is what the sha256 is for. A file count and a row count
//     both survive a file being edited in place, so a bundle that passed only
//     those checks would be certified as whole on evidence that cannot detect
//     the one thing the digest exists to catch.
func Verify(dir string) (*Manifest, error) {
	// #nosec G304 -- dir is the bundle directory the user named on the command line.
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", dir, ErrNoManifest)
		}
		return nil, fmt.Errorf("read %s: %w", ManifestName, err)
	}

	var m Manifest
	// DisallowUnknownFields is not used: a newer writer may add fields, and
	// the formatVersion check below is the compatibility gate. Refusing an
	// unknown field would make every additive change a breaking one.
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%s declares format version %d, this build reads %d",
			ManifestName, m.FormatVersion, FormatVersion)
	}
	if m.FileCount != len(m.Files) {
		return nil, fmt.Errorf("%s declares %d files and lists %d",
			ManifestName, m.FileCount, len(m.Files))
	}

	onDisk, err := walkBundleFiles(dir)
	if err != nil {
		return nil, err
	}
	listed := make(map[string]FileEntry, len(m.Files))
	for _, f := range m.Files {
		if f.Path == ManifestName {
			return nil, fmt.Errorf("%s lists itself", ManifestName)
		}
		if err := checkBundlePath(f.Path); err != nil {
			return nil, err
		}
		if _, dup := listed[f.Path]; dup {
			return nil, fmt.Errorf("%s lists %s twice", ManifestName, f.Path)
		}
		listed[f.Path] = f
	}

	// Rule 2, both directions.
	var missing, unlisted []string
	for path := range listed {
		if !slices.Contains(onDisk, path) {
			missing = append(missing, path)
		}
	}
	for _, path := range onDisk {
		if _, ok := listed[path]; !ok {
			unlisted = append(unlisted, path)
		}
	}
	slices.Sort(missing)
	slices.Sort(unlisted)
	if len(missing) > 0 || len(unlisted) > 0 {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "listed but absent: "+strings.Join(missing, ", "))
		}
		if len(unlisted) > 0 {
			parts = append(parts, "present but unlisted: "+strings.Join(unlisted, ", "))
		}
		return nil, fmt.Errorf("%s disagrees with the bundle contents (%s)",
			ManifestName, strings.Join(parts, "; "))
	}

	// Rule 3.
	for _, f := range m.Files {
		// #nosec G304 -- checkBundlePath above confirmed f.Path stays inside dir.
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Path, err)
		}
		if int64(len(data)) != f.Bytes {
			return nil, fmt.Errorf("%s is %d bytes, %s records %d",
				f.Path, len(data), ManifestName, f.Bytes)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
			return nil, fmt.Errorf("%s digest mismatch: %s records %s, file hashes to %s",
				f.Path, ManifestName, f.SHA256, got)
		}
	}
	return &m, nil
}

// checkBundlePath rejects a manifest entry that does not name a file inside
// the bundle.
//
// A bundle is an artifact that travels: it is handed over, archived, and
// opened by whoever receives it, so its manifest is attacker-controlled input
// to this reader. Without this, an entry of "../../etc/passwd" would send the
// rule-3 read outside the directory the user named, and the digest comparison
// would answer whether that file matches a hash the bundle's author chose.
//
// Today the paths-versus-disk comparison would reject such an entry first,
// because a traversing path cannot appear in a walk of the bundle. That is an
// accident of rule ordering, not a guarantee: reorder the rules, or add a
// read before them, and the hole opens. Checking the path where it is first
// trusted keeps the property local to the thing that has it.
func checkBundlePath(p string) error {
	if p == "" {
		return fmt.Errorf("%s lists an entry with no path", ManifestName)
	}
	// Slash-separated and relative by construction, so reject anything that is
	// not: an absolute path, a Windows volume, or any traversal segment.
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return fmt.Errorf("%s lists %s, which is not inside the bundle", ManifestName, p)
	}
	if p != filepath.ToSlash(filepath.Clean(p)) {
		return fmt.Errorf("%s lists %s, which is not a normalised path", ManifestName, p)
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("%s lists %s, which escapes the bundle", ManifestName, p)
	}
	return nil
}

// walkBundleFiles lists every regular file in the bundle except the manifest,
// as slash-separated paths relative to dir.
//
// Directories with no files are not reported, so an empty components or schema
// directory does not read as a disagreement. A symlink is not a regular file
// and is reported as unlisted, which refuses the bundle: a bundle is a set of
// files with digests, and a link is a way to make the digest describe
// something other than what a reader will follow.
func walkBundleFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk bundle %s: %w", dir, err)
	}
	slices.Sort(out)
	return out, nil
}
