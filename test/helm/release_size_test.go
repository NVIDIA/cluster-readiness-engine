// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package helm

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// releaseSecretBudget is the largest encoded release this repository accepts:
// about 83% of the 1 MiB a Secret may hold. Every minor release adds CRD
// fields, so the budget turns shrinking headroom into a CI failure here
// rather than a failed install on a user's cluster (ADR-107).
const releaseSecretBudget = 850 * 1024

// releaseSecretLimit is the most a Secret may hold, Helm's default storage.
const releaseSecretLimit = 1 << 20

// chartFile mirrors a file in Helm's stored release. Its data is []byte, so
// encoding/json writes it as base64, as Helm's release JSON does.
type chartFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// TestReleaseSecretFitsBudget estimates each chart's release Secret the way
// Helm's Secrets driver builds it: the release as JSON, holding the rendered
// manifest and every chart file, gzipped at best compression, then base64.
// Counting every file in the chart directory overestimates slightly, which
// errs toward failing early.
func TestReleaseSecretFitsBudget(t *testing.T) {
	requireHelm(t)
	for _, dir := range []string{crdsChartDir(t), chartDir(t)} {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			requireChartInputs(t, dir)
			manifest, err := helmTemplate(dir, nil)
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			files, err := chartFiles(dir)
			if err != nil {
				t.Fatalf("read chart files: %v", err)
			}
			size, err := encodedReleaseSize(string(manifest), files)
			if err != nil {
				t.Fatalf("encode release: %v", err)
			}
			t.Logf("encoded release: %d bytes, %.0f%% of the %d-byte Secret limit",
				size, 100*float64(size)/releaseSecretLimit, releaseSecretLimit)
			if size > releaseSecretBudget {
				t.Errorf("encoded release is %d bytes, over the %d-byte budget; "+
					"the release Secret is nearing Helm's 1 MiB limit", size, releaseSecretBudget)
			}
		})
	}
}

// chartFiles reads every file under a chart directory, subcharts included.
func chartFiles(dir string) ([]chartFile, error) {
	var files []chartFile
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, chartFile{Name: rel, Data: data})
		return nil
	})
	return files, err
}

// encodedReleaseSize returns the length of base64(gzip(json(release))), the
// value Helm's Secrets driver stores.
func encodedReleaseSize(manifest string, files []chartFile) (int, error) {
	release, err := json.Marshal(struct {
		Manifest string      `json:"manifest"`
		Files    []chartFile `json:"files"`
	}{manifest, files})
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return 0, err
	}
	if _, err := w.Write(release); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	return base64.StdEncoding.EncodedLen(buf.Len()), nil
}
