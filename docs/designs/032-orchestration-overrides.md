# ADR-032: Orchestration Overrides

## Context

`OverrideSpec` currently supports `jobTemplate`, `jobTemplatePatch`, and `dependencies` — but not `orchestration`. This forces catalog entries to create separate variants when only the orchestration differs (e.g., simple vs topology-aware, or different iteration counts per GPU architecture).

For example, GB300 NVL72 racks require topology-aware placement with `nvidia.com/gpu.clique` as the topology key, while H100 clusters use simple partitioning. Today this requires two catalog variants that are identical except for the orchestration section. The override mechanism (ADR-012) should handle this, just as it already handles jobTemplate and dependency differences.

## Decision

Add an `Orchestration *OrchestrationOverrideSpec` field to `OverrideSpec`. The `OrchestrationOverrideSpec` mirrors `OrchestrationSpec` but with all-pointer fields so omitted fields mean "don't change." A new `mergeOrchestration()` function applies non-nil override fields onto the base `OrchestrationSpec`.

## Implementation

### New type: `OrchestrationOverrideSpec`

Added to `api/v1alpha1/workflow_types.go`:

```go
type OrchestrationOverrideSpec struct {
    Target        *TargetSpec        `json:"target,omitempty"`
    Topology      *TopologySpec      `json:"topology,omitempty"`
    Combinatorial *CombinatorialSpec `json:"combinatorial,omitempty"`
    Bisection     *BisectionSpec     `json:"bisection,omitempty"`
    Execution     *ExecutionSpec     `json:"execution,omitempty"`
    Iterations    *int               `json:"iterations,omitempty"`
}
```

All fields are optional pointers. When a field is nil, the base value is preserved. When non-nil, it replaces the base value entirely (whole-field replacement, not recursive merge). This matches the semantics of the existing `jobTemplate` strategic merge — simple, predictable, no surprising partial merges of nested structs.

### New field on `OverrideSpec`

```go
type OverrideSpec struct {
    When            WhenSpec                  `json:"when"`
    JobTemplate     *apiextensionsv1.JSON     `json:"jobTemplate,omitempty"`
    JobTemplatePatch *apiextensionsv1.JSON    `json:"jobTemplatePatch,omitempty"`
    Dependencies    []DependencySpec          `json:"dependencies,omitempty"`
    Orchestration   *OrchestrationOverrideSpec `json:"orchestration,omitempty"`  // NEW
}
```

### Merge logic in `workflow_detect.go`

New `mergeOrchestration(base *OrchestrationSpec, override *OrchestrationOverrideSpec)` function that applies non-nil override fields to the base orchestration spec. Wired into both `applyOverrides()` and `applyOverridesWithTracking()` after the dependency merge step. `summarizePatches()` updated to include `"orchestration"` when the override has an orchestration field.

### Files modified

- `api/v1alpha1/workflow_types.go` — new `OrchestrationOverrideSpec` type, new field on `OverrideSpec`
- `pkg/controller/workflow_detect.go` — `mergeOrchestration()`, wired into apply functions, `summarizePatches()` updated
- `pkg/controller/testdata/apply-overrides/orchestration-topology-override/` — test case: override adds topology to a base with no topology
- `pkg/controller/testdata/apply-overrides/orchestration-iterations-override/` — test case: override changes iterations count
- CRD manifests regenerated via `make manifests generate`

## Rationale

- **Whole-field replacement**: `OrchestrationOverrideSpec` uses pointer fields with whole-field replacement semantics (non-nil replaces, nil preserves). This is simpler than recursive merge and matches how users think about orchestration — you either want topology-aware or you don't. Partial merging of `TopologySpec` or `ExecutionSpec` would add complexity for minimal benefit.
- **Separate type from `OrchestrationSpec`**: Using a distinct `OrchestrationOverrideSpec` with pointer fields makes it unambiguous which fields are being overridden. Reusing `OrchestrationSpec` directly would require defaulting logic to distinguish "user set iterations=1" from "user didn't set iterations."
- **Execution field is `*ExecutionSpec`**: Even though `ExecutionSpec` has defaults (maxConcurrent=0), the override uses a pointer so omitting it means "keep the base." This avoids accidentally resetting execution settings to zero values.

## Consequences

**Positive:**
- Catalog entries can use a single variant with orchestration overrides for platform/GPU-specific placement strategies
- Reduces catalog duplication when only orchestration differs between GPU architectures
- Consistent with existing override patterns — `when` conditions, `status.orchestration.appliedOverrides` tracking

**Negative:**
- One more field to consider when writing overrides (minor — it's optional)
- Whole-field replacement means you can't partially update `ExecutionSpec` (e.g., change only `timeoutPerJob` while keeping `maxConcurrent`) — you must specify the entire `ExecutionSpec`. This is acceptable because orchestration sub-structs are small.

## Alternatives Considered

1. **Reuse `OrchestrationSpec` directly on `OverrideSpec`**: Simpler but ambiguous — can't distinguish "user set iterations=1" from default. Would require sentinel values or separate "which fields are set" tracking.
2. **Strategic merge patch for orchestration**: Apply a JSON merge patch like `jobTemplate` does. Rejected because orchestration fields are strongly typed Go structs (not `runtime.RawExtension`), and the field-level pointer approach is more type-safe and readable.
3. **Do nothing — keep separate catalog variants**: Works but leads to combinatorial explosion as more GPU architectures and platforms are added.

## References

- ADR-012: Platform/GPU Architecture Overrides
- ADR-007: Topology-Aware Orchestration
- ADR-027: Kustomize-like Override UX
