// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
)

// Default values for template variables when not specified by the user.
const (
	DefaultMaxSteps           = 50
	DefaultExitDurationMins   = 30
	DefaultSaveInterval       = 250
	DefaultSaveRetainInterval = 1000
	DefaultSaveTopK           = 1
	DefaultStorageSize        = "10Ti"
	DefaultTestScale          = crev1alpha1.TestScaleFullScale
	DefaultMaxBytes           = "16G"
	DefaultNumIterations      = 100
	DefaultNumCycles          = 10
	DefaultMinGroupSize       = 2

	// Diagnose mode uses fewer iterations for faster fault detection.
	DiagnoseNumIterations = 5
	DiagnoseNumCycles     = 1
	DiagnoseTimeoutPerJob = "15m"

	// DefaultWorkloadRunTimeoutPerJob bounds a WorkloadRun Job when the user
	// sets no timeoutPerJob. Without it Execution.TimeoutPerJob stays nil,
	// isJobTimedOut always returns false, and a Job whose pods can never
	// schedule runs until someone notices: one such run sat InProgress for
	// 4h10m against a node with no allocatable GPUs.
	//
	// This is a backstop against never finishing, not a performance bound. It
	// is deliberately far longer than the catalog's 1h, because a WorkloadRun
	// is arbitrary user work and cutting a legitimate long training run short
	// would be worse than the hang. Set spec.orchestration.timeoutPerJob for
	// anything tighter.
	DefaultWorkloadRunTimeoutPerJob = "24h"

	// DefaultTimeoutPerJob is the default timeout for communication jobs.
	// Prevents pods from running indefinitely on launcher restarts.
	DefaultTimeoutPerJob = "1h"
)

//go:embed all:entries
var entriesFS embed.FS

// EntriesFS returns the embedded catalog entries filesystem.
// Used by internal/platform to load shared _lib/ templates.
func EntriesFS() embed.FS { return entriesFS }

