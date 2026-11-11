// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
)

// listPageSize bounds one List response. The shard count tracks the Job count,
// which at a thousand nodes running a numNodes:1 entry is a thousand objects
// each carrying up to 768 KiB of binaryData (ADR-090). Fetching that in one
// response would hold the whole result set in memory at once on the client,
// so the walk pages and discards each page's objects as it goes.
const listPageSize = 32

// ResolvedComponent is one category's rows, gunzipped and concatenated.
type ResolvedComponent struct {
	// Scope is the name of the run the rows came from: a Certification or a
	// WorkloadRun. Part of the file name because `nvcrectl certification
	// report` accepts several names at once, and two certifications that ran
	// the same category would otherwise write the same component file and one
	// would silently replace the other.
	Scope             string
	Domain            string
	Variant           string
	NDJSON            []byte
	ShardCount        int32
	RowCount          int64
	Final             bool
	TruncatedRowCount int64
	// Truncations is what the category's Jobs recorded as lost.
	Truncations []TruncationRecord
}

// FileName is the component file's name within the components directory.
//
// Built from whichever of scope, domain and variant are set, so a
// certification category reads as "<cert>-<domain>-<variant>.ndjson" and a
// WorkloadRun, which has no catalog category at all, reads as
// "<run>.ndjson" rather than carrying two empty segments.
func (c ResolvedComponent) FileName() string {
	parts := make([]string, 0, 3)
	for _, part := range []string{c.Scope, c.Domain, c.Variant} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "-") + componentExt
}

// ShardMismatchError reports that the shards found do not match what was
// sealed. It is deliberately a hard failure: the bundle is not written.
//
// This is where ADR-090 is explicit that FailedNodesFromRef's pattern must not
// be copied. That helper returns nil on a Get error, on a decode error and on
// a nil reference alike, so an unreadable ConfigMap is indistinguishable from
// nothing-to-report. For a node list that is a tolerable degradation. For the
// primary evidence of a certification it is not: a bundle short of its rows
// would carry a manifest certifying it as whole.
type ShardMismatchError struct {
	Domain, Variant         string
	SealedShards, GotShards int32
	SealedRows, GotRows     int64
}

func (e *ShardMismatchError) Error() string {
	return fmt.Sprintf(
		"%s/%s: resolved %d shards holding %d rows, but the run sealed %d shards holding %d rows;"+
			" refusing to write a bundle that would certify an incomplete result set",
		e.Domain, e.Variant, e.GotShards, e.GotRows, e.SealedShards, e.SealedRows)
}

// ResolveCertification walks every category's shards and returns them ready to
// write.
//
// Each category is one Workflow, so each carries its own selector and its own
// sealed counts, mirrored onto the Certification status. A category that
// produced no component results is skipped rather than written as an empty
// file: an absent category and one that produced nothing are the same thing to
// a consumer, and a file of zero rows invites the reading that rows were
// expected and lost.
//
// The namespace is resolved per category, not once for the Certification.
// WorkflowStatus.Namespace is where that Workflow's Jobs and their shards
// actually live, and the controller auto-generates it when the spec leaves it
// unset, so two categories of one Certification can sit in two namespaces.
// Listing one namespace for all of them would find no shards for the rest and
// fail the run as a sealed-count mismatch, which is the correct outcome for a
// genuinely missing shard and a misleading one for a bundle looking in the
// wrong place.
func ResolveCertification(
	ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification,
) ([]ResolvedComponent, error) {
	var out []ResolvedComponent
	for i := range cert.Status.CategoryStatuses {
		cs := &cert.Status.CategoryStatuses[i]
		if cs.ComponentResults == nil {
			continue
		}
		namespace, err := categoryNamespace(ctx, c, cert, cs)
		if err != nil {
			return nil, err
		}
		comp, err := resolveOne(ctx, c, namespace, cs.Domain, cs.Variant, cs.ComponentResults)
		if err != nil {
			return nil, err
		}
		comp.Scope = cert.Name
		for i := range comp.Truncations {
			comp.Truncations[i].Scope = cert.Name
		}
		out = append(out, comp)
	}
	return out, nil
}

