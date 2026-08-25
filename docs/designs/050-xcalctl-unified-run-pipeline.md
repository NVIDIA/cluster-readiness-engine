# ADR-050: Unified nvcrectl certification run Pipeline

> **Status:** Proposed

## Context

`nvcrectl certification run` has three divergent execution paths that independently handle namespace creation, secret management, wait/watch logic, and report printing:

1. **`runCertificationRun()`** — `--category` flags path
2. **`runCertificationFromFile()`** — `--cert-file` without `--watch`
3. **`runCertificationLifecycle()`** — `--cert-file --watch` (full lifecycle)

This divergence caused 3 regressions in one sprint: Path 1 was missing namespace creation (namespace not found error), Path 1 was missing `--image-pull-secret` wiring (flag silently ignored), and Paths 1 & 2 were missing report printing after `--wait`. Each fix had to be applied independently to each path.

The `--watch` flag is a monolithic "do everything" flag that conflates three orthogonal concerns: cluster setup (install CRDs/controller), waiting for completion, and cleanup (teardown installed components). Users cannot install without cleaning up (useful for debugging), or wait without installing (when the controller is already deployed).

Additionally, `cleanupLifecycle()` duplicates the phase teardown logic from `runReset()` in `setup.go` instead of reusing it.

## Decision

Replace the three divergent paths with a **single execution pipeline** (`executeCertificationRun`) that all input modes (flags and file) converge into. Replace `--watch` with three composable flags:

| Flag | Purpose |
|------|---------|
| `--setup` | Probe cluster + install CRDs/controller/LogProfiles via `runInit()` |
| `--wait` | Watch certification to completion via K8s watch API, then print report |
| `--cleanup` | Delete certification, namespace, and installed phases via `runReset()` |

`--watch` is kept as a hidden deprecated alias for `--setup --wait --cleanup`.

The `--wait` flag switches from 5-second polling (`waitForCertification`) to the K8s streaming watch API (`watchCertification`), which provides instant status updates and lower API server load.

## Implementation

### Pipeline Architecture

Two builder functions handle the only point of divergence (how the Certification object is constructed):

- **`buildConfigFromFlags()`** — `--category` path: discovers GPU product from cluster, validates categories against catalog, builds Certification from CLI flags.
- **`buildConfigFromFile()`** — `--cert-file` path: reads Certification from YAML, defaults namespace.

Both return a `certRunConfig` struct consumed by the single pipeline:

```go
type certRunConfig struct {
    cert            *crev1alpha1.Certification
    namespace       string
    imagePullSecret string
    doWait          bool
    doSetup         bool
    doCleanup       bool
    timeout         time.Duration
    kubeconfig      string
    kubeContext     string
    out             io.Writer
}
```

`executeCertificationRun(cfg)` executes phases in order:

```
[setup]   SIGINT/SIGTERM handling
[setup]   probeExistingComponents() → buildSkipPhasesFromProbe()
[setup]   runInit() (from setup.go, autoApprove=true, skip pre-existing)
[setup]   Create controller pull secret in cluster-readiness-engine namespace

[ALL]     Create K8s watch client (newK8sWatchClient)
[cleanup] Register cleanup defer (delete cert, namespace, call runReset)

[ALL]     ensureNamespace() (track if created)
[ALL]     createImagePullSecret() (if --image-pull-secret)
[ALL]     Create Certification resource

[wait]    watchCertification() + printReport()
[no-wait] Print kubectl status command
```

### Reusing setup.go Functions

- **`--setup`** calls `runInit(image, imagePullSecret, skipPhases, true, kubeconfig, kubeContext, "", nil, out)` — the same function used by `nvcrectl setup init`.
- **`--cleanup`** calls `runReset(skipPhases, true, kubeconfig, kubeContext, "", nil, out)` — the same function used by `nvcrectl setup reset`. The `skipPhases` argument ensures only phases installed by `--setup` are torn down.
- Certification and namespace deletion (cert-run-specific) execute before `runReset()` in the cleanup defer.

This eliminates `cleanupLifecycle()` which previously duplicated `runReset`'s embedded manifest deletion logic.

### Deleted Functions

- `runCertificationRun()` — absorbed into `buildConfigFromFlags()` + pipeline
- `runCertificationFromFile()` — absorbed into `buildConfigFromFile()` + pipeline
- `runCertificationLifecycle()` — absorbed into pipeline's setup/cleanup phases
- `ensureLifecycleNamespace()` — replaced by inline `ensureNamespace()` call
- `waitForCertification()` — replaced by `watchCertification()` (streaming)
- `cleanupLifecycle()` — replaced by `runReset()` + inline cert/ns deletion

### Kept Functions

All helper functions remain unchanged: `watchCertification`, `discoverGPUProduct`, `parseCategories`, `ensureNamespace`, `createImagePullSecret`, `probeExistingComponents`, `buildSkipPhasesFromProbe`, `buildReport`, `printReport`, `waitForDeletion`, `runInit`, `runReset`.

## Rationale

1. **Single pipeline eliminates divergence bugs.** Fixes to any phase (namespace, secrets, reporting) automatically apply to all input modes. The three regressions from this sprint would have been impossible.

2. **Composable flags give users control.** `--setup` without `--cleanup` leaves infrastructure for debugging. `--wait` without `--setup` works when the controller is already deployed. `--cleanup` can run independently to tear down a previous `--setup`.

3. **Streaming watch is strictly better than polling.** Instant status updates, lower API server load, no 5-second latency gap. The polling implementation is dead code after this change.

4. **Reusing `runInit`/`runReset` eliminates duplication.** `cleanupLifecycle` duplicated 30 lines of phase teardown logic. Delegating to `runReset()` means setup and reset improvements (e.g., new phases, better error handling) automatically apply to `--cleanup`.

## Consequences

- **Breaking change:** `--watch` becomes a hidden deprecated alias. Users should migrate to `--setup --wait --cleanup`. The alias ensures backward compatibility for one release cycle.
- **Behavioral change:** `--wait` now uses streaming watch instead of polling. Status updates are instant instead of every 5 seconds.
- **Simpler mental model:** Each flag does one thing. No hidden side effects.
- **Reduced code:** ~200 lines removed (3 large functions + `cleanupLifecycle` + `waitForCertification` replaced by one pipeline function + two small builders).

## Alternatives Considered

1. **Keep three paths, add shared helper functions.** Each path calls shared `doEnsureNamespace()`, `doCreateSecret()`, `doWaitAndReport()`. Rejected: still three code paths to maintain, shared helpers can diverge (different error handling, different output formatting).

2. **Keep `--watch` as-is, just fix the bugs.** Rejected: proven to be a regression magnet. The monolithic flag conflates concerns users need independently.

3. **Use middleware/interceptor pattern.** Each concern (setup, wait, cleanup) wraps the next as middleware. Rejected: over-engineered for a CLI tool. Go's linear function calls are clearer.

## Notes

- `probeExistingComponents()` and `buildSkipPhasesFromProbe()` remain in `certification.go` since they're used by the certification run pipeline, not by `setup init`/`setup reset` directly.
- The `--setup` flag does not require `--cert-file` (unlike the old `--watch`). Both `--category` and `--cert-file` paths can use `--setup`.

## References

- ADR-044: nvcrectl certification lifecycle (`--watch` original design)
- ADR-045: nvcrectl embedded config (embedded manifests used by `runInit`/`runReset`)
- `pkg/setup/setup.go`: `runInit()` (line 137), `runReset()` (line 330)
- `pkg/certification/certification.go`: current three paths

## Change Log

| Date | Change |
|------|--------|
| 2026-02-25 | Initial proposal |
