# ADR-076: Configurable Gang Scheduler Queue Label Key (Run:ai Support)

> **Status:** Accepted

## Context

NVCRE's gang scheduling support is a pure passthrough: `spec.gangScheduler.schedulerName` is a free-form string (Required, MinLength=1, no enum; `api/v1alpha1/workloadrun_types.go:288-303`), and the same `GangSchedulerSpec` is reused by Certification (`api/v1alpha1/certification_types.go:345`). Setting `schedulerName: runai-scheduler` therefore works today. The queue label key, however, is a compile-time constant: `labelKeyGangQueue = "kai.scheduler/queue"` (`pkg/platform/gang_scheduler.go:17`). Only the queue value is settable (`gangScheduler.queue`, defaulting to `default-queue` in code, `pkg/platform/gang_scheduler.go:27-32`); no spec field, flag, or override changes the key.

The NVIDIA Run:ai platform documents third-party workloads with `schedulerName: runai-scheduler` and the `runai/queue` label, and does not document honoring `kai.scheduler/queue`, even though the open-source KAI Scheduler (the engine inside the Run:ai platform) uses `kai-scheduler` and `kai.scheduler/queue`. So on a Run:ai cluster, NVCRE pods bind through the right scheduler but carry a queue label it ignores, and queue placement falls back to Run:ai's own project/namespace mapping.

Two related nuances:

- The queue label is written to the **Job template metadata** (`replicatedJobs[].template.metadata.labels`), not the pod template, at all three write sites: `ApplyGangSchedulerToDependencies` for Certification catalog runtimes (`pkg/platform/gang_scheduler.go:85-87`), `BuildTorchRuntime` (`pkg/platform/runtime.go:217-220`, via `applyGangScheduler` at `:132-138`), and `BuildMPIRuntime` (`pkg/platform/runtime.go:356-360` worker, `:410-413` launcher). Whether the pods themselves carry the label therefore depends on Trainer/JobSet label propagation. The docs already describe the label as landing on "the pod template metadata" (`docs/how-to-guides/run-workloadrun.md:112`, `docs/api-reference/workloadrun.md:50`), which the code does not literally do today.
- Every gangScheduler example in the docs shows `kai-scheduler` only (`docs/api-reference/certification.md:25`, `docs/api-reference/workloadrun.md:27`, `docs/how-to-guides/certify-a-cluster.md:72`, `docs/how-to-guides/run-workloadrun.md:103`, `docs/how-to-guides/workloadrun-deepseek-v3.md:297`, `docs/how-to-guides/workloadrun-nemotron5.md:335`; the label key also appears in prose at `docs/cli-reference/certification.md:82`). Run:ai is never mentioned.

## Decision

1. **Add an optional `queueLabelKey` field to the shared `GangSchedulerSpec`.** Empty means `kai.scheduler/queue`, resolved in code the same way `queue` defaults to `default-queue`, so the key resolves identically for existing specs (byte-identical rendering holds only for specs with no `gangScheduler`; specs that set it gain the pod-template label from Decision 2). The field is validated as a Kubernetes label key (qualified name: optional DNS-subdomain prefix, `/`, name segment).
2. **Stamp the queue label on the pod template metadata in addition to the Job template metadata** at all three write sites, so queue assignment does not depend on Trainer/JobSet label propagation. Only the queue label is added at the pod level; the other Job-template labels (`app`, `trainer.kubeflow.org/trainjob-ancestor-step`) stay where they are.
3. **Document the Run:ai combination** at every gangScheduler example: `schedulerName: runai-scheduler`, `queueLabelKey: runai/queue`, and a note that `queue` must name an existing Run:ai queue.

Example:

```yaml
spec:
  gangScheduler:
    schedulerName: runai-scheduler
    queueLabelKey: runai/queue
    queue: team-a
```

## Implementation