// categoryNamespace returns the namespace a category's shards live in.
//
// Read from the category's Workflow rather than inferred, because only the
// Workflow knows its resolved namespace. A Workflow that cannot be read is an
// error rather than a fallback to the Certification's own namespace: guessing
// would list the wrong namespace, find nothing, and report the result set as
// incomplete, which points a user at their cluster instead of at the Workflow
// this failed to read.
func categoryNamespace(
	ctx context.Context, c client.Client,
	cert *nvcrev1alpha1.Certification, cs *nvcrev1alpha1.CertificationCategoryStatus,
) (string, error) {
	if cs.WorkflowRef == nil {
		return "", fmt.Errorf("%s/%s sealed component results but has no workflow reference",
			cs.Domain, cs.Variant)
	}
	ns := cs.WorkflowRef.Namespace
	if ns == "" {
		ns = cert.Namespace
	}
	wf := &nvcrev1alpha1.Workflow{}
	if err := c.Get(ctx, client.ObjectKey{Name: cs.WorkflowRef.Name, Namespace: ns}, wf); err != nil {
		return "", fmt.Errorf("%s/%s: read workflow %s/%s to locate its result shards: %w",
			cs.Domain, cs.Variant, ns, cs.WorkflowRef.Name, err)
	}
	if wf.Status.Namespace != "" {
		return wf.Status.Namespace, nil
	}
	return wf.Namespace, nil
}

// ResolveWorkflow walks one Workflow's shards, for the workloadrun path where
// there is no Certification above it.
//
// Reports false when the Workflow sealed no component results, which every
// WorkloadRun in the repository does today. The caller writes no component
// file in that case rather than an empty one: a file of zero rows invites the
// reading that rows were expected and lost.
func ResolveWorkflow(
	ctx context.Context, c client.Client, scope string, wf *nvcrev1alpha1.Workflow,
) (ResolvedComponent, bool, error) {
	totals := wf.Status.ComponentResults
	if totals == nil {
		return ResolvedComponent{}, false, nil
	}
	namespace := wf.Status.Namespace
	if namespace == "" {
		namespace = wf.Namespace
	}
	comp, err := resolveOne(ctx, c, namespace, "", "", totals)
	if err != nil {
		return ResolvedComponent{}, false, err
	}
	comp.Scope = scope
	for i := range comp.Truncations {
		comp.Truncations[i].Scope = scope
	}
	return comp, true, nil
}

func resolveOne(
	ctx context.Context, c client.Client, namespace, domain, variant string,
	totals *nvcrev1alpha1.WorkflowComponentResults,
) (ResolvedComponent, error) {
	comp := ResolvedComponent{
		Domain: domain, Variant: variant,
		Final:             totals.Final,
		TruncatedRowCount: totals.TruncatedRowCount,
	}

	selector, err := labels.Parse(totals.Selector)
	if err != nil {
		return comp, fmt.Errorf("%s/%s: parse shard selector %q: %w", domain, variant, totals.Selector, err)
	}

	rows, truncations, err := walkShards(ctx, c, namespace, selector)
	if err != nil {
		return comp, fmt.Errorf("%s/%s: %w", domain, variant, err)
	}

	comp.ShardCount = rows.shards
	comp.RowCount = rows.rows
	comp.NDJSON = rows.ndjson.Bytes()
	for _, t := range truncations {
		t.Domain, t.Variant = domain, variant
		comp.Truncations = append(comp.Truncations, t)
	}

	// The completeness check. Both counts, because either alone can agree
	// while the other does not: a shard that failed to write leaves the shard
	// count short, and a shard rewritten with fewer rows leaves the row count
	// short at the same shard count.
	if rows.shards != totals.ShardCount || rows.rows != totals.RowCount {
		return comp, &ShardMismatchError{
			Domain: domain, Variant: variant,
			SealedShards: totals.ShardCount, GotShards: rows.shards,
			SealedRows: totals.RowCount, GotRows: rows.rows,
		}
	}
	return comp, nil
}

// walkedRows is what one selector's shards hold.
type walkedRows struct {
	shards int32
	rows   int64
	ndjson bytes.Buffer
}

