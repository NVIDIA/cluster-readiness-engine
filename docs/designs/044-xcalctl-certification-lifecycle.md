# ADR-044: Full Certification Lifecycle in ncrectl

## Context

ADR-042 introduced `ncrectl certification run` for creating Certifications from CLI flags with a `--wait` polling mode. Operators still need to orchestrate a multi-step workflow manually: install dependencies (`setup init`), create namespaces and secrets, apply a Certification YAML, monitor progress, inspect performance results, and tear down.

The desired UX is a single command that handles the entire lifecycle:

```
export SECRET=<NGC_API_KEY>
ncrectl certification run --cert-file cert.yaml --image-pull-secret $SECRET --watch
```

This command should install infrastructure, run the certification, display real-time progress and a performance report, and restore the cluster to its original state.

### Requirements

1. **Read Certification from file**: `--cert-file <path>` reads a Certification YAML (mutually exclusive with `--category`).
2. **Image pull secret**: `--image-pull-secret <NGC_API_KEY>` creates a `dockerconfigjson` secret for `nvcr.io` and injects it into the Certification spec.
3. **Full lifecycle with `--watch`**: Setup → namespace/secret creation → run → watch → report → cleanup.
4. **K8s watch mechanism**: Replace 5-second polling with event-driven watch via `client.NewWithWatch`.
5. **Performance report**: Display Runtime Goodput, TFLOPs, Train Time, Step Time grouped by topology domain; bandwidth per message size for communication categories.
6. **Clean teardown**: Only remove components that were newly installed; leave pre-existing infrastructure untouched.

## Decision

Enhance `ncrectl certification run` with three new flags (`--cert-file`, `--image-pull-secret`, `--watch`) that compose a six-phase lifecycle. The existing `--category` + `--wait` flow is preserved unchanged.

### New Flags

| Flag | Type | Description |
|------|------|-------------|
| `--cert-file` | string | Certification YAML file path |
| `--image-pull-secret` | string | NGC API key for `nvcr.io` image pull |
| `--watch` | bool | Run full lifecycle with watch-based monitoring |

`--cert-file` is mutually exclusive with `--category`, `--name`, `--nodes-per-job`.

### Lifecycle Phases (when `--watch` is used)

1. **Setup**: Probe cluster for existing components. Run `setup init --auto-approve`. Record which phases were newly installed.
2. **Namespace + Secret**: Create the target namespace (labeled `app.kubernetes.io/managed-by=ncrectl`). Create `dockerconfigjson` secret. Inject secret reference into `cert.Spec.ImagePullSecrets`.
3. **Create**: Apply the Certification to the cluster.
4. **Watch**: Monitor via K8s watch mechanism. Print category status changes in real time.
5. **Report**: Fetch Workflow, GoodputMeasurement, and BandwidthMeasurement resources. Print structured report with box-drawing characters.
6. **Cleanup**: Delete Certification (finalizer handles children). Delete namespace. Run `setup reset --auto-approve` only for newly-installed phases.

### Watch Implementation

Use `client.NewWithWatch()` from controller-runtime to create a watch-capable client. Watch the specific Certification by `metadata.name` field selector. Events arrive immediately on status changes, replacing the 5-second polling ticker.

### Image Pull Secret

The `--image-pull-secret` flag accepts an NGC API key. ncrectl creates a `kubernetes.io/dockerconfigjson` secret named `ncrectl-pull-secret` with:
- Server: `nvcr.io`
- Username: `token`
- Password: the provided API key

The secret name is appended to `cert.Spec.ImagePullSecrets` before the Certification is created, ensuring the controller propagates it to all workload pods.

### Performance Report

Metrics are fetched from child resources after the Certification reaches a terminal state:

- **Training categories**: GoodputMeasurement provides `result` (goodput ratio), `avgTFLOPSPerGPU`, `avgStepTimeSec`, `trainingTimeSec`. Results are grouped by topology domain from `workflow.Status.Orchestration.Groups[*].Domains`. If no topology exists, show averages across all groups.
- **Communication categories**: BandwidthMeasurement provides per-message-size `algBW`, `busBW`, `samples`.

Report destination: stdout on success (exit 0), stderr on failure (exit 1).

### Cleanup Strategy

Before setup, ncrectl probes for existing components (Kubeflow CRDs, CRE CRDs, controller Deployment, LogProfiles). On cleanup, `setup reset` is called with `--skip-phases` for any component that was already present, ensuring only newly-installed components are removed.

## Rationale

- **`--cert-file` over extending `--category`**: Certification YAML files allow full spec control (namespace, per-category options, target selectors) that CLI flags cannot easily express.
- **K8s watch over polling**: Events are immediate. For certifications running minutes to hours, watch is more responsive and generates less API traffic.
- **NGC-specific secret creation**: All CRE workload images come from `nvcr.io`. Hardcoding the registry and `token` username eliminates configuration for the 99% case.
- **Probe-based cleanup tracking**: More robust than a state file. Idempotent — multiple runs don't accumulate state.
- **New `report.go` file**: Report logic is substantial (~300+ lines) and deserves its own file rather than inflating `certification.go`.

## Consequences

### Positive

- Single-command certification lifecycle — no manual orchestration.
- Real-time progress via K8s watch.
- Structured performance report with topology-aware grouping.
- Clean teardown that respects pre-existing infrastructure.

### Negative

- `--watch` couples setup/teardown into the run command. Operators who want persistent infrastructure should use `setup init` separately and `--wait` instead.
- NGC-specific secret format may not work for all registries. Future work could add `--registry` and `--username` flags.

## Notes

- The existing `--category` + `--wait` flow is completely unchanged.
- `--cert-file` without `--watch` simply creates the Certification from the file and exits.
