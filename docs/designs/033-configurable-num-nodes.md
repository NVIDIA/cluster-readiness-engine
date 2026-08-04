# ADR-033: Configurable Node Count and GPUs Per Node

## Context

The catalog currently hardcodes node counts inside each entry. Training entries require different NeMo configurations depending on both the **GPU architecture** (GB200 vs H100) and the **number of nodes** -- different node counts change `global_batch_size`, `num_train_samples`, parallelism settings (`tensor_model_parallel_size`, `sequence_parallel`), and `devices` per node.

This creates several problems:

1. **Variant proliferation.** GB300 variants were near-complete copies of their base entries, differing only in node count, MNNVL setting, and topology-aware scheduling. Adding more node-count variants (e.g., 16-node, 32-node) would mean more copy-paste entries.

2. **NCCL tests have the same problem.** The `nccl-all-reduce` entry hardcodes `trainer.numNodes: 8`, `-np 32` (base) and `trainer.numNodes: 2`, `-np 16` (AWS override). Different cluster sizes require editing the catalog source.

3. **No user control over scale.** Operators cannot choose how many nodes to burn in without modifying catalog YAML. A Certification should declare *what* to test and *at what scale*, with the catalog adapting to the requested scale.

## Decision

1. **Add `numNodes` (required, `int32`, minimum 1) to `CertificationSpec`.** This is the single source of truth for multi-node scale. The certification controller passes it to `catalog.BuildConfig`, and catalog templates render it at Build time.

2. **Add `gpusPerNode` (optional, `*int32`, minimum 1) to `CertificationSpec`.** When unspecified, the certification controller derives the default from the GPU product label in `target.nodeSelector` (e.g., `NVIDIA-H100-80GB-HBM3` -> `h100` -> 8 GPUs, `NVIDIA-GB200-NVL72` -> `gb200` -> 4 GPUs). This reuses the same label-parsing logic as `nodeGPUArchitecture()` in `workflow_detect.go`. When explicitly specified, the user's value overrides the derived default. The resolved value is passed to `BuildConfig.GpusPerNode` (always non-zero), so templates use `{{ .GpusPerNode }}` directly with no `default` needed.

3. **Add `enableMNNVL` (optional, `bool`, default false) to `CertificationSpec`.** Controls the `NCCL_MNNVL_ENABLE` env var across training entries. Defaults to disabled (`0`); operators opt in via `enableMNNVL: true` for platforms with multi-node NVLink connectivity.

4. **Add `includeFile` template function to the catalog loader.** Training entries that need per-{gpu_arch, node_count} NeMo configurations store them as separate files under `entries/{domain}/{variant}/configs/`. The main template uses `{{ includeFile (printf "configs/%s_%d_node.yaml" arch .NumNodes) | indent N }}` to load the correct config into the ConfigMap dependency at render time.

5. **Templatize `numNodes` and `gpusPerNode` across all multi-node categories.** Training and communication entries replace hardcoded values with `{{ .NumNodes }}` and `{{ .GpusPerNode }}` (always resolved, never zero). MPI-based entries (NCCL) compute total processes via `{{ mul .NumNodes .GpusPerNode }}`. Single-node categories (diagnostics, stress) keep `numNodes: 1` -- they always test one node per job; the orchestrator handles multi-node distribution.