- `api/v1alpha1/workloadrun_types.go`: `QueueLabelKey string` on `GangSchedulerSpec` with `+optional`, `+kubebuilder:validation:MaxLength=317` (253-char prefix + `/` + 63-char name), and a qualified-name `Pattern` that admits the empty string, exactly as the sibling `queue` pattern does, and bounds the name segment inline: `^$|^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[a-zA-Z0-9]([-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?$` (verified with Go's `regexp` package, the RE2 engine CRD patterns use: accepts `""`, `runai/queue`, and `kai.scheduler/queue`, rejects a 64-character name segment with or without a prefix). A CEL `XValidation` caps the optional prefix at 253 characters, `self.contains('/') ? self.split('/')[0].size() <= 253 : true`; that is the one bound a single RE2 pattern cannot express, since asserting a total length across the variable dot-separated prefix segments needs a lookahead RE2 does not have. The CEL rule is a no-op on the empty string: `"".contains('/')` is false, so the ternary yields true. No `+kubebuilder:default`: the default lives in code, mirroring `queue` (see Rationale). Because `GangSchedulerSpec` is shared, this one change covers both CRDs; run `make manifests generate`.
- `pkg/platform/gang_scheduler.go`: a `gangSchedulerQueueLabelKey(key string) string` helper beside `gangSchedulerQueue` (`:27-32`); `ApplyGangSchedulerToDependencies` uses it and additionally writes the label into `replicatedJobs[].template.spec.template.metadata.labels` (the existing `ensureMap` helper at `:125-132` already creates absent metadata/labels maps).
- `pkg/platform/runtime.go`: `RuntimeConfig` gains `GangSchedulerQueueLabelKey` beside `GangSchedulerName`/`GangSchedulerQueue` (`:120-125`); `applyGangScheduler` (`:132-138`) takes the pod template labels map as well and stamps the queue label there; `BuildTorchRuntime` and `BuildMPIRuntime` pass the pod template metadata at their replicatedJob construction sites (`:217-220`, `:356-360`, `:410-413`).
- Plumbing: the two `RuntimeConfig` fill sites copy the new field (`pkg/controller/workloadrun_controller.go:319-320`, `pkg/workloadrun/workloadrun.go:274-275`). The three `ApplyGangSchedulerToDependencies` call sites are unchanged since they pass the whole spec (`pkg/controller/certification_controller.go:508`, `pkg/certification/certification.go:208` and `:270`).
- Docs: the six example sites and the two prose sites listed in Context gain the Run:ai variant and the corrected label-placement description; the Run:ai example lives in the docs pages (the repo no longer carries a `config/samples/` directory).

### Testing plan

- **testutil goldens**: new cases under `pkg/platform/testdata/apply-gang-scheduler-deps/` (custom key, invalid-shaped key rejected at the CRD not here, default key unchanged) and existing cases regenerate to show the pod-template label. Same for the render goldens (`pkg/certification/testdata/certification-render-gang-scheduler/`) and the integration cases (`cmd/integration/testdata/reconcile/certification-gangscheduler-nccl`, `certification-gangscheduler-training`). Every regeneration is reviewed field by field and gated on maintainer approval per CLAUDE.md.
- **Render verification**: `nvcrectl certification render` on a cert with `queueLabelKey: runai/queue` shows the label at both metadata levels and no `kai.scheduler/queue` anywhere; a cert without `gangScheduler` renders byte-identically to today; a cert with `gangScheduler` and no `queueLabelKey` renders today's output plus the pod-template copy of the same `kai.scheduler/queue` label.
- **Field validation** on a real Run:ai cluster is available through a field engagement.

## Rationale

- **Default in code, not in the CRD.** `queue` already defaults to `default-queue` in code with no structural default (`pkg/platform/gang_scheduler.go:19`, `:27-32`); following that pattern keeps stored objects untouched and keeps nvcrectl's file-based render path (which never sees API-server defaulting) consistent with the controller. Because the code treats an explicit `""` the same as an omitted field, admission must too: the pattern's `^$` alternative accepts the empty string, matching `queue`, rather than rejecting at admission a value the code would happily resolve to the default key.
- **A label-key string is the whole requirement.** NVCRE has no scheduler-specific behavior beyond injecting `schedulerName` and one queue label; the difference between KAI and Run:ai is exactly the key. A string field is the smallest change that closes the gap and needs no updates when the next gang scheduler appears with its own key.
- **Pod-template stamping removes a dependency on someone else's propagation rules.** Kubernetes Jobs do not copy Job-object labels onto pods; today the pods carry the queue label only if the Trainer/JobSet layer forwards template metadata. Writing the label at both levels makes the guarantee local and, as a side effect, makes the existing docs sentence true.

## Consequences

- **CRD schema change to both CRDs.** `GangSchedulerSpec` is defined once and referenced by WorkloadRun and Certification, so `make manifests generate` rewrites both CRD YAMLs under `helm/cluster-readiness-engine/crds/`.
- **Immutability.** Both specs are wholly immutable after creation (`+kubebuilder:validation:XValidation:rule="self == oldSelf"` at `api/v1alpha1/certification_types.go:388` and `api/v1alpha1/workloadrun_types.go:359`), so `queueLabelKey` is fixed at creation like every other field; switching keys means a new resource. There is no transition-rule interaction: the field is optional with no CRD default, so existing stored objects do not change shape and `self == oldSelf` continues to hold on their updates.
- **Golden updates.** Every gang-scheduler golden (unit, render, integration) gains the pod-template label. For clusters already running KAI this is a rendered-output change but not a behavioral one: same key, same value, one extra location.
- **Zero behavior change when unset.** Specs without `queueLabelKey` produce today's output plus the pod-template copy of the same label.

## Alternatives Considered

- **A scheduler flavor enum** (`gangScheduler.flavor: kai | runai` selecting key and defaults). Rejected: NVCRE has no other scheduler-specific behavior to hang off the enum, so it would be a two-value indirection over a single string, and every future scheduler would need a code change instead of a YAML value.
- **Document the namespace-level `runai/enforce-scheduler-name` annotation instead of any code change.** Noted as a complement, not a substitute: it only enforces the scheduler name on pods in the namespace, it does not translate the queue label key, so queue placement would still fall back to Run:ai's project/namespace mapping. The Run:ai docs section will mention it as an alternative to setting `schedulerName` explicitly.
- **A free-form `labels` map on `gangScheduler`.** Rejected: it duplicates the existing single-purpose `queue` field, weakens validation (any label could be injected through a gang-scheduling knob), and the requirement is one key.

## Notes

- Run:ai associates workloads with projects at the namespace level; the supported third-party path is running in a namespace associated with a Run:ai project, and the platform validates the named queue rather than falling back to a default. The docs pages state both. Pod-level admission behavior for a nonexistent queue is confirmed during the field validation.
- The empty-`schedulerName` no-op guard stays as is (`pkg/platform/gang_scheduler.go:52`): a `queueLabelKey` without a scheduler name renders nothing, matching today's contract that gang scheduling is off unless a scheduler is named.
- The `queue` value validation (label value, max 63 chars, `workloadrun_types.go:300-301`) is unchanged and applies to whatever key is used.
- External facts relied on above: Run:ai third-party workloads use `schedulerName: runai-scheduler` with the `runai/queue` label (Run:ai self-hosted docs, 2.20+); the open-source KAI Scheduler uses `kai-scheduler` and `kai.scheduler/queue` (KAI Scheduler quickstart).

## References

- ADR-012: Platform and GPU architecture overrides (override application order the gang scheduler is applied after; see `pkg/controller/certification_controller.go:506-508`)
- GitHub issue #322
- Run:ai third-party workload scheduling: https://run-ai-docs.nvidia.com (self-hosted 2.20+, "Scheduling third-party workloads")
- KAI Scheduler quickstart: https://github.com/NVIDIA/KAI-Scheduler
- Code citations:
  - `api/v1alpha1/workloadrun_types.go:288-303` (`GangSchedulerSpec`), `:359` (spec immutability)
  - `api/v1alpha1/certification_types.go:345` (Certification reuse), `:388` (spec immutability)
  - `pkg/platform/gang_scheduler.go:17` (hardcoded key), `:27-32` (queue code default), `:52` (empty-name guard), `:85-87` (Job template label write), `:125-132` (`ensureMap`)
  - `pkg/platform/runtime.go:120-125` (`RuntimeConfig` gang fields), `:132-138` (`applyGangScheduler`), `:217-220` (torch label placement), `:356-360` (MPI worker), `:410-413` (MPI launcher)
  - `pkg/controller/workloadrun_controller.go:319-320`, `pkg/workloadrun/workloadrun.go:274-275` (RuntimeConfig fill sites)
  - `pkg/controller/certification_controller.go:508`, `pkg/certification/certification.go:208`, `:270` (ApplyGangSchedulerToDependencies call sites)
  - Docs sites listed in Context (`docs/api-reference/*.md`, `docs/how-to-guides/*.md`, `docs/cli-reference/certification.md:82`)
