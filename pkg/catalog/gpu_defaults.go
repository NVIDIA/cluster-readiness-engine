// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"sync"

	"sigs.k8s.io/yaml"
)

// NodeDefaults holds per-node hardware counts for a GPU architecture.
// Populated from entries/_lib/gpu-defaults.yaml on first use and consumed by
// the controller and CLI to derive GpusPerNode/MlnxPerNode without hardcoding.
type NodeDefaults struct {
	GpusPerNode int32 `json:"gpusPerNode" yaml:"gpusPerNode"`
	MlnxPerNode int32 `json:"mlnxPerNode" yaml:"mlnxPerNode"`
}

// fallbackGpusPerNode is returned when the requested architecture is not
// listed in gpu-defaults.yaml. The 4-GPU baseline matches the GB200/GB300
// node shape and is the safest default for unknown hardware.
const fallbackGpusPerNode int32 = 4

// gpuDefaultsData is the parsed shape of entries/_lib/gpu-defaults.yaml.
type gpuDefaultsData struct {
	Defaults          map[string]NodeDefaults            `json:"defaults" yaml:"defaults"`
	PlatformOverrides map[string]map[string]NodeDefaults `json:"platformOverrides" yaml:"platformOverrides"`
}

var (
	gpuDefaults     gpuDefaultsData
	gpuDefaultsErr  error
	gpuDefaultsOnce sync.Once
)

// LoadGPUDefaults forces the embedded gpu-defaults.yaml to be parsed and
// returns any error from that parse. Safe to call multiple times — the file
// is loaded exactly once and the result is cached.
//
// Call this at process startup (e.g. from cmd/main.go) to fail fast if the
// embedded catalog data is malformed. Callers that don't invoke this will
// still trigger a lazy load on the first GPUDefaults call; in that path,
// parse errors are swallowed and architectures fall back to the safe default.
func LoadGPUDefaults() error {
	ensureGPUDefaultsLoaded()
	return gpuDefaultsErr
}

// GPUDefaults returns node hardware defaults for the given GPU architecture
// and platform. Platform overrides (e.g. OCI L40s) are applied on top of
// architecture defaults — only fields explicitly set in the platform override
// take effect; unset fields fall through to the architecture defaults.
//
// Unknown architectures get {GpusPerNode: fallbackGpusPerNode, MlnxPerNode: 0}.
// An empty platform skips override resolution and returns architecture defaults
// only — this preserves callers that don't yet have platform context.
func GPUDefaults(gpuArch, platform string) NodeDefaults {
	ensureGPUDefaultsLoaded()
	nd, ok := gpuDefaults.Defaults[gpuArch]
	if !ok {
		nd = NodeDefaults{GpusPerNode: fallbackGpusPerNode}
	}
	if platform == "" {
		return nd
	}
	platformDefaults, ok := gpuDefaults.PlatformOverrides[platform]
	if !ok {
		return nd
	}
	override, ok := platformDefaults[gpuArch]
	if !ok {
		return nd
	}
	if override.GpusPerNode != 0 {
		nd.GpusPerNode = override.GpusPerNode
	}
	if override.MlnxPerNode != 0 {
		nd.MlnxPerNode = override.MlnxPerNode
	}
	return nd
}

// ensureGPUDefaultsLoaded loads entries/_lib/gpu-defaults.yaml on first call
// and caches both the parsed data and any error for subsequent calls.
func ensureGPUDefaultsLoaded() {
	gpuDefaultsOnce.Do(func() {
		gpuDefaultsErr = loadGPUDefaults()
	})
}

// loadGPUDefaults reads entries/_lib/gpu-defaults.yaml from the embedded
// catalog filesystem and populates the package-level gpuDefaults.
func loadGPUDefaults() error {
	data, err := entriesFS.ReadFile("entries/_lib/gpu-defaults.yaml")
	if err != nil {
		return fmt.Errorf("read embedded gpu-defaults.yaml: %w", err)
	}
	if err := yaml.Unmarshal(data, &gpuDefaults); err != nil {
		return fmt.Errorf("parse gpu-defaults.yaml: %w", err)
	}
	return nil
}