// TemplateData holds the data passed to catalog YAML templates at Build time.
// Fields are populated from BuildConfig. Add new fields here for future
// template variables — no loader changes required.
type TemplateData struct {
	// ImagePullSecrets are references to secrets for pulling container images.
	// Templates use: {{- if .ImagePullSecrets }} ... {{- range .ImagePullSecrets }}
	ImagePullSecrets []corev1.LocalObjectReference

	// StorageClassName is the StorageClass for PVC dependencies.
	// Empty string means omit (use cluster default).
	// Templates use: {{- if .StorageClassName }}
	StorageClassName string

	// NodesPerJob is the number of nodes per job for multi-node workloads.
	// Templates use: {{ .NodesPerJob }}
	NodesPerJob int32

	// GpusPerNode is the resolved number of GPUs per node (always non-zero at Build time).
	// Templates use: {{ .GpusPerNode }}
	GpusPerNode int32

	// MlnxPerNode is the Mellanox NIC count per node for IB/RoCE platforms.
	// 0 means omit nvidia.com/mlnxnics. Templates use: {{ .MlnxPerNode }}
	MlnxPerNode int32

	// EnableMNNVL controls the NCCL_MNNVL_ENABLE env var.
	// Templates use: {{ if .EnableMNNVL }}1{{ else }}0{{ end }}
	EnableMNNVL bool

	// EnableCheckpoint enables checkpointing for training workloads.
	// Templates use: {{ .EnableCheckpoint }}
	EnableCheckpoint bool

	// MaxSteps is the maximum training steps (always non-zero after defaults).
	// Templates use: {{ .MaxSteps }}
	MaxSteps int32

	// ExitDurationMins is the training duration in minutes (always non-zero after defaults).
	// Templates use: {{ .ExitDurationMins }}
	ExitDurationMins int32

	// GPUArchitecture is the GPU architecture string (e.g., "h100", "gb200").
	// Templates use: {{ .GPUArchitecture }}
	GPUArchitecture string

	// ConfigArch is the GPU architecture used for config file lookups.
	// GB300 and GB200 use identical configs, so this maps gb300 → gb200.
	// Templates use: {{ .ConfigArch }} in includeTemplate/includeFile paths.
	ConfigArch string

	// EntryName is the variant name of the catalog entry (e.g., "nccl-all-gather").
	// Auto-populated by the loader from the entry's filesystem path.
	// Shared library templates use this to construct entry-specific resource names
	// like {{ .EntryName }}-runtime or {{ .EntryName }}-compute-domain.
	EntryName string

	// SaveInterval is the checkpoint save frequency in training steps.
	// Templates use: {{ .SaveInterval }}
	SaveInterval int32

	// SaveRetainInterval retains checkpoints at multiples of this value (NeMo 6).
	// Templates use: {{ .SaveRetainInterval }}
	SaveRetainInterval int32

	// SaveTopK keeps only the top K checkpoints by metric (NeMo 4).
	// Templates use: {{ .SaveTopK }}
	SaveTopK int32

	// StorageSize is the PVC size for checkpoint storage.
	// Templates use: {{ .StorageSize }}
	StorageSize string

	// TestScale controls orchestration strategy for NCCL tests.
	// Templates use: {{ .TestScale }}
	TestScale string

	// MaxBytes is the max message size for NCCL tests.
	// Templates use: {{ .MaxBytes }}
	MaxBytes string

	// NumIterations is the NCCL -n flag.
	// Templates use: {{ .NumIterations }}
	NumIterations int32

	// NumCycles is the NCCL -N flag.
	// Templates use: {{ .NumCycles }}
	NumCycles int32

	// MaxConcurrent limits simultaneous jobs.
	// Templates use: {{ .MaxConcurrent }}
	MaxConcurrent int32

	// MinGroupSize is the smallest group at which bisection stops.
	// Templates use: {{ .MinGroupSize }}
	MinGroupSize int32

	// TimeoutPerJob is the max duration per job for diagnose mode.
	// Templates use: {{ .TimeoutPerJob }}
	TimeoutPerJob string

	// MeasurementTimeout is the max duration to wait for measurement data after Job success.
	// Templates use: {{ .MeasurementTimeout }}
	MeasurementTimeout string

	// Thresholds maps metric names to CEL expressions.
	// Templates use: {{- range $k, $v := .Thresholds }}
	Thresholds map[string]string

	// TP is tensor-model-parallel size from meta.yaml for the resolved architecture.
	// Templates use: {{ .TP }}
	TP int32

	// PP is pipeline-model-parallel size from meta.yaml for the resolved architecture.
	// Templates use: {{ .PP }}
	PP int32

	// EP is expert-model-parallel size from meta.yaml for the resolved architecture.
	// Templates use: {{ .EP }}
	EP int32
}

func init() {
	if err := loadAndRegisterEntries(); err != nil {
		panic("catalog: " + err.Error())
	}
}

// TemplateFuncs returns the function map available to catalog entry templates
// and to the shared fragments under entries/_lib/.
//
// Anything that renders those fragments must use this map. pkg/platform renders
// _lib fragments too (for WorkloadRun overrides) and previously defined its own
// reduced map containing only lib and indent. Because a template that calls an
// unregistered function fails to parse, and both render paths panic on parse
// failure, a fragment using repeat or trimSuffix rendered fine through the
// catalog and would have crashed the process through pkg/platform. One map
// removes that class of divergence.
//
// indent, trimSuffix and repeat are the only sprig functions the templates use
// (confirmed by grep across pkg/catalog/entries/), which is why this replaces
// the github.com/Masterminds/sprig/v3 dependency rather than pulling it in.
func TemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"indent": func(spaces int, s string) string {
			pad := strings.Repeat(" ", spaces)
			return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"trimSuffix": func(suffix, s string) string {
			return strings.TrimSuffix(s, suffix)
		},
		"repeat": func(count int, s string) string {
			return strings.Repeat(s, count)
		},
		"int": func(v any) int {
			switch n := v.(type) {
			case int:
				return n
			case int32:
				return int(n)
			case int64:
				return int(n)
			case float64:
				return int(n)
			default:
				return 0
			}
		},
		"mul":       func(a, b int) int { return a * b },
		"toYaml":    toYaml,
		"toMpiArgs": toMpiArgs,
	}
}

