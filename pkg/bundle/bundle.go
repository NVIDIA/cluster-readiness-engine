// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle writes and verifies the result bundle ADR-090 defines: a
// directory holding a certification's report, its per-component result rows as
// plain NDJSON, an account of anything that was lost, and a manifest that
// certifies the rest by digest.
//
// The bundle exists so a consumer never has to walk NVCRE's object graph. The
// rows live in ConfigMap binaryData as gzipped NDJSON, sharded to stay under
// the API server's object limit, and none of that is a shape anyone outside
// the cluster should have to know. nvcrectl does the shard walk and the gunzip
// (ADR-090 decision 4), so what lands on disk is a directory of files.
//
// The package is deliberately separate from pkg/report. pkg/report builds and
// renders the certification report; this assembles a verifiable artifact
// around it, and needs a Kubernetes client to do so, which pkg/report does not
// have and should not acquire.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

// FormatVersion is the bundle layout's version. A reader that does not
// recognise it must refuse the bundle rather than guess at the layout.
const FormatVersion = 1

// File names and directories in a bundle. The layout is ADR-090 decision 4.
const (
	ManifestName   = "manifest.json"
	ResultsName    = "results.json"
	TruncationName = "truncation.json"
	ComponentsDir  = "components"
	SchemaDir      = "schema"

	// componentExt is the extension on a component rows file. Plain NDJSON,
	// not gzip: the point of the bundle is that the consumer does not deal
	// with the storage format.
	componentExt = ".ndjson"
)

// bundleFilePerm matches the mode report files are written with: these are
// output files meant to be read by whatever consumes them.
const bundleDirPerm os.FileMode = 0o755

// Manifest is the bundle's index, written last.
//
// Written last on purpose, and it is the whole integrity story. A bundle
// without a manifest is an incomplete write, and the reader refuses it rather
// than falling back to a directory glob (ADR-090: "a missing manifest means
// refuse the bundle, with no directory-glob fallback"). So the manifest's
// presence is the claim that the write finished, and its digests are the claim
// that nothing has changed since.
type Manifest struct {
	FormatVersion int `json:"formatVersion"`

	// FileCount is the number of entries in Files. Stored rather than derived
	// so a truncated manifest that still parses is caught: a reader compares
	// it against both len(Files) and what is on disk.
	FileCount int `json:"fileCount"`

	// Files lists every file in the bundle except the manifest itself, sorted
	// by path so the manifest is byte-identical for identical content.
	Files []FileEntry `json:"files"`

	// Components records what the shard walk resolved, per category.
	//
	// The manifest is only written when the resolved counts match the sealed
	// ones, so its existence already asserts completeness. These numbers make
	// that assertion auditable instead of implicit.
	Components []ComponentSet `json:"components,omitempty"`
}

// FileEntry is one file's identity in the manifest.
type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// ComponentSet is one category's resolved result set.
type ComponentSet struct {
	// Scope names the run the rows came from, a Certification or a
	// WorkloadRun.
	Scope   string `json:"scope,omitempty"`
	Domain  string `json:"domain"`
	Variant string `json:"variant"`
	// Path is the component rows file, relative to the bundle root.
	Path string `json:"path"`
	// ShardCount and RowCount are what the walk resolved, which equals what
	// the Workflow sealed or the bundle is not written at all.
	ShardCount int32 `json:"shardCount"`
	RowCount   int64 `json:"rowCount"`
	// Final reports whether the run had finished producing when this was read.
	// A bundle built from a non-final total is a snapshot of a running job,
	// which is legitimate and must not be mistaken for a complete one.
	Final bool `json:"final"`
	// TruncatedRowCount is how many rows were lost before they reached a
	// shard. Non-zero means truncation.json has the detail.
	TruncatedRowCount int64 `json:"truncatedRowCount,omitempty"`
}

// Contents is everything that goes into a bundle.
type Contents struct {
	// Reports is the certification report, written as results.json in the same
	// shape --results-file produces, so a consumer reading one can read the
	// other.
	Reports []*report.CertReport

	// Components holds each category's resolved rows, in the order the
	// component files should be listed.
	Components []ResolvedComponent

	// Truncations is everything that was lost, from any category. Written even
	// when empty, so a consumer can tell "nothing was lost" from "this bundle
	// predates truncation reporting".
	Truncations []TruncationRecord
}

