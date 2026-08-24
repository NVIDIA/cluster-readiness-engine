// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package catalog maps certification categories (domain/variant) to pre-configured
// WorkflowSpec builders. Each category is a YAML file under entries/{domain}/{variant}.yaml,
// embedded at compile time via //go:embed and loaded by loader.go.
// Adding a new certification category requires only adding a YAML file.
package catalog

import (
	"sort"

	corev1 "k8s.io/api/core/v1"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/gpu"
)

// categoryKey uniquely identifies a certification category.
type categoryKey struct {
	Domain  string
	Variant string
}

// BuildConfig holds deployment-specific configuration passed from the Certification
// spec to catalog builders. Fields are optional — catalogs use them when applicable.
type BuildConfig struct {
	// ImagePullSecrets are references to secrets for pulling container images.
	// If empty, the cluster's default image pull configuration is used.
	ImagePullSecrets []corev1.LocalObjectReference

	// StorageClassName is the StorageClass for PVC dependencies.
	// If nil, catalogs that create PVCs omit the field (cluster default).
	StorageClassName *string

	// NodesPerJob is the number of nodes per job for multi-node workloads.
	// Training and communication entries templatize this; single-node entries ignore it.
	NodesPerJob int32

	// GpusPerNode is the resolved number of GPUs per node (always non-zero).
	// The certification controller derives this from the GPU architecture label
	// or the user's explicit override.
	GpusPerNode int32

	// MlnxPerNode is the Mellanox NIC count per node, used by IB/RoCE templates
	// (Azure, OCI, TogetherAI). 0 means the templates omit nvidia.com/mlnxnics.
	// Resolved from gpu-defaults.yaml + platform overrides + user override.
	MlnxPerNode int32

	// EnableMNNVL controls the NCCL_MNNVL_ENABLE env var in training entries.
	// When true, entries set NCCL_MNNVL_ENABLE=1.
	EnableMNNVL bool

	// EnableCheckpoint enables checkpointing for training workloads.
	EnableCheckpoint bool

	// MaxSteps is the maximum training steps for NeMo 4 workloads.
	// 0 means use template default (50).
	MaxSteps int32

	// ExitDurationMins is the training duration in minutes for NeMo 6 workloads.
	// 0 means use template default (30).
	ExitDurationMins int32

	// GPUArchitecture is the GPU architecture string (e.g., "h100", "gb200").
	// Derived from the target nodeSelector's nvidia.com/gpu.product label.
	// Required — Build returns an error if empty.
	GPUArchitecture string

	// SaveInterval is the checkpoint save frequency in training steps.
	// 0 means use default (250).
	SaveInterval int32

	// SaveRetainInterval retains checkpoints at multiples of this value (NeMo 6).
	// 0 means use default (1000).
	SaveRetainInterval int32

	// SaveTopK keeps only the top K checkpoints by metric (NeMo 4).
	// 0 means use default (1).
	SaveTopK int32

	// StorageSize is the PVC size for checkpoint storage.
	// Empty means use default ("10Ti").
	StorageSize string

	// TestScale controls orchestration strategy for NCCL tests.
	// Empty means "full-scale" (default).
	TestScale string

	// MaxBytes is the max message size for NCCL tests (e.g., "16G", "32G").
	// Empty means use default ("16G").
	MaxBytes string

	// NumIterations is the NCCL -n flag (timed iterations per message size).
	// 0 means use default (100).
	NumIterations int32

	// NumCycles is the NCCL -N flag (run cycles, each printed separately).
	// 0 means use default (10).
	NumCycles int32

	// MaxConcurrent limits simultaneous jobs. 0 means unlimited.
	MaxConcurrent int32

	// MinGroupSize is the smallest group at which diagnose's bisection
	// stops splitting. 0 means use default (2).
	MinGroupSize int32

	// TimeoutPerJob is the max duration per job (e.g., "1h", "30m").
	// Empty means use default ("1h", or "15m" for diagnose).
	TimeoutPerJob string

	// MeasurementTimeout is the max duration to wait after Job success for
	// measurement data (e.g., "5m", "10m"). Empty means use controller default ("5m").
	MeasurementTimeout string

	// Thresholds maps metric names to CEL expressions for performance validation.
	Thresholds map[string]string

	// RepeatCount overrides the orchestration iteration count.
	// 0 means use catalog default.
	RepeatCount int32

	// MaxRestarts overrides the checkpoint maxRestarts for training workloads.
	// 0 means use catalog default. Only used when EnableCheckpoint is true.
	MaxRestarts int32
}

