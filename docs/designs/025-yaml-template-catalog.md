# ADR-025: YAML Template Catalog — Replace Post-Parse Injection with Go Templates + Sprig

## Context

ADR-024 replaced Go catalog files with embedded YAML, but the loader still uses fragile post-parse injection to modify the parsed `WorkflowSpec`:

- `injectImagePullSecrets()` walks typed Go structs to find/create PodTemplateOverrides — tightly coupled to the TrainJob type structure.
- `patchPVCStorageClass()` performs nested JSON unmarshal/marshal on `DependencySpec.Raw` bytes to patch `storageClassName` — fragile, silently swallows errors, and will break if the DependencySpec serialization changes.

Both approaches modify parsed data structures after YAML deserialization, which is the wrong abstraction layer. The values should be rendered into the YAML text before parsing, not injected into Go structs after.

## Decision

Convert catalog YAML files into Go templates rendered with `text/template` + [sprig](https://github.com/Masterminds/sprig) functions. Template execution happens at Build time (before YAML unmarshal), eliminating all post-parse injection except the simple `injectTarget()` struct assignment.

## Implementation

### Template rendering flow

```
Init:   embed YAML → template.Parse (with sprig funcs) → store *template.Template
Build:  template.Execute(data) → yaml.Unmarshal → injectTarget → return WorkflowSpec
```

Templates are compiled once at init (fail-fast on syntax errors). Each `Build()` call executes the template with runtime data, then unmarshals the result.

### Template data (extensible struct)

```go
type TemplateData struct {
    ImagePullSecrets []corev1.LocalObjectReference
    StorageClassName string // empty = omit (cluster default)
}
```

Built from `BuildConfig` at Build time. Future fields can be added without changing the loader.

### Custom template functions

- All [sprig v3](https://github.com/Masterminds/sprig) functions
- `toYaml` — marshals any value to a YAML string (same as Helm's `toYaml`)

### Template syntax in YAML entries

**imagePullSecrets** (conditional block, omitted when empty):
```yaml
        podTemplateOverrides:
        - spec:
            {{- if .ImagePullSecrets }}
            imagePullSecrets:
            {{- range .ImagePullSecrets }}
            - name: {{ .Name }}
            {{- end }}
            {{- end }}
            volumes:
            ...
```

**storageClassName** (conditional field, omitted when empty):
```yaml
  spec:
    accessModes:
    - ReadWriteMany
    {{- if .StorageClassName }}
    storageClassName: {{ .StorageClassName }}
    {{- end }}
    resources:
      ...
```

Entries without dynamic fields remain plain YAML — templates without `{{ }}` directives pass through unchanged.

### Files modified

- `loader.go` — rewrite: template.Parse at init, template.Execute at Build; remove `injectImagePullSecrets`, `injectStorageClassName`, `patchPVCStorageClass`; keep `injectTarget` (safe struct assignment)
- `go.mod` / `go.sum` — add `github.com/Masterminds/sprig/v3`
- Training YAML entries get template directives for imagePullSecrets and storageClassName
- Other entries unchanged (no dynamic fields)

## Rationale

- **Render before parse, not inject after parse.** Template rendering produces correct YAML text; the unmarshal step sees the final document with no surprises.
- **Eliminates JSON byte manipulation.** The `patchPVCStorageClass` function does 4 levels of JSON unmarshal/marshal with silent error swallowing. Templates make the value placement explicit and visible.
- **Sprig provides a battle-tested function library.** String manipulation, conditionals, defaults — no need to write custom helpers.
- **Extensible without loader changes.** Adding a new template variable requires only adding a field to `TemplateData` and using it in a YAML entry. No new injection functions.
- **`injectTarget` stays.** The target is a complex Kubernetes struct (`TargetSpec` with maps, slices, sub-structs). Serializing it through templates would be more fragile than a simple struct assignment.

## Consequences

### Positive
- Fragile JSON manipulation eliminated
- Template directives are visible in YAML files (self-documenting)
- Adding new configurable fields = add struct field + use in YAML template
- Sprig's `default`, `quote`, `indent` available for future use

### Negative
- New dependency: `github.com/Masterminds/sprig/v3`
- Template syntax errors in YAML are caught at startup (panic), not compile time
- Template directives add visual noise to YAML files (mitigated by `{{- }}` trim markers)

## References

- ADR-024: YAML-Embedded Catalog
- ADR-023: Catalog Configurability (BuildConfig)
- [Masterminds/sprig](https://github.com/Masterminds/sprig) — Go template function library