// toMpiArgs converts a rendered YAML env-format list (- name: VAR / value: V)
// to OpenMPI -x argument format (- -x / - "VAR=VALUE"). Values are always
// double-quoted to handle special characters uniformly.
// Usage: {{ lib "nccl/platform-env.yaml" . | toMpiArgs | indent 12 }}
func toMpiArgs(envYAML string) (string, error) {
	type envEntry struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	var entries []envEntry
	if err := yaml.Unmarshal([]byte(envYAML), &entries); err != nil {
		return "", fmt.Errorf("toMpiArgs: %w", err)
	}
	var lines []string
	for _, e := range entries {
		combined := e.Name + "=" + e.Value
		lines = append(lines, "- -x")
		lines = append(lines, `- "`+strings.ReplaceAll(combined, `"`, `\"`)+`"`)
	}
	return strings.Join(lines, "\n"), nil
}

// toYaml marshals a value to a YAML string, trimming the trailing newline.
func toYaml(v any) string {
	data, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}

// loadAndRegisterEntries discovers all YAML templates under entries/*/*.yaml,
// compiles them at init time, and registers entries whose Build closures
// execute the template with runtime data before unmarshaling.
func loadAndRegisterEntries() error {
	funcs := TemplateFuncs()

	return fs.WalkDir(entriesFS, "entries", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}

		// Only register top-level entry files (entries/{domain}/{variant}.yaml).
		// Skip config data files in subdirectories (e.g., entries/training/nemotron5-8b/configs/train.sh)
		// and shared library files (entries/_lib/).
		rel := strings.TrimPrefix(path, "entries/")
		if strings.HasPrefix(rel, "_lib/") || strings.Count(rel, "/") != 1 {
			return nil
		}

		domain, variant, parseErr := parsePath(path)
		if parseErr != nil {
			return parseErr
		}

		data, readErr := entriesFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}

		// Build per-entry function map with includeFile closure.
		dir := filepath.Dir(path) // e.g., "entries/training"
		entryFuncs := template.FuncMap{}
		maps.Copy(entryFuncs, funcs)
		entryFuncs["includeFile"] = makeIncludeFile(dir, variant)
		entryFuncs["includeTemplate"] = makeIncludeTemplate(dir, variant, entryFuncs)
		entryFuncs["lib"] = makeLibTemplate(entryFuncs)

		// Compile template at init time — fail fast on syntax errors.
		tmpl, tmplErr := template.New(path).Funcs(entryFuncs).Parse(string(data))
		if tmplErr != nil {
			return fmt.Errorf("compiling template %s: %w", path, tmplErr)
		}

		// Validate the template renders with empty data (catches missing keys early).
		var buf bytes.Buffer
		if execErr := tmpl.Execute(&buf, TemplateData{}); execErr != nil {
			return fmt.Errorf("validating template %s: %w", path, execErr)
		}

		// Read optional metadata (e.g., minGPUs) from meta.yaml.
		meta := discoverEntryMeta(dir, variant)

		Register(domain, variant, Entry{
			MinGPUs: meta.MinGPUs,
			MaxValidNodes: func(availableNodes, gpusPerNode int32, gpuArch string) int32 {
				for n := availableNodes; n >= 1; n-- {
					totalGPUs := n * gpusPerNode
					if meta.MinGPUs > 0 && totalGPUs < meta.MinGPUs {
						continue
					}
					tp, pp, ep := meta.getParallel(ConfigArch(gpuArch))
					if tp > 0 && pp > 0 && totalGPUs%(tp*pp) != 0 {
						continue
					}
					// EP requires DP to be divisible by EP.
					if ep > 0 && tp > 0 && pp > 0 {
						dp := totalGPUs / (tp * pp)
						if dp%ep != 0 {
							continue
						}
					}
					return n
				}
				return 0
			},
			Build: func(target crev1alpha1.TargetSpec, config BuildConfig) (crev1alpha1.WorkflowSpec, error) {
				if config.GPUArchitecture == "" {
					return crev1alpha1.WorkflowSpec{}, fmt.Errorf(
						"%s/%s: GPUArchitecture is required but was not provided",
						domain, variant)
				}

				configArch := ConfigArch(config.GPUArchitecture)

				// Validate GPU count against entry constraints.
				if err := validateParallelism(domain, variant, meta, configArch, config.NodesPerJob, config.GpusPerNode); err != nil {
					return crev1alpha1.WorkflowSpec{}, err
				}

				td := buildTemplateData(config, configArch, variant, meta)

				var rendered bytes.Buffer
				if err := tmpl.Execute(&rendered, td); err != nil {
					return crev1alpha1.WorkflowSpec{}, fmt.Errorf("executing template %s/%s: %w", domain, variant, err)
				}

				var spec crev1alpha1.WorkflowSpec
				if err := yaml.Unmarshal(rendered.Bytes(), &spec); err != nil {
					return crev1alpha1.WorkflowSpec{}, fmt.Errorf("parsing rendered %s/%s: %w", domain, variant, err)
				}

				spec.Orchestration.Target = &target

				// Override repeat count (orchestration iterations) if specified.
				if config.RepeatCount > 0 {
					spec.Orchestration.Iterations = int(config.RepeatCount)
				}

				// Override maxRestarts if specified and checkpoint is configured.
				if config.MaxRestarts > 0 && spec.JobTemplate.Spec.Checkpoint != nil {
					restarts := config.MaxRestarts
					spec.JobTemplate.Spec.Checkpoint.MaxRestarts = &restarts
				}

				return spec, nil
			},
		})
		return nil
	})
}

