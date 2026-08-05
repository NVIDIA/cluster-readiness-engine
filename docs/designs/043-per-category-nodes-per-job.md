# ADR-043: Per-Category nodesPerJob with Auto-Selection and Early Overlay Resolution

## Context

ADR-033 introduced a global `nodesPerJob` on `CertificationSpec` as the single source of truth for multi-node scale. This works well for training categories that need a specific node count matching their config files, but creates problems for other categories:

1. **Communication tests need all nodes.** NCCL all-reduce, send-recv, and bandwidth tests should exercise the full fabric — running on a subset misses real-world failure modes. A global `nodesPerJob: 4` means a 32-node cluster only tests 4-node communication.

2. **Different categories need different scales.** Training models have per-node-count config files (batch size, parallelism) that only work for specific node counts. Communication tests want all nodes. Diagnostics and stress tests are single-node-per-job. A single global value cannot serve all three.

3. **Operators must know valid node counts.** `nodesPerJob` is required and has no default. Operators must consult the catalog's config files to find valid values for training entries, adding friction.

4. **Overlay resolution happens late.** The WorkflowController applies overlays during `discoverAndPartition()`, meaning Workflow CRs carry all unresolved overlays until reconciliation. For entries with many platform/GPU overlays (e.g., `nccl-all-reduce` with ~150 lines), this inflates CR size unnecessarily.

ADR-033 explicitly rejected per-category `nodesPerJob` because "different categories in the same Certification should test the same nodes at the same scale." This ADR reverses that decision — the requirement has evolved: communication tests need all-node coverage, training tests need model-specific configs, and single-node tests ignore the value entirely.

## Decision

### 1. Move nodesPerJob to CategoryOptions as optional pointer

`nodesPerJob` moves from a required `int32` on `CertificationSpec` to an optional `*int32` on `CategoryOptions`. When nil at both global and per-category level, the controller auto-selects:

- **Entries with per-node-count config files** (training): pick the largest supported config <= matching node count.
- **All other entries** (communication, diagnostics, stress): use all matching nodes.

When explicitly set: clamped to `min(specified, matchingNodes)`.

### 2. Expand CategoryOptions as the unified configuration struct

`CategoryOptions` becomes the single struct for all configuration knobs, used at two levels:

- **Embedded inline in `CertificationSpec`** — global defaults (JSON layout unchanged for backward compatibility)
- **As `Options *CategoryOptions` in `CertificateCategory`** — per-category overrides

All fields that were individual on `CertificationSpec` (`enableCheckpoint`, `maxSteps`, `exitDurationMins`, `gpusPerNode`, `enableMNNVL`, `imagePullSecrets`, `storageClassName`) move into `CategoryOptions`. Per-category values take precedence over globals; nil means "use global."

### 3. Move overlay resolution to CertificationController

The CertificationController resolves overlays after template rendering:

1. Discover target nodes (shared context for nodesPerJob + overlays)
2. Detect platform and GPU architecture (best-effort, first-node heuristic)
3. Build OverrideContext from detected values + rendered WorkflowSpec
4. Apply matching overlays and **prune** them from `spec.Overrides`
5. Create Workflow with resolved spec — only unresolved overlays remain

The WorkflowController's overlay logic is unchanged — it naturally processes whatever overlays remain in the spec (typically none for the Certification path). This reduces Workflow CR size and ensures dependencies are rendered once with correct values.

### 4. Expose catalog SupportedNodeCounts

`catalog.Entry` gains a `SupportedNodeCounts []int32` field, populated at init from the `configs/` directory scan that already exists in the loader. A `LookupSupportedNodeCounts(domain, variant)` helper enables the controller to check whether an entry has per-node-count configs.

## Implementation

### API changes (`api/v1alpha1/certification_types.go`)

```go
type CategoryOptions struct {
    NodesPerJob      *int32                        `json:"nodesPerJob,omitempty"`
    EnableCheckpoint *bool                         `json:"enableCheckpoint,omitempty"`
    MaxSteps         *int32                        `json:"maxSteps,omitempty"`
    ExitDurationMins *int32                        `json:"exitDurationMins,omitempty"`
    GpusPerNode      *int32                        `json:"gpusPerNode,omitempty"`
    EnableMNNVL      *bool                         `json:"enableMNNVL,omitempty"`
    ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
    StorageClassName *string                       `json:"storageClassName,omitempty"`
}

type CertificationSpec struct {
    Target     TargetSpec            `json:"target"`
    Categories []CertificateCategory `json:"categories,omitempty"`
    CategoryOptions `json:",inline"` // Global defaults
}
```