// walkShards pages through the shards a selector resolves and decodes them.
//
// Shards are gathered then sorted by name before decoding, rather than decoded
// as each page arrives. A List is not ordered across pages in any way the
// client may rely on, and the concatenation order decides the component file's
// bytes, which the manifest digest covers. Sorting by name is deterministic
// because a shard's name is derived: the Workflow UID prefix, the Job name,
// and a zero-padded index, so lexical order is shard order within a Job and
// Jobs sort stably among themselves.
//
// Only the names are held across pages, not the payloads, so the walk's
// footprint stays one page of binaryData rather than the whole result set.
func walkShards(
	ctx context.Context, c client.Client, namespace string, selector labels.Selector,
) (walkedRows, []TruncationRecord, error) {
	var (
		out         walkedRows
		truncations []TruncationRecord
		rowShards   []string
		truncShards []string
	)

	token := ""
	for {
		var page corev1.ConfigMapList
		opts := []client.ListOption{
			client.InNamespace(namespace),
			client.MatchingLabelsSelector{Selector: selector},
			client.Limit(listPageSize),
		}
		if token != "" {
			opts = append(opts, client.Continue(token))
		}
		if err := c.List(ctx, &page, opts...); err != nil {
			return out, nil, fmt.Errorf("list result shards: %w", err)
		}
		for i := range page.Items {
			cm := &page.Items[i]
			switch {
			case cm.BinaryData[shard.RowsKey] != nil:
				rowShards = append(rowShards, cm.Name)
			case cm.BinaryData[shard.TruncationKey] != nil:
				truncShards = append(truncShards, cm.Name)
			}
		}
		token = page.Continue
		if token == "" {
			break
		}
	}

	slices.Sort(rowShards)
	slices.Sort(truncShards)

	for _, name := range rowShards {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
			return out, nil, fmt.Errorf("read result shard %s: %w", name, err)
		}
		decoded, err := shard.DecodeRows(cm)
		if err != nil {
			return out, nil, fmt.Errorf("decode result shard %s: %w", name, err)
		}
		for _, row := range decoded {
			line, err := row.MarshalJSON()
			if err != nil {
				return out, nil, fmt.Errorf("re-encode row from shard %s: %w", name, err)
			}
			out.ndjson.Write(line)
			out.ndjson.WriteByte('\n')
		}
		out.shards++
		out.rows += int64(len(decoded))
	}

	for _, name := range truncShards {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
			return out, nil, fmt.Errorf("read truncation record %s: %w", name, err)
		}
		dropped, err := shard.DecodeTruncation(cm.BinaryData[shard.TruncationKey])
		if err != nil {
			return out, nil, fmt.Errorf("decode truncation record %s: %w", name, err)
		}
		for _, d := range dropped {
			truncations = append(truncations, TruncationRecord{
				Reason:      d.Reason,
				Count:       d.Count,
				Node:        d.Node,
				ComponentID: d.ComponentID,
				Index:       d.Index,
			})
		}
	}

	return out, truncations, nil
}

// CollectTruncations flattens every category's truncation records, for the
// single truncation.json the bundle carries.
func CollectTruncations(comps []ResolvedComponent) []TruncationRecord {
	var out []TruncationRecord
	for _, c := range comps {
		out = append(out, c.Truncations...)
	}
	return out
}

// ComponentFileNamesUnique reports a collision between two categories' file
// names, which would silently overwrite one with the other.
//
// A domain and a variant cannot contain a path separator, because both are
// catalog directory names, so the only way two categories collide is by being
// the same category twice. That should be impossible; this makes it loud
// rather than a bundle quietly missing a category.
func ComponentFileNamesUnique(comps []ResolvedComponent) error {
	seen := make(map[string]string, len(comps))
	for _, c := range comps {
		name := c.FileName()
		if strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("category %s/%s produces an unusable file name %q", c.Domain, c.Variant, name)
		}
		if prev, dup := seen[name]; dup {
			return fmt.Errorf("categories %s and %s/%s both write %s",
				prev, c.Domain, c.Variant, filepath.Join(ComponentsDir, name))
		}
		seen[name] = c.Domain + "/" + c.Variant
	}
	return nil
}
