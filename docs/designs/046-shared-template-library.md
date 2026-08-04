# ADR-046: Shared Template Library for Catalog Entries

## Context

ADR-025 converted catalog YAML files into Go templates, and ADR-024 embedded them as `entries/{domain}/{variant}.yaml`. As the catalog grew to 14 entries across 5 domains with platform overrides for AWS, GCP, Azure, OCI, and GPU architecture variants (H100, GB200, GB300), massive YAML duplication emerged:

- The GCP H100 TCPXO daemon block (~65 lines) is copied word-for-word across 7 entries (4 communication + 3 training), differing only in the runtime resource name.
- The GB200/GB300 ComputeDomain + DRA claims (~30 lines) are duplicated across 8+ entries.
- GCP GB200 RoCE networking annotations and resource requests (~35 lines) appear in 8+ entries.
- The FastRak NCCL tuning profile (~48 NCCL variables) is duplicated in both mpirun `-x` and container `env:` formats.
- AWS and Azure topology XMLs (~50 and ~35 lines respectively) are byte-identical across communication entries.
- The `gb200/gb300` topology key orchestration override (~5 lines) appears in 13 entries.

This duplication creates three problems:

1. **Maintenance burden**: updating a TCPXO daemon image version requires editing 7 files and verifying each manually.
2. **Copy-paste drift**: subtle inconsistencies have already appeared between entries (e.g., ordering differences in NCCL variables).
3. **Contributor friction**: writing a new NCCL communication test requires ~800 lines of boilerplate, most of it copy-pasted platform overrides that the writer doesn't understand or need to modify.

The existing `includeFile` and `includeTemplate` functions are scoped to each entry's sibling directory (`entries/{domain}/{variant}/`). There is no mechanism to share template fragments across entries.

## Decision

Add a shared template library directory at `entries/_lib/` and a new `lib` template function that reads fragments from it. Catalog entries replace duplicated blocks with `{{ lib "path" . | indent N }}` calls. The `TemplateData` struct gains an `EntryName` field (auto-populated from the variant name) so shared templates can construct entry-specific resource names like `{{ .EntryName }}-runtime`.

## Implementation

### Loader changes (`pkg/catalog/loader.go`)

**Add `EntryName` to `TemplateData`:**

```go
// EntryName is the variant name of the catalog entry (e.g., "nccl-all-gather").
// Shared library templates use this to construct entry-specific resource names
// like {{ .EntryName }}-runtime or {{ .EntryName }}-compute-domain.
EntryName string
```

Auto-populated in the `Build` closure from the `variant` captured at registration time.

**Add `makeLibTemplate` function:**

```go
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
```

Unlike `includeFile`/`includeTemplate` (which silently return empty string on missing files), `lib` returns an error. Shared library files must exist — a missing file indicates a broken reference that should fail loudly at init-time validation.

**Skip `_lib` during entry discovery:**

```go
if strings.HasPrefix(rel, "_lib/") {
    return nil
}
```

### Shared template fragments (`entries/_lib/`)

```
entries/_lib/
├── deps/
│   ├── gcp-h100-tcpxo-runtime-patch.yaml     # TCPXO daemon + volumes + annotations
│   ├── gb200-compute-domain.yaml              # ComputeDomain resource
│   ├── gb200-dra-runtime-patch.yaml           # DRA claims on runtime
│   ├── gcp-gb200-roce-runtime-patch.yaml      # RoCE annotations + network resources
│   ├── gb300-roce-claim-template.yaml         # ResourceClaimTemplate for GB300
│   └── gb300-roce-runtime-patch.yaml          # Dual-claim runtime patch
├── nccl/
│   ├── gcp-h100-fastrak-mpiargs.yaml          # FastRak NCCL as mpirun -x args
│   └── gcp-h100-fastrak-env.yaml              # FastRak NCCL as env: list
├── topo/
│   ├── aws-h100.xml                           # AWS PCIe topology XML
│   └── azure-h100.xml                         # Azure PCIe topology XML
└── overrides/
    └── gb200-topology-key.yaml                # Complete gb200/gb300 topology override
```

Each fragment is written at its "natural" indentation (list items starting with `- `) and callers use `| indent N` to place it at the correct depth:

```yaml
# In entry YAML:
  dependencies:
{{ lib "deps/gcp-h100-tcpxo-runtime-patch.yaml" . | indent 2 }}
  jobTemplate:
    spec:
      workload:
        trainJob:
          trainer:
            args:
            ...
{{ lib "nccl/gcp-h100-fastrak-mpiargs.yaml" . | indent 12 }}
            ...
```

### Entry refactoring

All 14 catalog entries are updated to use `lib` calls. No entries are removed or added. The rendered output of every entry is byte-identical before and after the change.

## Rationale

- **Single function, minimal API surface.** One `lib` function covers all use cases. It follows the same `(path, data) → (string, error)` pattern as `includeTemplate`, so the learning curve is zero.
- **Error on missing files.** Unlike entry-specific `includeFile`/`includeTemplate` (which return empty string for missing files to support override sections that reference files for only some node counts), shared library files must always exist. A typo in a lib path fails at init, not silently at runtime.
- **`EntryName` is auto-populated.** Catalog writers don't need to pass the entry name manually — it's derived from the filesystem path, consistent with the existing catalog convention.
- **Scope limited to identical blocks.** Only blocks that are truly byte-identical across entries (parameterized solely by `EntryName` and existing `TemplateData` fields) are extracted. This guarantees zero output change and avoids premature abstraction.

## Consequences

### Positive

- ~2,100 lines of duplicated YAML eliminated across 14 entries
- Platform infrastructure changes (TCPXO image, NCCL tuning) require editing one file instead of 7+
- New catalog entries can compose from building blocks instead of copy-pasting boilerplate
- No changes to rendered output, tests, golden files, or controller code

### Negative

- Template indirection: reading an entry now requires following `lib` references to understand the full spec
- Indentation sensitivity: callers must use the correct `indent N` value (caught by golden file tests)
- New naming convention to learn (`entries/_lib/` path structure)

### Mitigated

- Indirection is mitigated by clear naming (`deps/gcp-h100-tcpxo-runtime-patch.yaml` is self-documenting)
- Indentation errors are caught at init time (template validation with empty data) and by existing golden file tests

## Alternatives Considered

### Named templates (`{{ define }}` / `{{ template }}`)

Go `text/template` supports named templates, but they require pre-registration in the same template tree. This adds complexity to the loader (parse shared templates first, then attach to each entry template) and introduces name collision risk between entries. The `lib` function approach is simpler and namespace-safe.

### Helm-style library charts

Helm's library chart pattern is more powerful but requires a package manager and dependency resolution system. Over-engineering for a single-binary embedded catalog.

### Parameterized platform override templates

Extracting complete override blocks (including `when:`, `dependencies:`, `jobTemplate:`) as shared templates would achieve higher deduplication but entries vary in their `jobTemplate` sections (different NCCL variables, different mpirun args). Restricting to identical sub-blocks is safer and still achieves >80% of the dedup benefit.

## References

- ADR-024: YAML-Embedded Catalog
- ADR-025: YAML Template Catalog
- ADR-012: Platform and GPU Architecture Overrides
- ADR-031: Platform-Aware NCCL Config