// TruncationRecord is one loss, as truncation.json carries it.
//
// ADR-090 requires that rows lost to log pagination are "recorded as a partial
// read in truncation.json and are never collapsed to zero rows". The same file
// carries every other loss the write path recorded, because a consumer asking
// "is this evidence complete" wants one answer, not one per mechanism.
type TruncationRecord struct {
	Scope       string `json:"scope,omitempty"`
	Domain      string `json:"domain"`
	Variant     string `json:"variant"`
	Reason      string `json:"reason"`
	Count       int64  `json:"count"`
	Node        string `json:"node,omitempty"`
	ComponentID string `json:"componentID,omitempty"`
	Index       int32  `json:"index,omitempty"`
	Message     string `json:"message,omitempty"`
}

// Write lays out a bundle in dir, writing the manifest last.
//
// Every file is written atomically, and the manifest only after all of them
// are on disk and hashed. An interrupted write therefore leaves a directory
// with no manifest, which the reader refuses, rather than a manifest that
// certifies files that are not all there.
func Write(dir string, c Contents) error {
	if err := os.MkdirAll(filepath.Join(dir, ComponentsDir), bundleDirPerm); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, SchemaDir), bundleDirPerm); err != nil {
		return fmt.Errorf("create bundle schema directory: %w", err)
	}

	m := &Manifest{FormatVersion: FormatVersion}

	resultsJSON, err := marshalIndent(resultsDocument(c.Reports))
	if err != nil {
		return fmt.Errorf("marshal %s: %w", ResultsName, err)
	}
	if err := writeBundleFile(dir, ResultsName, resultsJSON, m); err != nil {
		return err
	}

	for _, comp := range c.Components {
		rel := filepath.ToSlash(filepath.Join(ComponentsDir, comp.FileName()))
		if err := writeBundleFile(dir, rel, comp.NDJSON, m); err != nil {
			return err
		}
		m.Components = append(m.Components, ComponentSet{
			Scope:             comp.Scope,
			Domain:            comp.Domain,
			Variant:           comp.Variant,
			Path:              rel,
			ShardCount:        comp.ShardCount,
			RowCount:          comp.RowCount,
			Final:             comp.Final,
			TruncatedRowCount: comp.TruncatedRowCount,
		})
	}

	truncJSON, err := marshalIndent(truncationDocument(c.Truncations))
	if err != nil {
		return fmt.Errorf("marshal %s: %w", TruncationName, err)
	}
	if err := writeBundleFile(dir, TruncationName, truncJSON, m); err != nil {
		return err
	}

	schemaRel := filepath.ToSlash(filepath.Join(SchemaDir, rowSchemaName))
	if err := writeBundleFile(dir, schemaRel, []byte(rowSchema), m); err != nil {
		return err
	}

	// Sorted so the manifest is byte-identical for identical content, which is
	// what makes a bundle comparable between runs.
	slices.SortFunc(m.Files, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	m.FileCount = len(m.Files)

	manifestJSON, err := marshalIndent(m)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", ManifestName, err)
	}
	// Last. Everything above is already on disk and hashed.
	if err := report.WriteFileAtomic(filepath.Join(dir, ManifestName), manifestJSON); err != nil {
		return fmt.Errorf("write %s: %w", ManifestName, err)
	}
	return nil
}

// writeBundleFile writes one file and records it in the manifest under
// construction. The digest is taken from the bytes that were written, not by
// reading the file back: hashing a re-read would certify whatever is on disk
// now rather than what this call put there.
func writeBundleFile(dir, rel string, data []byte, m *Manifest) error {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := report.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	sum := sha256.Sum256(data)
	m.Files = append(m.Files, FileEntry{
		Path:   rel,
		SHA256: hex.EncodeToString(sum[:]),
		Bytes:  int64(len(data)),
	})
	return nil
}

// resultsDocument keeps --results-file's shape: one report is an object and
// several are an array (ADR-071). A consumer that already parses a results
// file parses this unchanged.
func resultsDocument(reports []*report.CertReport) any {
	if len(reports) == 1 {
		return reports[0]
	}
	return reports
}

// truncationDocument wraps the records so the file has somewhere to grow and
// so an empty bundle writes `{"truncations": []}` rather than `null`. A null
// there reads as "unknown", which is the one thing this file must never say.
func truncationDocument(records []TruncationRecord) any {
	if records == nil {
		records = []TruncationRecord{}
	}
	return struct {
		FormatVersion int                `json:"formatVersion"`
		Truncations   []TruncationRecord `json:"truncations"`
	}{FormatVersion: FormatVersion, Truncations: records}
}

func marshalIndent(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
