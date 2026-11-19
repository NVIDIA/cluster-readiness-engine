// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
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
	Scope   string
	Domain  string
	Variant string
	// Workflow is the name of the Workflow the category ran as, when it had
	// one. It is what makes the file name unique.
	//
	// Domain and variant do not: two categories can name the same pair and
	// differ only in their options, which is the case categoryOptionsSuffix
	// exists for, and that is not a hypothetical. Two compute/cutlass-gemm
	// categories with different floors are the headline use of the suffix, and
	// under a name built from domain and variant alone they collide,
	// ComponentFileNamesUnique refuses the bundle, and the run's entire
	// per-component evidence becomes unobtainable. The Workflow name already
	// carries the suffix, so it distinguishes exactly the categories that are
	// distinguishable.
	Workflow          string
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
// Built from whichever of scope, domain, variant and Workflow name are set, so
// a certification category reads as
// "<cert>-<domain>-<variant>-<workflow>.ndjson" and a WorkloadRun, which has
// no catalog category at all, reads as "<run>.ndjson" rather than carrying
// empty segments.
//
// The Workflow name is what carries uniqueness; domain and variant are kept
// ahead of it because they are what a human scans the directory for, and the
// Workflow name alone reads as an opaque token.
func (c ResolvedComponent) FileName() string {
	parts := make([]string, 0, 4)
	for _, part := range []string{c.Scope, c.Domain, c.Variant, c.Workflow} {
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
		// The Workflow name is what makes the component file name unique:
		// domain and variant repeat across categories that differ only in
		// their options.
		if cs.WorkflowRef != nil {
			comp.Workflow = cs.WorkflowRef.Name
		}
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

	// A sealed loss with no record of it is refused for the same reason, and
	// the counts above cannot see it. A dropped row was never in a shard, so
	// removing the truncation sibling leaves both counts in perfect agreement
	// while the bundle loses the identities and reasons that record exists to
	// carry: a manifest stating rows were lost, beside a truncation.json
	// holding the empty list this format defines as nothing having been lost.
	// The split is deliberate on the write side, because a thousand groups'
	// reasons do not fit in a Workflow's status, so the Workflow keeps the
	// magnitude and the ConfigMap keeps the detail. Half of that record is not
	// a bundle.
	//
	// Presence, deliberately not a count. Equality looks available here and
	// would refuse runs whose evidence is intact:
	//
	//   - the producer's ledger is bounded at a thousand named records while
	//     DroppedTotal stays exact, so ADR-090's 480,000 row corpus resolves a
	//     thousand records against a count three orders larger.
	//   - a record legitimately carries Count 0, for a log page that could not
	//     be fetched or a producer that never came up, so a set of them sums
	//     below what it reports.
	//   - a superseded attempt's sibling whose delete failed is still matched
	//     by this selector while its seal is no longer in the total, so the sum
	//     read can exceed the sum sealed.
	//
	// What does hold is a lower bound on the magnitude. TruncatedRowCount is
	// summed from the very records the siblings carry, and the one place the
	// two could drift, a ledger capped at maxDroppedRecords, is reconciled onto
	// both sides by the same call, so the counts a Job writes sum to the count
	// its seal reports. Across Jobs, then, the resolved records cannot sum
	// below what the run sealed unless some Job's sibling is missing.
	//
	// A bound rather than equality, because the sum can legitimately EXCEED.
	// A seal dropped past the retained-failure cap leaves its records behind
	// while its count is no longer in the total, and a re-drive that lost
	// nothing leaves an earlier attempt's sibling in place. Both already fail
	// the shard counts above, so refusing them here would only restate it,
	// while refusing on equality would also refuse runs whose evidence is whole.
	//
	// Presence alone was not enough, which is what this replaces: with two Jobs
	// recording losses, one surviving record satisfied a category-wide presence
	// check while the other Job's identities and reasons were gone, and neither
	// count moved because a dropped row was never in a shard.
	var resolvedDropped int64
	for _, tr := range comp.Truncations {
		resolvedDropped += tr.Count
	}
	if resolvedDropped < totals.TruncatedRowCount {
		return comp, fmt.Errorf(
			"%s/%s: the run sealed %d truncated row(s) and the truncation records that"+
				" resolved account for only %d; refusing to write a bundle whose"+
				" truncation.json understates the loss its manifest records",
			domain, variant, totals.TruncatedRowCount, resolvedDropped)
	}
	return comp, nil
}

// walkedRows is what one selector's shards hold.
type walkedRows struct {
	shards int32
	rows   int64
	ndjson bytes.Buffer
}

// labelShardJob is the Job a shard was written for, stamped by the controller
// in writeShardConfigMap.
//
// Used to group shard indices by Job, nothing more. It deliberately does not
// gate which shards are read. The obvious use would be to admit only shards
// whose Job the Workflow lists as sealed, which would exclude a superseded
// attempt whose shard delete failed, but the Workflow does not retain a Job
// name for every row it counts: diagnoseSetGroups folds a round's seals into
// RetiredComponentResults, a bare totals struct, and then replaces the group
// list those names lived on without deleting the shards. Gating on
// attributability would therefore refuse a bundle for every run that entered
// fault isolation, which is the mode these rows are most worth having in. An
// orphaned attempt is caught by the count gate in resolveOne instead.
const labelShardJob = "nvcre.nvidia.com/job"

// shardIndexOf reads a row shard's position within its Job.
//
// From the data block rather than the name, because the name is derived and the
// block is what the encoder wrote. A shard with no readable index cannot be
// placed, and a set whose order is unknown cannot be concatenated into rows in
// emission order, so this is an error rather than a default.
func shardIndexOf(cm *corev1.ConfigMap) (int, error) {
	raw, ok := cm.Data[shard.ShardIndexKey]
	if !ok {
		return 0, fmt.Errorf("shard %s carries no %s, so its position in the set is unknown",
			cm.Name, shard.ShardIndexKey)
	}
	idx, err := strconv.Atoi(raw)
	if err != nil || idx < 0 {
		return 0, fmt.Errorf("shard %s has %s=%q, which is not a position", cm.Name,
			shard.ShardIndexKey, raw)
	}
	return idx, nil
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
	// Shard index per Job, so a duplicate index cannot double-count rows.
	// shard.DecodeRows deliberately leaves this to bundle assembly.
	indexByJob := map[string]map[int]string{}

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
				idx, err := shardIndexOf(cm)
				if err != nil {
					return out, nil, err
				}
				job := cm.Labels[labelShardJob]
				if indexByJob[job] == nil {
					indexByJob[job] = map[int]string{}
				}
				if prev, dup := indexByJob[job][idx]; dup {
					// Named in lexical order, not the order the List happened
					// to return them in. A List is unordered, so without this
					// the same cluster state produces either of two messages.
					first, second := min(prev, cm.Name), max(prev, cm.Name)
					return out, nil, fmt.Errorf(
						"shards %s and %s both claim index %d for job %s: two shards at one "+
							"index means the set is not the one that was sealed, and taking "+
							"both would count a component's rows twice",
						first, second, idx, job)
				}
				indexByJob[job][idx] = cm.Name
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
				Message:     d.Message,
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
// Two categories CAN name the same domain and variant and differ only in
// their options, which is what categoryOptionsSuffix exists for, so the file
// name carries the Workflow name as well. An earlier version of this comment
// claimed the collision was impossible; it was reachable by the headline use
// of that suffix, two compute/cutlass-gemm categories with different floors,
// and it refused the whole bundle rather than one category.
//
// This check stays as the backstop. Domain and variant cannot contain a path
// separator because both are catalog directory names, and the Workflow name is
// a DNS-1035 label, so a collision now means two genuinely identical
// categories; that is loud here rather than a bundle quietly missing one.
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