The `json:",inline"` embedding preserves the existing JSON layout — `nodesPerJob`, `enableCheckpoint`, etc. still appear directly under `spec:`.

### Catalog changes (`pkg/catalog/catalog.go`, `pkg/catalog/loader.go`)

`Entry` gains `SupportedNodeCounts []int32` (sorted ascending, nil = any count accepted). Populated during registration from `sortedKeys(supportedNodes)` which already exists. `LookupSupportedNodeCounts()` added as a public helper.

### CertificationController changes (`pkg/controller/certification_controller.go`)

New helpers:
- `resolveOptions(global, override *CategoryOptions) CategoryOptions` — merge global + per-category
- `resolveNodesPerJob(nodes, category, opts) (int32, error)` — auto-select or explicit
- `pruneAppliedOverrides(spec, applied)` — remove applied overlays from spec
- `derefBool(b *bool) bool` — nil-safe dereference

`createWorkflowForCategory()` refactored for full resolution: discover nodes → detect platform/GPU → resolve options → render templates → apply overlays → prune → create Workflow.

### WorkflowController changes (`pkg/controller/workflow_controller.go`)

- `discoverAndPartition()` nodesPerJob clamping: `< 1` → all nodes, `> available` → cap to available
- `createJobForGroup()`: always call `adapter.SetNumNodes()` (not just bisection mode)
- Overlay logic unchanged — naturally handles remaining unresolved overlays

### ncrectl changes (`pkg/certification/certification.go`)

- `render` command: full resolution (same as CertificationController) — resolve options, nodesPerJob, templates, overlays
- `run` command: `--nodes-per-job` default changes from 2 to 0 (auto-select); when 0, leave nil on spec

## Rationale

- **Per-category nodesPerJob reverses ADR-033** — the original reasoning ("same scale across categories") doesn't hold when communication tests need all nodes and training tests need specific configs. The scale is inherently category-dependent.
- **Auto-selection reduces operator burden** — operators no longer need to know which node counts have config files. The controller picks the best fit automatically.
- **CategoryOptions as shared struct** — avoids field duplication between global and per-category levels. The `json:",inline"` embedding preserves backward-compatible JSON layout.
- **Pointer types for optional fields** — `*bool` and `*int32` distinguish "not set" (nil, use global) from "explicitly set to zero/false." This is essential for the override semantics.
- **Early overlay resolution** — resolving overlays in CertificationController (1) reduces Workflow CR size, (2) ensures dependencies have correct values when created, and (3) gives a single resolution point for the Certification path.
- **Best-effort platform detection** — CertificationController uses `DetectPlatform(nodes)` (first-node heuristic) rather than the strict `detectPlatformConsistent()`. If detection fails, overlays are left for WorkflowController. This avoids failing the Certification on detection edge cases.
- **WorkflowController as safety net** — remaining overlays and nodesPerJob clamping in WorkflowController handle Workflows not created via the Certification path (kubectl apply, ncrectl).

## Consequences

### Positive
- Communication tests automatically use all matching nodes
- Training auto-selects optimal config — no manual lookup needed
- Per-category overrides for all options (enableMNNVL, gpusPerNode, etc.)
- Smaller Workflow CRs (overlays pruned after resolution)
- Single resolution path — dependencies created once with correct values
- `nodesPerJob` no longer required for Certifications that only test single-node categories

### Negative
- Breaking v1alpha1 API change: `nodesPerJob` type changes from required `int32` to optional `*int32`. All existing samples and test inputs must be updated.
- Bool fields change from `bool` to `*bool` — callers must handle nil checks (via `derefBool` helper).
- CertificationController takes on more responsibility (node discovery, overlay resolution).
- Auto-selection adds a node discovery call per Workflow creation in the Certification path.

## Alternatives Considered

### Keep nodesPerJob global, add separate allNodes flag for communication
**Rejected** because: adds a special case for one category type. The per-category approach is more general and handles future categories that may need different node counts.

### Auto-select only, no explicit nodesPerJob option
**Rejected** because: operators may need to test at a specific scale (e.g., 4-node training on a 32-node cluster). The explicit override is necessary for controlled testing.

### Keep overlay resolution in WorkflowController only
**Rejected** because: Workflow CRs carry all unresolved overlays, inflating size. Dependencies are created from the unresolved spec, requiring a safety-net patch. Moving resolution earlier is cleaner.

## References

- ADR-010: Certification Catalog
- ADR-012: Platform/GPU Overrides
- ADR-023: Catalog Configurability (BuildConfig)
- ADR-025: YAML Template Catalog (TemplateData)
- ADR-032: Orchestration Overrides
- ADR-033: Configurable Node Count and GPUs Per Node (superseded by this ADR)