// Entry holds a builder that produces a WorkflowSpec for a given target.
type Entry struct {
	// Build returns a WorkflowSpec configured for the given target nodes
	// and deployment-specific configuration. Returns an error if the
	// configuration is invalid (e.g., nodesPerJob not a power of 2,
	// or fewer GPUs than required).
	Build func(target crev1alpha1.TargetSpec, config BuildConfig) (crev1alpha1.WorkflowSpec, error)

	// MinGPUs is the minimum number of GPUs (nodesPerJob × gpusPerNode)
	// required by this entry. 0 means no minimum. Build returns an error
	// if the requested GPU count is below this threshold.
	MinGPUs int32

	// TimeoutPerJob is the entry's default job timeout from its meta.yaml
	// (e.g., "2h" for dcgm-level4). Empty means the global default applies
	// (DefaultTimeoutPerJob, or DiagnoseTimeoutPerJob for diagnose scale).
	// Exposed so callers (e.g., the ncrectl --wait timeout derivation) can
	// compute a category's job budget without a full Build.
	TimeoutPerJob string

	// Iterations is the entry's default orchestration iteration count,
	// parsed from the entry template. Always >= 1.
	Iterations int

	// MaxValidNodes returns the largest node count <= availableNodes that
	// satisfies the entry's constraints (minGPUs, TP×PP divisibility).
	// Returns 0 if no valid count exists. When nil, any node count is valid.
	MaxValidNodes func(availableNodes, gpusPerNode int32, gpuArch string) int32
}

// registry holds all registered catalog entries.
var registry = map[categoryKey]Entry{}

// Register adds a catalog entry for the given domain and variant.
func Register(domain, variant string, entry Entry) {
	registry[categoryKey{Domain: domain, Variant: variant}] = entry
}

// CategoryInfo describes a registered catalog category.
type CategoryInfo struct {
	Domain  string `json:"domain"`
	Variant string `json:"variant"`
}

// List returns all registered catalog categories, sorted by domain then variant.
func List() []CategoryInfo {
	keys := make([]CategoryInfo, 0, len(registry))
	for k := range registry {
		keys = append(keys, CategoryInfo(k))
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Domain != keys[j].Domain {
			return keys[i].Domain < keys[j].Domain
		}
		return keys[i].Variant < keys[j].Variant
	})
	return keys
}

// GPUArchFromNodeSelector extracts the GPU architecture string from a nodeSelector.
// It parses the nvidia.com/gpu.product label (e.g., "NVIDIA-H100-80GB-HBM3" → "h100").
// Returns an empty string if the label is absent.
func GPUArchFromNodeSelector(nodeSelector map[string]string) string {
	return gpu.ParseProduct(nodeSelector["nvidia.com/gpu.product"])
}

// Lookup returns the catalog entry for the given domain and variant.
// Returns nil if the category is not registered.
func Lookup(domain, variant string) *Entry {
	entry, ok := registry[categoryKey{Domain: domain, Variant: variant}]
	if !ok {
		return nil
	}
	return &entry
}

// EffectiveTimeoutPerJob returns the timeoutPerJob string that Build would
// render for this entry: an explicit user override wins, then the entry's
// meta.yaml default, then the test-scale-dependent global default. Shares
// resolveTimeoutPerJob with buildTemplateData so the two cannot diverge.
func (e *Entry) EffectiveTimeoutPerJob(userTimeout, testScale string) string {
	return resolveTimeoutPerJob(userTimeout, e.TimeoutPerJob, testScale)
}

// ConfigArch returns the GPU architecture used for config file lookups.
// GB300 and GB200 use identical configs, so gb300 maps to gb200.
func ConfigArch(arch string) string {
	if arch == "gb300" {
		return "gb200"
	}
	return arch
}
