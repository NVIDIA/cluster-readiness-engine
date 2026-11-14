// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"slices"
	"strconv"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
)

// maxDegradedComponents caps the lists on JobStatus and WorkflowStatus, and
// matches the MaxItems marker on both. The exact count travels beside the
// list, so the cap limits which components are named rather than how many are
// reported.
const maxDegradedComponents = 16

// outcomeBelow is the per-sample marker a producer sets when a value is under
// the floor it was given.
//
// The floor itself never reaches the controller, and that is deliberate: it is
// a template parameter the harness resolves, and a verdict rule the ADR-090
// follow-on will replace. Keying on the producer's own marker means this
// derivation works for any producer that reports one, and yields nothing for a
// producer that enforces no floor, with no special cases either way.
const outcomeBelow = "below"

// outcomeError is the marker a producer sets when a sample could not be taken
// at all: the kernel would not run, the device returned an error, the tool
// exited non-zero.
//
// Counted as degradation alongside "below", because for a burn-in it is the
// stronger signal of the two. A GPU that returns a low number is slow; a GPU
// that intermittently cannot run the kernel is faulty. Keying the summary on
// "below" alone meant a component that errored on 59 of 60 sweeps and
// completed one sample above the floor was reported as healthy and named
// nowhere, which is the opposite of what this summary is for.
const outcomeError = "error"

// degradedKey identifies one component within a result set.
type degradedKey struct {
	node        string
	componentID string
	index       int32
}

// degradedComponents summarises the components whose samples went below the
// floor, worst first, and returns the exact count alongside the capped list.
//
// Built from the rows the caller already holds, so it costs one pass over a
// slice that is in memory anyway. The alternative, resolving it later from the
// shards, only works for someone who asked for a result bundle; this is what
// puts the finding in the report of a run that passed.
//
// Worst first means the lowest value reached. That ordering is what makes the
// cap defensible: the components a cap drops are the ones closest to healthy.
func degradedComponents(rows []shard.Row) (int32, []nvcrev1alpha1.DegradedComponent) {
	type agg struct {
		node, id   string
		index      int32
		minV, maxV float64
		below      int64
		errored    int64
		seen       bool
	}
	byComponent := map[degradedKey]*agg{}

	for _, row := range rows {
		k := degradedKey{node: row.Node, componentID: row.ComponentID, index: row.Index}
		a := byComponent[k]
		if a == nil {
			a = &agg{node: row.Node, id: row.ComponentID, index: row.Index}
			byComponent[k] = a
		}
		if !a.seen || row.Value < a.minV {
			a.minV = row.Value
		}
		if !a.seen || row.Value > a.maxV {
			a.maxV = row.Value
		}
		a.seen = true
		switch row.Outcome {
		case outcomeBelow:
			a.below++
		case outcomeError:
			a.errored++
		}
	}

	out := make([]nvcrev1alpha1.DegradedComponent, 0, len(byComponent))
	for _, a := range byComponent {
		if a.below == 0 && a.errored == 0 {
			continue
		}
		out = append(out, nvcrev1alpha1.DegradedComponent{
			Node:           a.node,
			ComponentID:    a.id,
			Index:          a.index,
			MaxValue:       strconv.FormatFloat(a.maxV, 'f', 2, 64),
			MinValue:       strconv.FormatFloat(a.minV, 'f', 2, 64),
			BelowSamples:   a.below,
			ErroredSamples: a.errored,
		})
	}

	sortDegraded(out)
	total := int32(len(out)) //nolint:gosec // bounded by the row set's component count
	if len(out) > maxDegradedComponents {
		out = out[:maxDegradedComponents]
	}
	return total, out
}

// sortDegraded orders components worst first, then by identity.
//
// The comparison parses minValue rather than comparing the stored strings.
// Lexical order on a formatted number is not numeric order: "20.00" sorts
// before "9.00", so a string comparison would put a GPU at 20 TFLOPS ahead of
// one at 9 and the cap would then drop the worse of the two. The identity tie
// break keeps the list stable across reconciles, because an unstable order
// makes every recompute look like a change and flaps the status write.
func sortDegraded(d []nvcrev1alpha1.DegradedComponent) {
	slices.SortFunc(d, func(x, y nvcrev1alpha1.DegradedComponent) int {
		if c := cmp.Compare(parseDegradedValue(x.MinValue), parseDegradedValue(y.MinValue)); c != 0 {
			return c
		}
		if c := cmp.Compare(x.Node, y.Node); c != 0 {
			return c
		}
		if c := cmp.Compare(x.ComponentID, y.ComponentID); c != 0 {
			return c
		}
		return cmp.Compare(x.Index, y.Index)
	})
}

// parseDegradedValue reads a stored value back for comparison. An unparseable
// one sorts as zero, which places it at the worst end: a value the controller
// cannot read is not a value it should quietly rank as healthy.
func parseDegradedValue(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// mergeDegraded folds one seal's degraded components into a running set, worst
// first and capped, keeping the caller to the exact total it tracks itself.
//
// Used by the Workflow to aggregate across its groups. Re-derived from the
// seals on every recompute rather than accumulated, for the same reason the
// totals are: the function that observes a terminal group is re-entered, so a
// running append would list the same component repeatedly.
func mergeDegraded(
	into []nvcrev1alpha1.DegradedComponent, add []nvcrev1alpha1.DegradedComponent,
) []nvcrev1alpha1.DegradedComponent {
	merged := make([]nvcrev1alpha1.DegradedComponent, 0, len(into)+len(add))
	merged = append(merged, into...)
	merged = append(merged, add...)
	sortDegraded(merged)
	if len(merged) > maxDegradedComponents {
		merged = merged[:maxDegradedComponents]
	}
	return merged
}
