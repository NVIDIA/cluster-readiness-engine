// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"context"
	"fmt"
	"io"

	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/bundle"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

// writeResultsBundle resolves a WorkloadRun's per-component result shards and
// writes the bundle ADR-090 defines.
//
// A WorkloadRun has one Workflow and no catalog category, so there is at most
// one component file and it is named for the run. Everything else about the
// bundle, including the three reader rules, is the same artifact the
// certification path writes: a consumer should not have to care which command
// produced the directory it was handed.
func writeResultsBundle(
	ctx context.Context, c client.Client,
	run *nvcrev1alpha1.WorkloadRun, r *report.CertReport, dir string, out io.Writer,
) error {
	var comps []bundle.ResolvedComponent

	if run.Status.WorkflowRef != nil {
		ns := run.Status.WorkflowRef.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		wf := &nvcrev1alpha1.Workflow{}
		key := client.ObjectKey{Name: run.Status.WorkflowRef.Name, Namespace: ns}
		if err := c.Get(ctx, key, wf); err != nil {
			// An error rather than a bundle without components. The run says
			// it has a Workflow, so a Workflow that cannot be read means the
			// result set is unknown, not empty, and a bundle that quietly
			// omits it would certify the omission.
			return fmt.Errorf("read workflow %s/%s to locate its result shards: %w",
				ns, run.Status.WorkflowRef.Name, err)
		}
		comp, found, err := bundle.ResolveWorkflow(ctx, c, run.Name, wf)
		if err != nil {
			return fmt.Errorf("resolve component results: %w", err)
		}
		if found {
			comps = append(comps, comp)
		}
	}

	if err := bundle.ComponentFileNamesUnique(comps); err != nil {
		return fmt.Errorf("write results bundle: %w", err)
	}
	if err := bundle.Write(dir, bundle.Contents{
		Reports:     []*report.CertReport{r},
		Components:  comps,
		Truncations: bundle.CollectTruncations(comps),
	}); err != nil {
		return fmt.Errorf("write results bundle: %w", err)
	}

	// Verify what was just written, before reporting it. A writer that cannot
	// produce a bundle its own reader accepts has written something no
	// consumer should trust.
	m, err := bundle.Verify(dir)
	if err != nil {
		return fmt.Errorf("the results bundle just written does not verify: %w", err)
	}

	var rows int64
	var partial bool
	for _, set := range m.Components {
		rows += set.RowCount
		if !set.Final || set.TruncatedRowCount > 0 {
			partial = true
		}
	}
	_, _ = fmt.Fprintf(out, "Results bundle written to %s (%d files", dir, m.FileCount)
	if len(m.Components) > 0 {
		_, _ = fmt.Fprintf(out, ", %d component rows", rows)
	}
	_, _ = fmt.Fprintln(out, ")")
	if partial {
		_, _ = fmt.Fprintln(out,
			"Warning: some component results are incomplete; see components in "+
				bundle.ManifestName+" and "+bundle.TruncationName)
	}
	return nil
}
