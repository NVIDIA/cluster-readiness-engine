# ADR-023: Catalog Configurability — Remove Hardcoded Values, Add Certification-Level Config

## Context

Every catalog entry currently hardcodes environment-specific values:

1. **Tolerations**: All entries include `dedicated=user-workload` toleration. This is fragile — different clusters use different taints, and burn-in workloads need to schedule on target nodes regardless of what taints are present.
2. **Image pull secrets**: Training entries hardcode `nvidia-ngcuser-pull-secret`. This secret name is deployment-specific and may not exist in every cluster. Some clusters use ServiceAccount-based image pulling and don't need pod-level secrets at all.
3. **Storage class names**: Entries with PVC dependencies hardcode `dgxc-enterprise-file`. Storage class names vary across clusters and providers.
4. **Health expressions**: Most entries set `node.spec.unschedulable == true` inline, while diagnostics/stress entries use an extended expression also checking for `nvidia.com/gpu-unhealthy` taints. The workflow controller already defaults to `node.spec.unschedulable == true` when no health monitor is specified.

These hardcoded values make catalog entries non-portable and require code changes for each new deployment target.

## Decision

1. **Tolerations**: Remove all hardcoded tolerations from catalogs. The workflow controller will unconditionally inject a wildcard toleration (`Operator: Exists`, no key) when creating jobs. Since workloads are already pinned to specific nodes via NodeAffinity, tolerating all taints is both safe and necessary.

2. **Image pull secrets**: Add optional `imagePullSecrets` field to `CertificationSpec`. Pass to catalog `Build()` via a new `BuildConfig` struct. Catalogs use the value when constructing workload specs. Omitting the field means no pod-level secrets (rely on ServiceAccount config instead).

3. **Storage class name**: Add optional `storageClassName` field to `CertificationSpec`. Pass to catalog `Build()` via `BuildConfig`. Catalogs that create PVC dependencies use the value. Catalogs without PVCs ignore it.

4. **Health expression**: Remove all explicit `NodeHealthMonitor` from catalog entries. The workflow controller's existing default (`node.spec.unschedulable == true`) applies universally. The extended `nvidia.com/gpu-unhealthy` taint check in diagnostics/stress entries is unnecessary because the remediation controller already cordons nodes on failure, which sets `unschedulable=true`.

## Implementation

### API changes

**`api/v1alpha1/certification_types.go`** — add two optional fields to `CertificationSpec`:

```go
type CertificationSpec struct {
    Target     TargetSpec            `json:"target"`
    Categories []CertificateCategory `json:"categories,omitempty"`

    // imagePullSecrets is an optional list of references to secrets for pulling
    // container images used by catalog workloads. If not specified, the cluster's
    // default image pull configuration (e.g., ServiceAccount) is used.
    // +optional
    ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

    // storageClassName is the StorageClass to use for PersistentVolumeClaim
    // dependencies created by catalog entries. If not specified, catalog entries
    // that require PVCs will use the cluster's default StorageClass.
    // +optional
    StorageClassName *string `json:"storageClassName,omitempty"`
}
```

### Catalog changes

**`pkg/catalog/catalog.go`** — new `BuildConfig` type, updated `Entry.Build` signature:

```go
type BuildConfig struct {
    ImagePullSecrets []corev1.LocalObjectReference
    StorageClassName *string
}

type Entry struct {
    Build func(target crev1alpha1.TargetSpec, config BuildConfig) crev1alpha1.WorkflowSpec
}
```

**All catalog files** — update `Build` function signatures, remove hardcoded tolerations, image pull secrets, health monitors, and storageClassName. Use `config.ImagePullSecrets` and `config.StorageClassName` where applicable.

### Controller changes

**`pkg/controller/certification_controller.go`** — construct `BuildConfig` from `CertificationSpec` and pass to `entry.Build()`:

```go
config := catalog.BuildConfig{
    ImagePullSecrets: certification.Spec.ImagePullSecrets,
    StorageClassName: certification.Spec.StorageClassName,
}
workflowSpec := entry.Build(certification.Spec.Target, config)
```

**`pkg/controller/workflow_controller.go`** — replace conditional TaintSelector-based toleration injection with unconditional wildcard:

```go
// Always tolerate all taints — workloads are pinned to specific nodes
// via NodeAffinity and must schedule regardless of taints.
adapter.SetTolerations(&job.Spec.Workload, []corev1.Toleration{{
    Operator: corev1.TolerationOpExists,
}})
```

## Rationale

- **Wildcard toleration is safe** because the workflow controller already pins workloads to specific nodes via `RequiredDuringSchedulingIgnoredDuringExecution` NodeAffinity. The toleration only determines *whether* a pod can schedule on the selected node, not *which* node it schedules on.
- **CertificationSpec is the right level** for image pull secrets and storage class because these are deployment-specific (not workload-specific). A single Certification targets one cluster environment, so one storage class and one set of secrets covers all its categories.
- **Removing health monitors from catalogs** eliminates redundancy. The workflow controller default at line 558-564 already handles this. The extended `nvidia.com/gpu-unhealthy` check is unnecessary since the remediation controller cordons failed nodes (`unschedulable=true`), and other failure detectors (GPU Operator, GPUd) also typically cordon the node.

## Consequences

### Positive
- Catalog entries become cluster-agnostic and portable
- New deployments don't require catalog code changes for tolerations, secrets, or storage
- Health expression is consistent across all catalog entries
- Certification API explicitly declares its infrastructure requirements

### Negative
- Users creating Certifications must now specify `imagePullSecrets` if their cluster requires pod-level secrets (previously automatic from catalog)
- Users must specify `storageClassName` if their cluster doesn't have a default StorageClass
- The `nvidia.com/gpu-unhealthy` taint check is lost from diagnostics/stress entries (mitigated by cordon behavior)

## Alternatives Considered

### Per-category image pull secrets and storage class
**Rejected** because: These values are deployment-level concerns, not workload-level. A GB300 cluster has one storage class and one set of secrets regardless of whether you're running DCGM diagnostics or Nemotron training.

### Tolerate only discovered node taints (instead of wildcard)
**Rejected** because: This requires reading all target nodes to collect their taints before creating jobs. The workflow controller already does node discovery for partitioning, but the toleration list could grow unboundedly. A wildcard toleration is simpler, standard Kubernetes practice, and equally safe given the NodeAffinity pinning.

### Make health expression configurable in CertificationSpec
**Rejected** because: The default expression (`node.spec.unschedulable == true`) is correct for all current use cases. Adding a configurable field increases API complexity for no practical benefit. Users with custom health checks can always create Workflows directly.

## References

- ADR-010: Certification Catalog (`pkg/catalog/`)
- ADR-004: CEL Node Health Monitoring
- ADR-006: Remediation Lifecycle
- `pkg/controller/workflow_controller.go:558-564` — default health monitor
- `pkg/controller/workflow_controller.go:551-555` — current toleration injection