// buildTemplateData creates a TemplateData from BuildConfig, applying defaults for unset fields.
func buildTemplateData(config BuildConfig, configArch, variant string, meta entryMeta) TemplateData {
	td := TemplateData{
		ImagePullSecrets:   config.ImagePullSecrets,
		NodesPerJob:        config.NodesPerJob,
		GpusPerNode:        config.GpusPerNode,
		MlnxPerNode:        config.MlnxPerNode,
		EnableMNNVL:        config.EnableMNNVL,
		EnableCheckpoint:   config.EnableCheckpoint,
		MaxSteps:           config.MaxSteps,
		ExitDurationMins:   config.ExitDurationMins,
		GPUArchitecture:    config.GPUArchitecture,
		ConfigArch:         configArch,
		EntryName:          variant,
		SaveInterval:       config.SaveInterval,
		SaveRetainInterval: config.SaveRetainInterval,
		SaveTopK:           config.SaveTopK,
		TestScale:          config.TestScale,
		MaxBytes:           config.MaxBytes,
		NumIterations:      config.NumIterations,
		NumCycles:          config.NumCycles,
		MaxConcurrent:      config.MaxConcurrent,
		MinGroupSize:       config.MinGroupSize,
		TimeoutPerJob:      config.TimeoutPerJob,
		MeasurementTimeout: config.MeasurementTimeout,
		Thresholds:         config.Thresholds,
	}
	if td.MaxSteps == 0 {
		td.MaxSteps = DefaultMaxSteps
	}
	if td.ExitDurationMins == 0 {
		td.ExitDurationMins = DefaultExitDurationMins
	}
	if td.SaveInterval == 0 {
		td.SaveInterval = DefaultSaveInterval
	}
	if td.SaveRetainInterval == 0 {
		td.SaveRetainInterval = DefaultSaveRetainInterval
	}
	if td.SaveTopK == 0 {
		td.SaveTopK = DefaultSaveTopK
	}
	td.StorageSize = config.StorageSize
	if td.StorageSize == "" {
		td.StorageSize = DefaultStorageSize
	}
	if config.StorageClassName != nil {
		td.StorageClassName = *config.StorageClassName
	}
	if td.TestScale == "" {
		td.TestScale = DefaultTestScale
	}
	// intra-node means each node is tested on its own, which is one node per
	// Job. Partitioning reads the workload's numNodes, so this is the knob that
	// makes the setting do anything; without it the template rendered the full
	// node count and the scale was a no-op.
	if td.TestScale == crev1alpha1.TestScaleIntraNode {
		td.NodesPerJob = 1
	}
	if td.MaxBytes == "" {
		td.MaxBytes = DefaultMaxBytes
	}
	if td.NumIterations == 0 {
		if td.TestScale == crev1alpha1.TestScaleDiagnose {
			td.NumIterations = DiagnoseNumIterations
		} else {
			td.NumIterations = DefaultNumIterations
		}
	}
	if td.NumCycles == 0 {
		if td.TestScale == crev1alpha1.TestScaleDiagnose {
			td.NumCycles = DiagnoseNumCycles
		} else {
			td.NumCycles = DefaultNumCycles
		}
	}
	if td.MinGroupSize == 0 {
		td.MinGroupSize = DefaultMinGroupSize
	}
	if td.TimeoutPerJob == "" {
		if td.TestScale == crev1alpha1.TestScaleDiagnose {
			td.TimeoutPerJob = DiagnoseTimeoutPerJob
		} else {
			td.TimeoutPerJob = DefaultTimeoutPerJob
		}
	}
	tp, pp, ep := meta.getParallel(configArch)
	if tp > 0 {
		td.TP = tp
	}
	if pp > 0 {
		td.PP = pp
	}
	if ep > 0 {
		td.EP = ep
	}
	return td
}

