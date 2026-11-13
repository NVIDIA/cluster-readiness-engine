// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both tests here are single-value assertions about whether a guard refuses,
// so they are written as tables rather than through the golden-file parser the
// rest of this package uses: there is no structured output to compare.

// TestWriteRefusesANonEmptyDirectory pins that Write refuses before writing.
//
// Verify rejects any file the manifest does not list, so one leftover makes a
// bundle unverifiable. Without this guard that surfaced at the worst moment:
// the writer wrote every file, sealed the manifest, and only then
// self-verified, so the command failed with an integrity error after leaving a
// sealed manifest certifying a directory no reader would accept, and blamed a
// tampering check for what was really a stale file.
//
// It was not a corner case either. `certification run` defaults the name to
// nvcrectl-<timestamp>, and that name is part of every component file name, so
// a second run into one directory always left the first run's components
// behind.
func TestWriteRefusesANonEmptyDirectory(t *testing.T) {
	cases := map[string]struct {
		setup      func(t *testing.T, dir string)
		wantRefuse bool
	}{
		"missing directory": {
			// Write creates what it needs, so nothing to refuse.
			setup:      func(t *testing.T, dir string) { t.Helper() },
			wantRefuse: false,
		},
		"empty directory": {
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantRefuse: false,
		},
		"a previous run's component file": {
			setup: func(t *testing.T, dir string) {
				t.Helper()
				comps := filepath.Join(dir, ComponentsDir)
				if err := os.MkdirAll(comps, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				f := filepath.Join(comps, "nvcrectl-older-compute-cutlass-gemm.ndjson")
				if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantRefuse: true,
		},
		"an unrelated file, as a CI artifacts directory would hold": {
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "build.log"), []byte("x"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
			wantRefuse: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "bundle")
			tc.setup(t, dir)

			err := requireEmptyBundleDir(dir)
			if tc.wantRefuse {
				if err == nil {
					t.Fatal("the guard accepted a directory that already holds files, so the " +
						"bundle will be written and sealed and then fail its own self-verify")
				}
				// The offending entry has to be named: the operator cannot
				// decide what to do about a file the error does not identify.
				if !strings.Contains(err.Error(), "not empty") {
					t.Errorf("refusal does not say the directory is not empty: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the guard refused a usable directory: %v", err)
			}
		})
	}
}

// TestValidateExportPathsRefusesPathsInsideTheBundle pins that the other
// export flags cannot be aimed into the bundle directory.
//
// The two ways of getting this wrong failed differently and both badly. A path
// written before the bundle became an unlisted file, so the bundle's own
// self-verify failed after everything had been written. A path written after
// left the bundle verifying clean, the command printing success, and the
// artifact already invalid with nothing reporting it.
// junitFlag is the flag these cases pass; named because goconst counts
// across the package.
const junitFlag = "--junit-file"

func TestValidateExportPathsRefusesPathsInsideTheBundle(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "out")

	cases := map[string]struct {
		path       string
		wantRefuse bool
	}{
		"directly inside":             {filepath.Join(bundleDir, "junit.xml"), true},
		"nested inside":               {filepath.Join(bundleDir, ComponentsDir, "junit.xml"), true},
		"the bundle directory itself": {bundleDir, true},
		"a sibling":                   {filepath.Join(root, "junit.xml"), false},
		// The prefix test must compare path segments, not strings: "out-junit"
		// starts with "out" and is not inside it.
		"a sibling whose name starts with the bundle's": {filepath.Join(root, "out-junit.xml"), false},
		"elsewhere entirely":                            {filepath.Join(t.TempDir(), "junit.xml"), false},
		"unset":                                         {"", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateExportPaths(bundleDir, ExportPath{Flag: junitFlag, Path: tc.path})
			if tc.wantRefuse {
				if err == nil {
					t.Fatalf("accepted %q inside the bundle directory, which makes the "+
						"bundle unverifiable", tc.path)
				}
				if !strings.Contains(err.Error(), junitFlag) {
					t.Errorf("refusal does not name the flag at fault: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused %q, which is outside the bundle: %v", tc.path, err)
			}
		})
	}

	// An empty bundle directory means the flag was not given, so nothing to
	// check against.
	t.Run("no bundle requested", func(t *testing.T) {
		if err := ValidateExportPaths("",
			ExportPath{Flag: junitFlag, Path: filepath.Join(root, "junit.xml")}); err != nil {
			t.Fatalf("refused a path with no bundle in play: %v", err)
		}
	})
}

// TestValidateExportPathsResolvesSymlinks pins that an alias to the bundle
// directory does not get a report past the containment check.
//
// A lexical comparison reads /tmp/alias/junit.xml as outside /tmp/out even
// when alias links to out, because filepath.Rel returns "../alias/junit.xml".
// The writer then resolves the parent and creates the file inside the bundle,
// so the bundle holds a file its manifest does not list and the command still
// reports success. That is the exact invalidation this check exists to stop,
// reached by the one spelling the check did not see.
func TestValidateExportPathsResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "out")
	if err := os.MkdirAll(bundleDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(bundleDir, alias); err != nil {
		t.Skipf("this platform cannot create a symlink: %v", err)
	}

	t.Run("through a symlinked bundle directory", func(t *testing.T) {
		err := ValidateExportPaths(bundleDir,
			ExportPath{Flag: junitFlag, Path: filepath.Join(alias, "junit.xml")})
		if err == nil {
			t.Fatal("accepted a path that reaches the bundle through a symlink, so the " +
				"report would land inside the bundle and invalidate it")
		}
	})

	// And the reverse spelling: the bundle named through the alias, the report
	// named through the real path.
	t.Run("bundle named through the symlink", func(t *testing.T) {
		err := ValidateExportPaths(alias,
			ExportPath{Flag: junitFlag, Path: filepath.Join(bundleDir, "junit.xml")})
		if err == nil {
			t.Fatal("accepted a path inside the bundle because the bundle was named " +
				"through a symlink")
		}
	})

	// A genuinely outside path must still be accepted once symlinks resolve,
	// or the fix would simply refuse everything.
	t.Run("outside stays outside", func(t *testing.T) {
		if err := ValidateExportPaths(bundleDir,
			ExportPath{Flag: junitFlag, Path: filepath.Join(root, "junit.xml")}); err != nil {
			t.Fatalf("refused a sibling of the bundle: %v", err)
		}
	})

	// Neither path existing yet is the ordinary case on a first run.
	t.Run("neither path exists yet", func(t *testing.T) {
		fresh := filepath.Join(root, "fresh")
		if err := ValidateExportPaths(fresh,
			ExportPath{Flag: junitFlag, Path: filepath.Join(fresh, "junit.xml")}); err == nil {
			t.Fatal("accepted a path inside a bundle directory that does not exist yet")
		}
		if err := ValidateExportPaths(fresh,
			ExportPath{Flag: junitFlag, Path: filepath.Join(root, "junit.xml")}); err != nil {
			t.Fatalf("refused an outside path when neither exists: %v", err)
		}
	})
}
