// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"context"
	"fmt"
	"io"

	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/bundle"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

// reportTargets is where a report should be written.
//
// A struct rather than three more string parameters on handleReport, which
// already takes seven: results file, JUnit file and bundle directory are three
// adjacent strings, and transposing two of them compiles, runs, and writes
// each artifact to the other's path.
type reportTargets struct {
	resultsFile   string
	junitFile     string
	resultsBundle string
}

// bundleComponents aliases the resolved component type so the command layer
// does not spell out the bundle package at every call.
type bundleComponents = bundle.ResolvedComponent

// resolveBundleComponents walks one certification's per-component result
// shards.
//
// The shard walk is the CLI's job, not the controller's: shards are gzipped
// NDJSON inside ConfigMap binaryData, sharded to stay under the API server's
// object limit, and the point of the bundle is that a consumer never sees any
// of that (ADR-090 decision 4). nvcrectl runs under the user's kubeconfig, so
// it can list ConfigMaps; the controller's own grant deliberately cannot.
func resolveBundleComponents(
	ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification,
) ([]bundleComponents, error) {
	comps, err := bundle.ResolveCertification(ctx, c, cert)
	if err != nil {
		return nil, fmt.Errorf("resolve component results: %w", err)
	}
	return comps, nil
}

// writeBundleDir writes the bundle and verifies what it just wrote.
//
// A resolution or write failure fails the command rather than degrading to a
// partial bundle. That is the one behaviour this path must get right: a bundle
// short of its rows would still carry a manifest certifying it as whole, so
// anything short has to stop the write. A results file, if one was asked for,
// has already been written by the caller and is unaffected.
func writeBundleDir(
	dir string, reports []*report.CertReport, comps []bundleComponents, out io.Writer,
) error {
	if err := bundle.ComponentFileNamesUnique(comps); err != nil {
		return fmt.Errorf("write results bundle: %w", err)
	}
	if err := bundle.Write(dir, bundle.Contents{
		Reports:     reports,
		Components:  comps,
		Truncations: bundle.CollectTruncations(comps),
	}); err != nil {
		return fmt.Errorf("write results bundle: %w", err)
	}

	// Verify what was just written, before telling the user it is there.
	//
	// Not a belt-and-braces check: the three reader rules are normative, and a
	// writer that cannot produce a bundle its own reader accepts has written
	// something no consumer should trust. Running them here means a digest or
	// file-count disagreement surfaces as a failed command rather than as a
	// refusal weeks later, when whatever caused it is no longer reproducible.
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
		// Said plainly rather than left for the consumer to notice in the
		// manifest. A bundle read from a run that was still producing, or one
		// that lost rows, is a legitimate artifact and a misleading one to
		// treat as complete.
		_, _ = fmt.Fprintln(out,
			"Warning: some component results are incomplete; see components in "+
				bundle.ManifestName+" and "+bundle.TruncationName)
	}
	return nil
}

// writeResultsBundle is the single-certification path, used by handleReport
// after a --wait run.
func writeResultsBundle(
	ctx context.Context, c client.Client,
	cert *nvcrev1alpha1.Certification, r *report.CertReport, dir string, out io.Writer,
) error {
	comps, err := resolveBundleComponents(ctx, c, cert)
	if err != nil {
		return err
	}
	return writeBundleDir(dir, []*report.CertReport{r}, comps, out)
}