6. **Eliminate GB300-specific variants.** GB300 variants are consolidated into their base entries. GB300-specific 8-node configs become config data files (`gb200_8_node.yaml`). Topology-aware scheduling moves to the GB200 override. PVC dependencies are dropped (emptyDir suffices for burn-in's short runs). MNNVL defaults to disabled (`NCCL_MNNVL_ENABLE=0`) in all base configs.

7. **Expand the embed directive** from `entries/*/*.yaml` to `entries` (whole tree). The WalkDir callback skips subdirectory files (path depth > 2 after `entries/`) so config data files are embedded but not registered as catalog entries.

## Implementation

### API changes (`api/v1alpha1/certification_types.go`)

```go
type CertificationSpec struct {
    Target           TargetSpec                    `json:"target"`
    Categories       []CertificateCategory         `json:"categories,omitempty"`
    NumNodes         int32                         `json:"numNodes"`
    GpusPerNode      *int32                        `json:"gpusPerNode,omitempty"`
    EnableMNNVL      bool                          `json:"enableMNNVL,omitempty"`
    ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
    StorageClassName *string                       `json:"storageClassName,omitempty"`
}
```

### Controller changes (`pkg/controller/certification_controller.go`)

The `createWorkflowForCategory` method resolves `gpusPerNode` before calling Build:
1. Parse `target.nodeSelector["nvidia.com/gpu.product"]` using the same logic as `nodeGPUArchitecture()` (strip "NVIDIA-" prefix, extract model family, lowercase)
2. Map architecture to default GPUs: `h100/a100/l40s -> 8`, `gb200/gb300 -> 4`
3. If `spec.gpusPerNode` is specified, use it instead
4. Pass the resolved value as `BuildConfig.GpusPerNode` (always non-zero)

### Catalog changes (`pkg/catalog/`)

`BuildConfig` and `TemplateData` gain `NumNodes int32`, `GpusPerNode int32`, and `EnableMNNVL bool`. The loader creates a per-entry `includeFile` closure that reads from the entry's sibling directory (`entries/{domain}/{variant}/configs/`). Missing files return empty string (safe for init-time validation with `NumNodes=0`; actual Build calls use the user-specified value).

### Config data file layout

```
entries/training/
  nemotron5-8b.yaml
  nemotron5-8b/configs/
    gb200_4_node.yaml    # GB200 4-node NeMo config
    gb200_8_node.yaml    # GB200 8-node NeMo config
    h100_2_node.yaml     # H100 2-node NeMo config
  nemotron5-56b.yaml
  nemotron5-56b/configs/
    gb200_32_node.yaml   # GB200 32-node NeMo config
```

### Template patterns

**Training (ConfigMap via includeFile):**
```yaml
- apiVersion: v1
  data:
    config.yaml: |
{{ includeFile (printf "configs/gb200_%d_node.yaml" .NumNodes) | indent 6 }}
  kind: ConfigMap
```

**Training (numProcPerNode):**
```yaml
mlPolicy:
  numNodes: {{ .NumNodes }}
  torch:
    numProcPerNode: {{ .GpusPerNode }}
```

**MNNVL env var:**
```yaml
env:
- name: NCCL_MNNVL_ENABLE
  value: "{{ if .EnableMNNVL }}1{{ else }}0{{ end }}"
```

**NCCL (MPI policy):**
```yaml
mlPolicy:
  mpi:
    numProcPerNode: {{ .GpusPerNode }}
```

**NCCL (computed -np arg and trainer):**
```yaml
trainer:
  args:
  - -np
  - "{{ mul .NumNodes .GpusPerNode }}"
  - -N
  - "{{ .GpusPerNode }}"
  numNodes: {{ .NumNodes }}
  numProcPerNode: {{ .GpusPerNode }}
```

## Rationale

- **`CertificationSpec` is the right level** -- `numNodes` is a deployment-level choice, not a workload detail. Different clusters may burn in with different node counts for the same model.
- **Required `numNodes`, no default** -- there is no universally correct default. Forcing operators to declare the scale prevents accidental under-testing (1 node when 8 was intended) or over-allocation.
- **Optional `gpusPerNode` with smart default** -- the GPU product label in `target.nodeSelector` already identifies the architecture. Deriving gpusPerNode from it (h100->8, gb200->4) covers 99% of cases. The optional override handles non-standard hardware.
- **Controller-resolved `gpusPerNode`** -- resolving at the controller (not in templates) centralizes the architecture-to-GPU mapping in Go code. Templates use `{{ .GpusPerNode }}` directly, avoiding scattered `{{ default N .GpusPerNode }}` calls.
- **`includeFile` over inline templating** -- NeMo configs are 200+ line YAML files with many fields that vary by {arch, node_count}. Extracting them to files keeps the main template readable and makes configs independently reviewable.
- **`includeFile` over per-node-count overrides** -- using the override system would require one override per node count, each with a `when.numNodes` matcher (which doesn't exist in the API). Overrides are designed for runtime-detected properties (platform, GPU arch), not user-specified parameters.
- **Single-node categories ignore `numNodes`** -- diagnostics and stress tests are inherently single-node-per-job. The orchestrator creates one job per target node regardless. Forcing them to acknowledge `numNodes` would add complexity with no benefit.
- **GB300 topology in GB200 override** -- the recently added `OrchestrationOverrideSpec.Topology` (ADR-032) enables the GB200 override to set `topologyKey: nvidia.com/gpu.clique`, eliminating the need for a separate gb300 variant.
- **Init-time config discovery for numNodes validation** -- at init, the loader scans each entry's `configs/` directory and parses supported numNodes values from filenames matching `{arch}_{numNodes}_node.yaml` (e.g., `gb200_4_node.yaml` -> 4). At Build time, if the entry has a configs directory and the requested numNodes is not in the discovered set, Build returns an error immediately -- before template rendering. This catches unsupported numNodes at Certification creation rather than at pod runtime. Entries without a configs directory (nemotron6-8b, NCCL tests) accept any numNodes since their templates don't use `includeFile`. The Build signature is `(WorkflowSpec, error)` so callers handle failures explicitly.
- **Failed status on validation errors** -- the certification controller surfaces Build errors as a `Failed` condition with reason `WorkflowValidationFailed` on the Certification status. This makes unsupported numNodes visible via `kubectl describe certification` instead of requiring users to inspect controller logs or debug failing pods.

## Consequences

### Positive
- Eliminates duplicated GB300-specific variants (~431 lines of duplicated YAML)
- Operators control test scale from the Certification spec without modifying the catalog
- Adding a new node-count configuration = adding one config data file (e.g., `gb200_16_node.yaml`)
- NeMo config data is independently reviewable and diffable
- NCCL test node counts configurable -- no more editing catalog for different cluster sizes

### Negative
- `numNodes` is required even for Certifications that only test single-node categories (diagnostics/stress). The field is ignored but must be specified.
- Config data files must exist for the specified `numNodes` x GPU architecture combination. Invalid combinations are caught at Build time (the controller sets a `Failed` condition with reason `WorkflowValidationFailed`), but adding new node counts still requires creating a config data file.
- Communication entries use `{{ mul .NumNodes .GpusPerNode }}` in string args, which is less readable than literal values.
- The architecture-to-GPU mapping in `defaultGpusPerNode()` must be maintained when new GPU families are introduced.

## Alternatives Considered

### Per-category numNodes on CertificateCategory
**Rejected** because: Different categories in the same Certification should test the same nodes at the same scale. A per-category numNodes would create confusion when NCCL uses 2 nodes but training uses 8 in the same cert run.

### Optional numNodes with per-variant defaults
**Rejected** because: No universally correct default exists. GB200 might default to 4 but a 16-node cluster should test at 16. Making it optional invites silent misconfiguration.

### Compute GBS/num_train_samples via template math instead of separate files
**Rejected** because: The relationship between numNodes and batch parameters is not a simple formula -- it varies by GPU architecture and model. GB200 4-node uses GBS=64, GB200 16-node uses GBS=256, but 8-node also uses GBS=64. Separate files are explicit and correct.

### Store configs in ConfigMaps on the cluster instead of embedded files
**Rejected** because: This would require a separate deployment step to create ConfigMaps before running Certifications. The embedded catalog is self-contained by design (ADR-024).

## References

- ADR-010: Certification Catalog
- ADR-023: Catalog Configurability (BuildConfig)
- ADR-024: YAML-Embedded Catalog
- ADR-025: YAML Template Catalog (TemplateData, Sprig, `toYaml`)
- ADR-032: Orchestration Overrides (OrchestrationOverrideSpec.Topology)