// makeIncludeFile returns a template function that reads files relative to the
// entry's sibling directory. For example, for entry "entries/training/nemotron5-8b.yaml",
// includeFile("configs/gb200_4_node.yaml") reads "entries/training/nemotron5-8b/configs/train.sh".
// Returns empty string if the file doesn't exist — override sections may reference
// files that only exist for certain numNodes values, and those overrides are resolved
// after template rendering. Post-render validation catches truly missing configs.
func makeIncludeFile(dir, variant string) func(string) string {
	return func(relPath string) string {
		fullPath := filepath.Join(dir, variant, relPath)
		data, err := entriesFS.ReadFile(fullPath)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(data), "\n")
	}
}

// makeIncludeTemplate returns a template function that reads files relative to the
// entry's sibling directory and processes the content as a Go template with the
// provided data. This allows config files (e.g., NeMo YAML configs) to use
// template variables like {{ .MaxSteps }} and {{ .EnableCheckpoint }}.
func makeIncludeTemplate(dir, variant string, funcs template.FuncMap) func(string, any) (string, error) {
	return func(relPath string, data any) (string, error) {
		fullPath := filepath.Join(dir, variant, relPath)
		content, err := entriesFS.ReadFile(fullPath)
		if err != nil {
			return "", nil
		}
		tmpl, err := template.New(relPath).Funcs(funcs).Parse(string(content))
		if err != nil {
			return "", fmt.Errorf("parsing %s: %w", fullPath, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return "", fmt.Errorf("executing %s: %w", fullPath, err)
		}
		return strings.TrimSuffix(buf.String(), "\n"), nil
	}
}

// TemplateFuncsWithLib returns TemplateFuncs with the "lib" function wired in,
// for renderers outside this package that pull in entries/_lib/ fragments but
// have no catalog entry directory of their own (see pkg/platform).
//
// lib is registered into the same map it renders with, so a fragment may call
// lib recursively.
func TemplateFuncsWithLib() template.FuncMap {
	funcs := TemplateFuncs()
	funcs["lib"] = makeLibTemplate(funcs)
	return funcs
}

// makeLibTemplate returns a template function that reads shared template fragments
// from entries/_lib/. Unlike includeFile/includeTemplate (which return empty string
// for missing files), lib returns an error — shared library files must exist.
func makeLibTemplate(funcs template.FuncMap) func(string, any) (string, error) {
	return func(relPath string, data any) (string, error) {
		fullPath := filepath.Join("entries/_lib", relPath)
		content, err := entriesFS.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("lib %s: %w", relPath, err)
		}
		tmpl, err := template.New(relPath).Funcs(funcs).Parse(string(content))
		if err != nil {
			return "", fmt.Errorf("parsing lib %s: %w", fullPath, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return "", fmt.Errorf("executing lib %s: %w", fullPath, err)
		}
		return strings.TrimSuffix(buf.String(), "\n"), nil
	}
}

// entryMeta holds optional metadata from a variant's meta.yaml file.
type entryMeta struct {
	MinGPUs     int32                   `yaml:"minGPUs"`
	Parallelism map[string]archParallel `yaml:"parallelism"`
}

// archParallel defines the parallelism config for a GPU architecture.
type archParallel struct {
	TP int32 `yaml:"tp"`
	PP int32 `yaml:"pp"`
	EP int32 `yaml:"ep"`
}

// validateParallelism checks GPU count against min, TP×PP divisibility, and DP%EP.
func validateParallelism(domain, variant string, meta entryMeta, arch string, nodesPerJob, gpusPerNode int32) error {
	if nodesPerJob <= 0 || gpusPerNode <= 0 {
		return nil
	}
	totalGPUs := nodesPerJob * gpusPerNode

	if meta.MinGPUs > 0 && totalGPUs < meta.MinGPUs {
		minNodes := (meta.MinGPUs + gpusPerNode - 1) / gpusPerNode
		return fmt.Errorf(
			"%s/%s: requires at least %d GPUs (%d nodes × %d GPUs/node), got %d GPUs (%d nodes); set nodesPerJob to %d or higher",
			domain, variant, meta.MinGPUs, minNodes, gpusPerNode, totalGPUs, nodesPerJob, minNodes)
	}

	tp, pp, ep := meta.getParallel(arch)
	if tp <= 0 || pp <= 0 {
		return nil
	}
	tppp := tp * pp
	if totalGPUs%tppp != 0 {
		return fmt.Errorf(
			"%s/%s: totalGPUs=%d (%d nodes × %d GPUs/node) must be divisible by TP×PP=%d (TP=%d, PP=%d); try nodesPerJob=%d or %d",
			domain, variant, totalGPUs, nodesPerJob, gpusPerNode, tppp, tp, pp,
			maxNodesBelow(totalGPUs, gpusPerNode, tppp),
			minNodesAbove(totalGPUs, gpusPerNode, tppp))
	}
	if ep > 0 {
		dp := totalGPUs / tppp
		if dp%ep != 0 {
			required := tppp * ep
			return fmt.Errorf(
				"%s/%s: totalGPUs=%d, DP=%d must be divisible by EP=%d; totalGPUs must be a multiple of %d (TP×PP×EP=%d×%d×%d)",
				domain, variant, totalGPUs, dp, ep, required, tp, pp, ep)
		}
	}
	return nil
}

// getParallel returns TP, PP, and EP for the given architecture, falling back
// to the "default" key if the arch isn't explicitly listed. Returns (0,0,0) if
// no parallelism metadata exists.
func (m entryMeta) getParallel(arch string) (tp, pp, ep int32) {
	if m.Parallelism == nil {
		return 0, 0, 0
	}
	if p, ok := m.Parallelism[arch]; ok {
		return p.TP, p.PP, p.EP
	}
	if p, ok := m.Parallelism["default"]; ok {
		return p.TP, p.PP, p.EP
	}
	return 0, 0, 0
}

// maxNodesBelow returns the largest node count <= current where totalGPUs is divisible by tppp.
func maxNodesBelow(totalGPUs, gpusPerNode, tppp int32) int32 {
	for n := totalGPUs / gpusPerNode; n > 0; n-- {
		if (n*gpusPerNode)%tppp == 0 {
			return n
		}
	}
	return 0
}

// minNodesAbove returns the smallest node count >= current where totalGPUs is divisible by tppp.
func minNodesAbove(totalGPUs, gpusPerNode, tppp int32) int32 {
	for n := totalGPUs/gpusPerNode + 1; n <= totalGPUs; n++ {
		if (n*gpusPerNode)%tppp == 0 {
			return n
		}
	}
	return 0
}

// discoverEntryMeta reads meta.yaml from entries/{domain}/{variant}/ if it exists.
func discoverEntryMeta(dir, variant string) entryMeta {
	data, err := entriesFS.ReadFile(filepath.Join(dir, variant, "meta.yaml"))
	if err != nil {
		return entryMeta{}
	}
	var meta entryMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return entryMeta{}
	}
	return meta
}

// parsePath extracts domain and variant from "entries/{domain}/{variant}.yaml".
func parsePath(path string) (domain, variant string, err error) {
	rel := strings.TrimPrefix(path, "entries/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected path format: %s", path)
	}
	domain = parts[0]
	variant = strings.TrimSuffix(parts[1], ".yaml")
	if domain == "" || variant == "" {
		return "", "", fmt.Errorf("empty domain or variant in path: %s", path)
	}
	return domain, variant, nil
}
