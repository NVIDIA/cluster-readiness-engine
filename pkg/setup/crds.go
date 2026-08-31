// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"
)

// crdFieldManager is the server-side-apply field manager `setup init` uses
// when reconciling the chart CRDs.
const crdFieldManager = "nvcrectl-setup"

// fetchChartCRDs runs `helm show crds` against the same OCI chart source and
// version installHelmRelease deploys, and returns the chart's crds/ directory
// as one multi-document YAML stream. Only stdout is treated as manifest data;
// stderr (registry warnings and errors) is surfaced separately on failure so
// it can never corrupt the YAML stream.
func fetchChartCRDs(helmPath, chartVersion string, out io.Writer) ([]byte, error) {
	args := []string{"show", "crds", helmChartOCI, "--version", chartVersion}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(helmPath, args...) // #nosec G204 -- helmPath and args come from this CLI, not from untrusted input
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_, _ = io.Copy(out, &stderr)
		printGHCR403Hint(out, stderr.String())
		return nil, fmt.Errorf("helm show crds: %w", err)
	}
	return stdout.Bytes(), nil
}

// applyChartCRDs server-side-applies every CustomResourceDefinition document
// in manifests with force ownership. Helm applies a chart's crds/ directory
// only on the first install, so `helm upgrade --install` alone leaves the
// CRDs at the schema of whichever version first installed them (issue #145).
// Reconciling them from the chart before every upgrade closes that gap;
// server-side apply is idempotent, so a fresh install and an unchanged
// re-run are unaffected.
//
// Known limitation: SSA only removes a field once no manager owns it. The
// managedFields entry helm left behind on first install co-owns every field
// it created, and helm never re-applies CRDs, so a schema field that a
// future chart version REMOVES will linger on the installed CRD until
// something clears helm's ownership. That is strictly better than the
// pre-#145 behavior (no update at all), and new/changed fields — the actual
// drift in issue #145 — are fully reconciled.
func applyChartCRDs(ctx context.Context, c client.Client, manifests []byte, out io.Writer) error {
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifests)))
	applied := 0
	for {
		doc, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read chart CRD stream: %w", readErr)
		}

		// A document may be blank or comment-only (helm show crds emits a
		// separator per crds/ file, and the files carry leading separators of
		// their own); probing with a map skips those without tripping on the
		// missing kind.
		var probe map[string]any
		if err := sigsyaml.Unmarshal(doc, &probe); err != nil {
			return fmt.Errorf("parse chart CRD document: %w", err)
		}
		if len(probe) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{Object: probe}
		if obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
			client.FieldOwner(crdFieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply CRD %s: %w", obj.GetName(), err)
		}
		_, _ = fmt.Fprintf(out, "  CRD %s applied.\n", obj.GetName())
		applied++
	}
	if applied == 0 {
		return errors.New("chart contains no CustomResourceDefinition documents")
	}
	return nil
}
