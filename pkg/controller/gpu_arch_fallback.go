// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// gpuArchFallbackMessage renders the user-facing explanation emitted when the
// detected GPU architecture is not listed in the catalog's gpu-defaults.yaml,
// so GPUDefaults had no node shape for it and fell back to its Go-side
// default (ADR-094). gpusPerNode and mlnxPerNode are the caller's resolved
// counts, after any spec or category override, so the text states what the
// run is actually sized at next to what the catalog fell back to; a user who
// already set both fields sees their values, not the fallback. It returns ""
// when the architecture is empty, known, or the "unknown" sentinel detection
// returns for unlabeled nodes (that is a missing label, ADR-082's problem,
// not a missing table entry), so callers can emit unconditionally. fieldHint
// names where the caller's users set the counts (the field paths differ
// between a Certification and a WorkloadRun). The text is used verbatim as
// the GPUArchitectureDefaults event message by both controllers and printed
// by the CLI render paths.
func gpuArchFallbackMessage(gpuArch string, gpusPerNode, mlnxPerNode int32, fieldHint string) string {
	if gpuArch == "" || gpuArch == gpuArchUnknown || catalog.KnownGPUArch(gpuArch) {
		return ""
	}
	fallback := catalog.GPUDefaults(gpuArch, "")
	return fmt.Sprintf("GPU architecture %q has no entry in the catalog's GPU defaults, which fall back to"+
		" gpusPerNode=%d mlnxPerNode=%d for it; this run is sized at gpusPerNode=%d mlnxPerNode=%d."+
		" If the nodes differ, set %s, and file an issue so the architecture can be added to the catalog.",
		gpuArch, fallback.GpusPerNode, fallback.MlnxPerNode, gpusPerNode, mlnxPerNode, fieldHint)
}

// Field hints for gpuArchFallbackMessage, one per resource kind.
const (
	gpuArchFieldHintCertification = "spec.gpusPerNode and spec.mlnxPerNode (or spec.categories[].options)"
	gpuArchFieldHintWorkloadRun   = "spec.gpusPerNode and spec.mlnxPerNode"
)
