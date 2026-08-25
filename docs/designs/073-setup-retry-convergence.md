# ADR-073: Convergent `setup init` Retry After a Partial Kubeflow Trainer Install

> **Status:** Accepted

## Context

`ncrectl setup init` installs Kubeflow Trainer by shelling out to `helm upgrade --install` ([`installTrainerHelmRelease`, helm.go:178-199](../../pkg/setup/helm.go#L178-L199)) with no server-side-apply, `--force`, or field-manager flags, and a retry of `setup init` runs the byte-identical command ([`installDepsPhase`, setup.go](../../pkg/setup/setup.go)). Field evidence from issue #180 shows this retry path can never converge after a partial install:

1. A first `setup init` failed partway through the `[deps]` phase, leaving the `kubeflow-trainer` Helm release in state `failed` with its webhook Secrets already created on the cluster.
2. The retry failed with **four certificate-data field-ownership conflicts across the two webhook Secrets** in `kubeflow-system` (the Trainer webhook cert Secret and the JobSet sub-chart's webhook cert Secret — `tls.crt`/`tls.key` on each). The kubeflow-trainer chart templates fresh self-signed certificate material on every render, so each retry tries to overwrite `data` fields whose ownership a plain Helm re-apply cannot take over.
3. `helm rollback` failed on the same four conflicts — Helm's own recovery tool re-applies through the same mechanics, so there is no Helm-native way out.
4. `ncrectl setup status` reported **ready** while the Trainer Helm revision was `failed`. Issue #179 / PR #188 fixes the reporting half: it adds `helmStateFunc` plumbing that queries `helm status -o json` per managed release and blocks readiness on `failed`/`pending-*` states.
5. The recovery that actually worked was manual: `helm uninstall kubeflow-trainer`, delete its four CRDs (`trainjobs`, `trainingruntimes`, `clustertrainingruntimes` in `trainer.kubeflow.org`; `jobsets` in `jobset.x-k8s.io`), delete the `kubeflow-system` namespace, then re-run the pinned `setup init`.

So the reporting gap is closed by #188, but `setup init` itself still retries into a wall: an identical command against identical conflicted state produces an identical failure, forever. `setup init` needs to converge from any partial state, and it needs to do so without ever endangering user workloads.

## Decision

1. **Make the `[deps]` phase state-aware using the #188 plumbing.** Before touching the Trainer release, `setup init` queries its state through `helmStateFunc` ([PR #188, helm.go](../../pkg/setup/helm.go)). When the release is already `deployed` at the pinned `kubeflowTrainerVersion`, the phase prints "already deployed" and **skips the upgrade entirely** — never re-rendering the chart means never re-rolling the webhook certs, which removes the trigger surface from healthy retries, not just failed ones.

2. **Detect the failure class by attempt-then-classify, not by pre-probing.** When the release is not cleanly deployed, run `helm upgrade --install` once. If it fails, classify the captured output against the server-side-apply conflict signature — Helm's `conflict` wording with conflicting paths under `.data` of Secrets in `kubeflow-system` — and combine that with the release state (`failed` or `pending-*`) from `helmStateFunc`. Both signals must agree before the failure is treated as this class. A Secret `managedFields` probe is not the detector; it is recorded in the printed diagnostics only.

3. **Layered recovery: automatic only when provably safe, fail-fast otherwise.**
   - **(a) Automatic safe recovery.** When the failure is conflict-classified AND the safety gate passes (see decision 5), `setup init` performs the field-validated recovery itself, exactly once per run: `helm uninstall kubeflow-trainer` → delete the four `trainer.kubeflow.org`/`jobset.x-k8s.io` CRDs (reusing [`deleteCRDsByGroup`](../../pkg/setup/setup.go)) → delete the `kubeflow-system` namespace and wait ([`WaitForNamespaceDeletion`](../../pkg/setup/setup.go#L477)) → reinstall the pinned chart fresh. CRDs are deleted deliberately and reinstalled by the fresh chart install; the safety gate guarantees no instances exist, so no data is lost.
   - **(c) Fail fast with the exact procedure.** When the failure is conflict-classified but the safety gate does not pass — or classification is ambiguous — `setup init` fails with the manual recovery procedure from issue #180 printed verbatim (uninstall, the four CRD deletions, namespace deletion, pinned re-init) plus the reason automatic recovery was refused.
   - Option (b), Helm-level `--force` or `--take-ownership`, is rejected outright (see Alternatives Considered).

4. **Enumerate the partial states and give each a deterministic action**, so `setup init` converges from any of them:

   | Observed state (Trainer release / CRE release) | `[deps]` action | `[helm]` action |
   |---|---|---|
   | not installed / not installed | fresh install | fresh install |
   | `deployed` at pinned version / any | skip (print "already deployed") | `upgrade --install` as today |
   | `deployed` at other version / any | `upgrade --install`; on failure, classify per decision 2 | as today |
   | `failed` or `pending-*` / any | attempt once → classify → recover (3a) or fail fast (3c) | reached only after `[deps]` converges |
   | `unknown` (state query could not complete) / any | `upgrade --install` as today; on failure, fail with raw Helm output | as today |

   The CRE release keeps its existing always-`upgrade --install` behavior: its chart renders no per-render certificate material, so it has no equivalent failure class. The same attempt-then-classify wrapper wraps it for symmetric error reporting, but with no automatic recovery arm.

5. **Safety rails.**
   - Automatic recovery is refused if **any `TrainJob` or `JobSet` instance exists** in the cluster — the same precondition the field recovery honored — or if any `TrainingRuntime`/`ClusterTrainingRuntime` exists that is not Helm-owned (missing the chart's `app.kubernetes.io/managed-by: Helm` metadata), since deleting the CRDs would destroy it.
   - Recovery never touches anything outside the `kubeflow-trainer` release, its four CRDs, and the `kubeflow-system` namespace that `setup init` itself creates via `--create-namespace`.
   - Recovery appears in the interactive confirmation flow like every other destructive phase: in interactive mode the recovery plan is printed and re-confirmed via [`promptForConfirmation`](../../pkg/setup/setup.go) before anything is deleted; `--auto-approve` covers CI.
   - Every action is printed as it happens under a `[deps][recover]` prefix, and the final summary states what was uninstalled, deleted, and reinstalled.
   - Exactly one recovery attempt per `setup init` invocation. If the reinstall fails again, `setup init` fails with the manual procedure — no loops.

## Implementation

- **`pkg/setup/helm.go`** — extend the #188 plumbing: `helmReleaseState` already returns the state string; add `helmReleaseChartVersion` (from the same `helm status -o json` payload) for the pinned-version skip. Add `classifyTrainerInstallFailure(output string) failureClass` matching the SSA conflict signature (`conflict` wording + `.data.` paths + Secret references) — a plain classifier over the captured `runHelm` transcript.
- **`pkg/setup/setup.go`** — `installDepsPhase` becomes the state machine from decision 4. New helpers:
  - `planTrainerPhase(state, chartVersion string) trainerAction` — pure function returning skip / install / attempt-with-recovery.
  - `trainerRecoveryGate(ctx, c)` — lists `TrainJob`, `JobSet`, `TrainingRuntime`, `ClusterTrainingRuntime` instances via the unstructured-list pattern already used by [`anyCRECRsExist`](../../pkg/setup/setup.go) and returns (safe bool, blockers []string).
  - `recoverTrainerRelease(sp setupPhaseParams)` — composes the existing [`uninstallTrainerHelmRelease`](../../pkg/setup/helm.go), [`deleteCRDsByGroup`](../../pkg/setup/setup.go) for both API groups (both already exist for `setup reset`), namespace deletion + [`WaitForNamespaceDeletion`](../../pkg/setup/setup.go#L477), then [`installTrainerHelmRelease`](../../pkg/setup/helm.go).
  - `printManualTrainerRecovery(out)` — the issue #180 procedure, printed on every fail-fast path.
- **Helm subprocess injection** — thread a `trainerHelm` struct of function fields (`state`, `install`, `uninstall helmStateFunc`-style func types) through `setupPhaseParams`, defaulting to the real CLI-backed implementations. This is the same substitution shape `status_test.go` on the #179 branch uses for `helmStateFunc` (a stub closure over an `input.yaml` state map).
- **Unit tests (testutil golden, per repo policy)** — `pkg/setup/testdata/init-recovery/<case>/` with `input.yaml` carrying `{releaseState, chartVersion, helmOutput transcript, trainerCRs inventory}` and golden `expected.txt` carrying the classification, the chosen action, and the full printed plan. Cases: pinned-deployed-skip, failed-conflict-safe-recovers, failed-conflict-trainjob-exists-refuses, failed-conflict-custom-runtime-refuses, failed-nonconflict-fails-raw, pending-upgrade-conflict-recovers, unknown-state-attempts, recovery-reinstall-fails-once. Fakes follow [`pkg/setup/setup_test.go`](../../pkg/setup/setup_test.go) (`testutil.TestCaseParser`) and the #179 [`status_test.go`](../../pkg/setup/status_test.go) helm-stub pattern; the Kubernetes side uses the fake client builder already in `status_test.go`.
- **envtest conflict fixture (no live cluster)** — envtest runs a real kube-apiserver, so real SSA field ownership is available. A test in `pkg/setup` (envtest wired via `KUBEBUILDER_ASSETS`, as `cmd/integration` does):
  1. server-side-applies a webhook-shaped Secret as field manager `helm` (simulating the first install),
  2. force-applies different `tls.crt`/`tls.key` bytes as field manager `trainer-controller` (simulating the ownership takeover observed in the field),
  3. applies again as `helm` without force and captures the API server's 409 conflict listing the `.data` fields,
  4. feeds that real conflict text (wrapped in Helm's error framing) through `classifyTrainerInstallFailure` and golden-asserts the classification and the recovery decision.

  This pins the classifier to apiserver-generated conflict wording rather than a hand-written string.
- **Docs** — update `docs/cli-reference/setup.md` (recovery behavior, safety gate, printed manual procedure) and the troubleshooting section of the site's setup page.

## Rationale

- **Skip-when-deployed removes the trigger, not just the symptom.** Every `upgrade --install` re-render rolls new webhook cert bytes. Not re-applying a healthy pinned release is the cheapest possible convergence for the common retry, and makes `setup init` idempotent in the ordinary sense.
- **Attempt-then-classify over pre-probing.** A pre-flight `managedFields` probe would have to predict Helm's behavior; attempting once and classifying the real failure cannot be wrong about whether the failure occurs, handles transient non-conflict failures (timeouts, image pulls) naturally by not recovering, and keeps one code path for fresh installs and retries alike.
- **Two agreeing signals before destructive action.** Recovery uninstalls a release and deletes CRDs; requiring both the conflict signature and a `failed`/`pending-*` release state prevents a stray "conflict" substring in an unrelated error from triggering it.
- **Automatic recovery mirrors the field-validated procedure exactly.** The uninstall + 4 CRDs + namespace + pinned reinstall sequence is the only recovery known to work (issue #180); every building block already exists in `pkg/setup` for the `reset` path, so the recovery arm is composition, not new mechanics.
- **The TrainJob/JobSet gate matches the field precondition.** The manual recovery was performed on a cluster with no Trainer workloads; automating it is only defensible under the same precondition, extended to non-Helm-owned runtime CRs because CRD deletion destroys instances.
- **One attempt, loudly.** A single recovery attempt with every action printed keeps the tool debuggable; loops would mask genuinely broken clusters.

## Consequences

- **`setup init` becomes convergent**: from nothing-installed, trainer-failed, trainer-deployed-only, or both-deployed, a re-run reaches the same healthy end state or fails with an exact procedure. The issue #180 wall (identical retry, identical failure, forever) is closed.
- **`setup init` gains a destructive arm.** Even gated and confirmed, it deletes CRDs and a namespace. The safety gate, the interactive re-confirmation, and the one-attempt rule bound the blast radius; the fail-fast path is the default whenever safety is not provable.
- **Anything a user manually placed in `kubeflow-system` is removed by recovery.** The namespace is created and managed by `setup init`; this is documented, and the gate refuses when Trainer-family CRs exist, but arbitrary foreign objects (a user's ConfigMap) are not individually protected. Accepted: mirroring the field recovery beats partial cleanup that leaves the conflicted Secrets behind.
- **The conflict classifier couples to Helm/apiserver error wording.** The envtest fixture pins the apiserver half byte-for-byte and will fail on a wording change; the Helm framing half is matched loosely. Misclassification degrades to the fail-fast path, never to wrong recovery.
- **Pinned-version skip means `--set` drift on the Trainer release is not reconciled** until `kubeflowTrainerVersion` changes. Acceptable: `setup init` passes a fixed value set today, so there is nothing to drift.
- **`helm rollback` remains broken for this chart.** Out of scope — `ncrectl` never invokes rollback, and the recovery path makes it unnecessary.

## Alternatives Considered

- **Helm-level fixes: `--force` or `--take-ownership` (option b).** Rejected. `--force` uses delete-and-recreate/replace semantics that can strand webhook configurations whose `caBundle` no longer matches the recreated Secret, and it does not resolve field ownership on all Helm versions. `--take-ownership` exists only on newer Helm, would silently raise `ncrectl`'s minimum Helm version, and papers over the symptom: the chart still re-rolls cert bytes each render while another manager also writes the Secret, so the write-fight resumes on the next apply. Both also change behavior for every install, not just the broken retry.
- **Always uninstall-and-reinstall on any non-deployed state, unconditionally.** Rejected — turns every transient failure (registry timeout) into CRD deletion, and cannot be gated per-failure-class.
- **Pre-probe Secret `managedFields` as the primary detector.** Rejected — predicts rather than observes the failure, adds an API round-trip to every init, and still needs the attempt path for confirmation. Kept only as diagnostic output.
- **Fail fast only (option c alone), never recover automatically.** Rejected as the sole answer — the recovery is mechanical, field-validated, and composed entirely of code `setup reset` already has; making a CI pipeline hand-execute five printed steps is friction without added safety, given the gate. It remains the fallback arm.
- **Fix the chart upstream (cert-manager or stable cert generation) and wait.** Pursued in parallel as an upstream issue, but rejected as the fix here: `ncrectl` pins `kubeflowTrainerVersion` and must converge with the charts that exist today.
- **Delete only the two conflicted Secrets instead of the release + namespace.** Rejected — leaves the release record `failed` (so `helm upgrade` history stays wedged and #188 keeps reporting unhealthy), and a partial first install may have left other orphans the release record never captured; the field-validated sweep is the only sequence proven to converge.

## Notes

- The four conflicts in the field failure map to `tls.crt` + `tls.key` on each of the two webhook Secrets (Trainer's and the JobSet sub-chart's) in `kubeflow-system`.
- The four CRDs deleted during recovery are `trainjobs.trainer.kubeflow.org`, `trainingruntimes.trainer.kubeflow.org`, `clustertrainingruntimes.trainer.kubeflow.org`, and `jobsets.jobset.x-k8s.io` — the same set `setup reset` already deletes via `deleteCRDsByGroup`.
- This ADR depends on issue #179 / PR #188 (`helmStateFunc`, release-state-aware `setup status`) landing first; the `[deps]` state machine consumes that plumbing unchanged.
- `envtest has no GC controller` (CLAUDE.md) is irrelevant here: the recovery deletes objects explicitly, and the envtest fixture only exercises SSA conflicts, not cascade deletion.

## References

- Issue #180 — `setup init` retry certificate field-ownership conflicts (field evidence and manual recovery).
- Issue #179 / PR #188 — `setup status` Helm release health (`helmStateFunc` plumbing this ADR reuses).
- ADR-064: Helm chart distribution.
- ADR-065: xcalctl Helm install — the decision to drive Helm via CLI subprocess rather than SDK, which shapes the attempt-then-classify design.
- ADR-014: envtest integration tests with golden files — the testing pattern the SSA conflict fixture follows.
- [`pkg/setup/helm.go`](../../pkg/setup/helm.go), [`pkg/setup/setup.go`](../../pkg/setup/setup.go), [`pkg/setup/status.go`](../../pkg/setup/status.go) — the code this ADR modifies.
