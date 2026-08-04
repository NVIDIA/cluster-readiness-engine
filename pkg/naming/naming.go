// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package naming

import (
	"fmt"
	"hash/fnv"
	"strings"
)

const (
	// MaxK8sNameLen is the Kubernetes DNS-1123 label limit.
	MaxK8sNameLen = 63

	// MaxWorkloadNameLen is the safe max for TrainJob names,
	// leaving room for JobSet pod suffixes. The longest replicatedJob name
	// is "launcher", giving suffix "-launcher-{N}-{hash}" ~18 chars.
	MaxWorkloadNameLen = 44

	// MaxJobNameLen is the safe max for Job names,
	// leaving room for the "-workload" suffix (9 chars).
	MaxJobNameLen = 35

	// MaxWorkflowNameLen is the safe max for Workflow names,
	// leaving room for "-job" (4 chars) + "-workload" (9 chars) downstream.
	MaxWorkflowNameLen = 31

	hashLen      = 5
	separatorLen = 1
)

// ExtractHash returns the trailing hash from a Truncate'd name.
// e.g., "gpu-cluster-cert-communic-873b9" → "873b9".
// If the name has no hash (no hyphen or too short), returns a computed hash.
func ExtractHash(name string) string {
	if idx := strings.LastIndex(name, "-"); idx >= 0 {
		tail := name[idx+1:]
		if len(tail) == hashLen {
			return tail
		}
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("%05x", h.Sum32())[:hashLen]
}

// Truncate returns name unchanged if it fits within maxLen.
// Otherwise, it truncates to (maxLen - 6) characters, strips trailing
// hyphens, and appends "-{5-char FNV-32a hash}" of the full original name.
// The result is deterministic: same input always produces the same output.
func Truncate(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}

	prefixLen := max(maxLen-hashLen-separatorLen, 1)

	prefix := name[:prefixLen]
	prefix = strings.TrimRight(prefix, "-")

	h := fnv.New32a()
	h.Write([]byte(name))
	hash := fmt.Sprintf("%05x", h.Sum32())[:hashLen]

	return prefix + "-" + hash
}
