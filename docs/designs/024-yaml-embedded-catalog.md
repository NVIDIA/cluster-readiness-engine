# ADR-024: YAML-Embedded Catalog — Replace Go Struct Literals with Embedded YAML Files

## Context

Catalog entries are currently Go files that construct `WorkflowSpec` using Go struct literals and `map[string]any` payloads marshaled through `mustMarshalJSON()`. Despite being ~4,000 lines of Go code across 10 files, the actual Go logic is minimal:

- **Target injection**: `spec.Orchestration.Target = &target` (all 14 entries)
- **ImagePullSecrets**: `config.ImagePullSecrets` passed to `PodTemplateSpecOverride` (4 entries)
- **StorageClassName**: `config.StorageClassName` conditionally added to PVC dependencies (2 entries)

Everything else is static data — Kubernetes resource specs, shell scripts, NeMo training configs, environment variables, dependency definitions. These are effectively YAML documents awkwardly encoded as Go struct literals and `map[string]any` maps.

The Go encoding makes catalog entries:
- Hard to read for non-Go engineers (verbose struct syntax for what is standard K8s YAML)
- Error-prone (Go compiler can't validate K8s resource structure inside `map[string]any`)
- Unnecessarily complex (loop-based registration for simple variant lists)
- Resistant to external tooling (linters, formatters, schema validators can't process Go maps as K8s resources)

## Decision

Replace all 10 Go catalog files with 14 embedded YAML files (one per catalog entry) and a generic Go loader that:

1. Embeds YAML files via `//go:embed` at compile time
2. Parses them into `WorkflowSpec` at init time (fail-fast on malformed data)
3. Applies runtime configuration (target, imagePullSecrets, storageClassName) via generic post-processing functions at Build time

## Implementation

### Directory structure

```
pkg/catalog/
  catalog.go          # Registry types (unchanged API)
  loader.go           # YAML loader + post-processing (new)
  entries/            # Embedded YAML files (new)
    training/
      hello-world.yaml
      nemotron5-8b.yaml
      nemotron5-56b.yaml
    diagnostics/
      dcgm-level4.yaml
    communication/
      nccl-all-reduce.yaml
      nccl-all-gather.yaml
      nccl-alltoall.yaml
    stress/
      gpu-burn.yaml
      gpu-power-impulse.yaml
```

### YAML file format

Each YAML file contains a bare `WorkflowSpec` (the same structure as `spec:` in a Workflow CR), minus `orchestration.target` which is injected at Build time:

```yaml
jobTemplate:
  spec:
    nodeHealthMonitor:
      cel:
        expression: "node.spec.unschedulable == true"
    workload:
      trainJob:
        runtimeRef:
          name: hello-world-runtime
        trainer:
          image: "busybox:latest"
          command: ["echo"]
          args: ["hello world"]
          numNodes: 1
          numProcPerNode: 1
orchestration:
  iterations: 1
```

Dependencies use the existing DependencySpec serialization — the custom `UnmarshalJSON` in `dependency_json.go` correctly extracts `lifecycle`/`when` sibling fields from the flat YAML/JSON representation.

### Loader (`loader.go`)

```go
//go:embed entries/*/*.yaml
var entriesFS embed.FS

func init() {
    // Walk entries, parse domain/variant from path, unmarshal YAML,
    // Register with Build closure that DeepCopies + injects runtime config
}
```

Pre-parses all YAML at init time and panics on errors (same fail-fast pattern as `mustMarshalJSON`). Each `Build()` call DeepCopies the template and applies three generic post-processing functions:

- `injectTarget(spec, target)` — sets `spec.Orchestration.Target`
- `injectImagePullSecrets(spec, secrets)` — finds/creates "trainer" PodTemplateOverride, sets ImagePullSecrets
- `injectStorageClassName(spec, className)` — walks PVC dependencies, patches `storageClassName` in raw JSON

### Files deleted (10 Go files)

`training_helloworld.go`, `training_nemotron_15b.go`, `training_nemotron_gb300.go`, `training_nemotron6.go`, `training_nemotron_340b.go`, `diagnostics_dcgm.go`, `diagnostics_nvlink.go`, `diagnostics_clusterkit.go`, `communication_nccl.go`, `stress_gpu.go`

### `catalog.go` cleanup

Remove dead helpers: `mustMarshalJSON()`, `defaultHealthMonitor()`, `runtimeKindTrainingRuntime`, `imageNemoNCCL228`. Keep: `BuildConfig`, `Entry`, `Register`, `Lookup`, `DefaultHealthExpression`, registry.

## Rationale

- **YAML is the natural format for Kubernetes resource specs.** Dependencies, overrides, and workload specs are Kubernetes objects. Writing them as YAML eliminates the `map[string]any` indirection and makes them validateable by standard K8s tooling.
- **Shell scripts are cleaner in YAML.** YAML `|` block scalars preserve newlines natively, avoiding Go raw string edge cases.
- **NeMo configs are YAML-in-YAML.** The 200+ line NeMo training configs currently stored as Go `const` strings are naturally expressed as ConfigMap `data:` block scalars.
- **Generic post-processing eliminates per-entry code.** Three small functions handle all runtime configuration for all 14 entries. Adding a new entry requires zero Go code — just a YAML file.
- **`sigs.k8s.io/yaml` makes this safe.** The library converts YAML→JSON→Go struct, so all custom JSON unmarshalers (`DependencySpec`, `apiextensionsv1.JSON`, `intstr.IntOrString`) work transparently with YAML input.
- **Existing golden file tests validate the conversion.** If all 14 golden files pass after conversion, the YAML files produce identical `WorkflowSpec` structs.

## Consequences

### Positive
- Adding a new catalog entry requires only a YAML file — no Go code, no compilation
- Catalog entries are standard Kubernetes YAML, readable by any platform engineer
- Dependencies and overrides can be validated by standard K8s schema tools
- The 10 Go files (~4,000 lines) are replaced by 14 YAML files + 1 loader (~150 lines)
- No API changes — `Build()` signature and behavior are identical

### Negative
- NCCL variants (3 files) and stress variants (2 files) duplicate shared structures (MPI launcher/node job specs) instead of sharing Go helper functions
- YAML parse errors surface at binary startup (panic) rather than compile time
- YAML lacks type safety for Kubernetes resource structure (mitigated by golden file tests)

## Alternatives Considered

### Keep Go but extract large strings
**Rejected** because: The fundamental problem isn't the embedded strings — it's that Kubernetes resources are written as Go `map[string]any` literals. Moving strings to files while keeping the map-based construction doesn't improve readability.

### Go templates with YAML
**Rejected** because: Adding a template engine (Go `text/template` or similar) would require a custom parser, break the clean `yaml.Unmarshal` path, and add complexity. The three post-processing functions are simpler and type-safe.

### External YAML files loaded at runtime
**Rejected** because: The catalog is operational configuration that should be compiled into the binary (see ADR-010). `//go:embed` maintains this property while using YAML format.

## References

- ADR-010: Certification Catalog (`pkg/catalog/`)
- ADR-023: Catalog Configurability (BuildConfig, tolerations, imagePullSecrets, storageClassName)
- `api/v1alpha1/dependency_json.go` — custom DependencySpec JSON marshaling
- `sigs.k8s.io/yaml` — YAML→JSON→Go struct conversion
