// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// gpuArchFallbackMessage renders the user-facing explanation emitted when the
// detected GPU architecture is not listed in the catalog's gpu-defaults.yaml,
// so GPUDefaults sized the workflow on its Go-side fallback instead of a known
// node shape (ADR-092). It returns "" when the architecture is empty, known,
// or the "unknown" sentinel detection returns for unlabeled nodes (that is a
// missing label, ADR-082's problem, not a missing table entry), so callers can
// emit unconditionally. fieldHint names where the caller's users set the
// counts (the field paths differ between a Certification and a WorkloadRun).
// The text is used verbatim as the GPUArchitectureDefaults event message by
// both controllers and printed by the CLI render paths.
func gpuArchFallbackMessage(gpuArch string, nd catalog.NodeDefaults, fieldHint string) string {
	if gpuArch == "" || gpuArch == gpuArchUnknown || catalog.KnownGPUArch(gpuArch) {
		return ""
	}
	return fmt.Sprintf("GPU architecture %q has no entry in the catalog's GPU defaults;"+
		" the job is sized as gpusPerNode=%d mlnxPerNode=%d. If the nodes differ, set %s,"+
		" and file an issue so the architecture can be added to the catalog.",
		gpuArch, nd.GpusPerNode, nd.MlnxPerNode, fieldHint)
}

// Field hints for gpuArchFallbackMessage, one per resource kind.
const (
	gpuArchFieldHintCertification = "spec.gpusPerNode and spec.mlnxPerNode (or categories[].options)"
	gpuArchFieldHintWorkloadRun   = "spec.gpusPerNode and spec.mlnxPerNode"
)
