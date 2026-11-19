// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// outputFlags are the flags on `certification run` that name somewhere to
// write, each with the runConfig field it has to reach.
//
// A flag here is bound to a local in newRunCommand, so it is only honoured if
// that local is also copied onto the config the command executes. Nothing in
// the compiler connects the two: an unassigned field is simply the zero value,
// and the command then runs successfully while writing nothing.
var outputFlags = []struct{ flag, field string }{
	{"results-file", "resultsFile"},
	{"junit-file", "junitFile"},
	{"results-bundle", "resultsBundle"},
}

// TestRunForwardsEveryOutputFlag is the regression guard for a defect that
// made the headline feature of ADR-090 silently do nothing.
//
// `--results-bundle` was bound, validated against `--wait`, and checked for
// containment against the other export paths, and then never assigned to
// `cfg.resultsBundle`. So `certification run --wait --results-bundle DIR`
// accepted the flag, ran the certification, reported success and exported no
// bundle. With `--cleanup` it also deleted the cluster-side results, so the
// evidence was gone and the bundle that was supposed to carry it off the
// cluster was never written.
//
// Nothing caught it. The flag's own validation passed, `certification report`
// wires the same flag correctly and its tests cover that path, and no test
// asserted that the run command forwards what it binds.
//
// Checked at the source level rather than by executing the command, because
// the thing being asserted is the assignment itself: a test that ran the
// command would need a cluster, and one that inspected the config would have
// to reach inside the cobra closure that builds it.
func TestRunForwardsEveryOutputFlag(t *testing.T) {
	raw, err := os.ReadFile("certification.go")
	if err != nil {
		t.Fatalf("read certification.go: %v", err)
	}
	body := string(raw)

	run := functionBody(t, body, "func newRunCommand(")

	for _, f := range outputFlags {
		t.Run(f.flag, func(t *testing.T) {
			// The flag must still be bound here, or this entry is stale.
			bound := regexp.MustCompile(
				`cmd\.Flags\(\)\.StringVar\(&[A-Za-z]+, "` + regexp.QuoteMeta(f.flag) + `"`)
			if !bound.MatchString(run) {
				t.Fatalf("newRunCommand no longer binds --%s; if it moved, update this "+
					"test rather than deleting the entry", f.flag)
			}

			assigned := regexp.MustCompile(`cfg\.` + regexp.QuoteMeta(f.field) + `\s*=`)
			if !assigned.MatchString(run) {
				t.Errorf("newRunCommand binds --%s but never assigns cfg.%s, so the flag "+
					"is accepted and then ignored: the command reports success having "+
					"written nothing to the path the operator asked for.", f.flag, f.field)
			}
		})
	}
}

// functionBody returns the source of one function, from its signature to the
// closing brace at column zero.
func functionBody(t *testing.T, src, signature string) string {
	t.Helper()
	i := strings.Index(src, signature)
	if i < 0 {
		t.Fatalf("no function matching %q; if it was renamed, update this test", signature)
	}
	rest := src[i:]
	if before, _, ok := strings.Cut(rest, "\n}\n"); ok {
		return before
	}
	return rest
}
