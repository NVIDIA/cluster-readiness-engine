# ADR-010: Architecture — Certification Catalog with init() Registration

## Context

Certification runs multiple benchmark categories — NCCL allreduce, Nemotron pre-training, storage stress, etc. Each category has a pre-configured Workflow specification (workload type, iteration count, health checks, resource requirements). These configurations are complex and hardware-specific.

The question is how to manage the mapping from `{domain, variant}` to pre-configured Workflow specs. This mapping must be extensible (new categories added regularly), but each entry is a complex Go struct (not simple YAML).

Options considered:
1. YAML/JSON config files loaded at startup
2. CRD-based catalog (catalog entries as Kubernetes resources)
3. Go package with `init()` registration pattern

## Decision

Implement the catalog as a Go package (`pkg/catalog/`) with a global registry populated by `init()` functions. Each certification category is a separate Go file that registers itself at import time. The Certification controller calls `catalog.Lookup(domain, variant)` to retrieve pre-configured specs.

## Implementation

- **Registry** (`pkg/catalog/catalog.go`): Package-level `map[categoryKey]Entry` where `categoryKey` is `{Domain, Variant}`. `Register()` adds entries. `Lookup()` retrieves them.
- **Entry** (`pkg/catalog/catalog.go`): `Build func(target TargetSpec) WorkflowSpec` — a function that takes the target node specification and returns a complete Workflow spec. This allows the spec to reference target-specific values (node count, topology).
- **Category files**: Each category is a separate file (e.g., `training_nemotron.go`) with an `init()` function that calls `Register()`.
- **Blank imports**: `cmd/manager/main.go` and `pkg/controller/suite_test.go` must import the catalog package with `_` to trigger `init()` registration.

Example category file:
```go
// pkg/catalog/training_nemotron.go
func init() {
    Register("training", "nemotron", Entry{
        Build: func(target nvcrev1alpha1.TargetSpec) nvcrev1alpha1.WorkflowSpec {
            return nvcrev1alpha1.WorkflowSpec{
                JobTemplate: nvcrev1alpha1.JobTemplateSpec{
                    Spec: nvcrev1alpha1.JobSpec{
                        Workload: &nvcrev1alpha1.WorkloadSpec{
                            TrainJob: &trainerv1alpha1.TrainJob{...},
                        },
                    },
                },
            }
        },
    })
}
```

Adding a new category requires only a new Go file — no changes to the controller, no CRD updates, no configuration files.

## Rationale

- **Type safety.** Catalog entries are Go structs with compile-time type checking. A malformed Workflow spec is caught at build time, not at runtime when a Certification tries to use it.
- **Self-registering.** The `init()` pattern means the catalog is always consistent with the compiled binary. No configuration drift between a config file and the code.
- **One file per category.** New categories don't touch existing code. No merge conflicts when multiple people add categories simultaneously.
- **Build function, not static spec.** The `Build(target)` function can customize the spec based on the target (e.g., different GPU counts, different tolerations). This is more flexible than static YAML.

## Consequences

### Positive
- Adding a category is a single-file change with no impact on existing code
- Compile-time validation of all catalog entries
- The Build function can generate specs dynamically based on target configuration
- Standard Go testing works for catalog entries (`pkg/catalog/catalog_test.go`)

### Negative
- Adding a category requires a Go code change and rebuild (not runtime-configurable)
- Blank imports in `main.go` and `suite_test.go` are easy to forget (causes "category not found" at runtime)
- The global registry pattern is not safe for concurrent registration (acceptable since `init()` runs sequentially)

### Mitigations
- The blank import requirement is documented as a common pitfall
- Catalog tests verify expected entries are registered
- For runtime-configurable categories, users can create Workflows directly (bypassing the catalog)

## Alternatives Considered

### YAML/JSON config files
**Rejected** because: Workflow specs contain Go types (intstr.IntOrString, resource.Quantity, metav1.Condition) that don't serialize cleanly to YAML. Complex specs with nested structs, pointer fields, and tagged unions would require custom deserialization logic — reintroducing the runtime parsing problems that the typed adapter pattern (ADR-003) eliminated.

### CRD-based catalog
**Rejected** because: Catalog entries are operational configuration, not user-facing resources. They shouldn't be visible in `kubectl get` or editable by cluster users. A CRD-based catalog would need RBAC to prevent modification, versioning to prevent drift, and a controller to watch for changes — all for something that is fundamentally compiled-in configuration.

### Hard-coded in Certification controller
**Rejected** because: The Certification controller would grow with every new category. The catalog package keeps the controller focused on orchestration and the category definitions in separate files.

## Notes

- `Trainer.NumProcPerNode` is `*intstr.IntOrString` — use `intstr.FromInt32()` in catalog entries
- The catalog currently has entries under `training/`, `communication/`, `diagnostics/`, and `stress/` domains

## References

- `pkg/catalog/catalog.go` — registry and types
- `pkg/catalog/training_nemotron.go` — example category
- `pkg/catalog/catalog_test.go` — standard Go tests
