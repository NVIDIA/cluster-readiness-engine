# ADR-078: Preserve Externally Managed JobSet Across setup init and reset

> **Status:** Proposed

## Context

`nvcrectl setup init` installs Kubeflow Trainer 2.2.1 as Helm release `kubeflow-trainer` in `kubeflow-system`. That chart vendors JobSet as a subchart (`jobset.install` defaults to `true`, `fullnameOverride: jobset`). The chart's own values file documents the constraint we violate:

```yaml
jobset:
  # This must be set to false if jobset controller/webhook has already
  # been installed into the cluster.
  install: true
  fullnameOverride: jobset
```

[`trainerHelmUpgradeArgs`](../../pkg/setup/helm.go) never sets `jobset.install`. There is no Trainer `--set` passthrough. On a cluster that already has JobSet as its own Helm release (the field case: release `jobset` in `jobset-system`), `helm upgrade --install` refuses to adopt the cluster-scoped objects:

```text
invalid ownership metadata; key "meta.helm.sh/release-name" must equal
"kubeflow-trainer": current value is "jobset"
```

[`classifyTrainerInstallFailure`](../../pkg/setup/helm.go) requires `conflict`, `.data.`, and `secret` (ADR-073 decision 2). The ownership transcript contains none of the first two, so it classifies as `failureClassOther` and [`handleTrainerInstallFailure`](../../pkg/setup/setup.go) returns the raw Helm output. Nothing points the operator at `jobset.install=false` or `--skip-phases=deps`.

The damaging half is cleanup. [`uninstallDepsPhase`](../../pkg/setup/setup.go) and [`recoverTrainerRelease`](../../pkg/setup/setup.go) both call `deleteCRDsByGroup(..., jobset.x-k8s.io)` with no owner check. Deleting `jobsets.jobset.x-k8s.io` cascades every JobSet in the cluster, including workloads that have nothing to do with NVCRE. ADR-073 decision 5 also refuses automatic recovery if **any** JobSet instance exists, which would block Trainer recovery forever on a cluster that already runs JobSet for other tenants.

The workaround today is to install Trainer by hand with `--set jobset.install=false`, then `nvcrectl setup init --skip-phases=deps`, and to pass `--skip-phases=deps` on reset as well. Issue #335 is that `setup` must do this itself.

This record does not reopen ADR-073's SSA recovery for webhook Secret `.data` conflicts (issue #180). Skip-when-deployed, attempt-then-classify for that class, no `--force`/`--take-ownership`, TrainJob / non-Helm TrainingRuntime gating, and the one-attempt recovery arm stay as written. What this record supersedes is the assumption that JobSet CRDs are always inside the `kubeflow-trainer` blast radius.

## Decision

1. **Separate controller ownership from CRD provenance.** The published Trainer 2.2.1 archive stores JobSet's CRD at `charts/jobset/crds/jobset.x-k8s.io_jobsets.yaml`. It has no Helm release ownership metadata, and Helm installs `crds/` resources without adding that metadata. An unlabeled CRD therefore does **not** distinguish an external install from an existing bundled install.

   Resolve the JobSet controller mode from the exact Trainer release's stored manifest and live JobSet controller, RBAC, and webhook resources. Use the pinned subchart's resource identities and webhook service references; do not assume the external Helm release is named `jobset`. Read the release manifest before uninstalling it, including failed/partial release state. Values alone are insufficient: `jobset.install=true` does not prove that the release created a pre-existing CRD.

   A live resource belongs to the bundled release only when its `app.kubernetes.io/managed-by` **label** is `Helm` and its `meta.helm.sh/release-name` and `meta.helm.sh/release-namespace` **annotations** are `kubeflow-trainer` and `kubeflow-system`. A same-named release in another namespace is external. Missing CRD annotations never override positive bundled controller/release evidence.

   | State | Meaning |
   |---|---|
   | `absent` | No JobSet CRD, controller resources, or bundled release manifest entries exist |
   | `bundled` | The Trainer release manifest or its live JobSet resources identify the bundled controller, with no conflicting external ownership |
   | `external` | External controller ownership is established, or a CRD exists with no bundled release/controller evidence after successful inspection |
   | `unknown` | Required reads fail or release/live-resource evidence conflicts |

   A CRD-only installation is conservatively reused as external, with a warning that controller availability has not been established. Failed reads are not equivalent to absent resources. The implementation must test the detector against the published chart's actual resource identities and an unlabeled CRD, not fixtures that invent CRD ownership labels.

2. **Choose the subchart value before mutating the Trainer release.** Preserve skip-when-deployed at the pinned version; resolve ownership for every install or upgrade that would actually run.

   | State | `jobset.install` |
   |---|---|
   | `absent` or `bundled` | Keep enabled; omit the override |
   | `external` | Pass `--set jobset.install=false` |
   | `unknown` | Stop before Helm mutation; print the unavailable or conflicting evidence |

   Never disable a subchart already present in the Trainer release merely because its CRD lacks annotations: upgrading with `false` can remove the controller. Mixed bundled/external evidence requires operator reconciliation rather than guessing. `jobset.controller.tolerations[0].operator=Exists` may remain; it is unused with the subchart disabled.

   Capture the selected mode before recovery uninstalls the release and reuse it for reinstall. Revalidate conflicting live ownership before destructive operations; do not classify a retained bundled CRD as external solely because recovery has just removed its controller and release record. An independent init after a completed reset may see a CRD-only installation and require operator cleanup or restoration of the external controller; it must warn rather than claim that a controller was detected.

   This pre-probe concerns observable installation ownership, not prediction of ADR-073's SSA Secret conflicts.

3. **Retain JobSet CRDs in reset and recovery.** Neither a bundled controller nor a release manifest proves that NVCRE originally created the shared CRD: Helm can reuse an existing CRD. Existing releases have no trustworthy creation record. Remove the unconditional JobSet `deleteCRDsByGroup` calls and preserve the CRD in every ownership state, including bundled installs. Do not retrofit ownership metadata onto existing CRDs or infer deletion permission from controller ownership.

   This deliberately changes the original proposal's bundled-cleanup behavior: positive creation provenance is unavailable, so automatic CRD deletion is deferred. A future design could record creation provenance, but that is not part of this fix. Trainer CRD cleanup remains unchanged.

   `printRetainedResources` reports the retained JobSet CRD and explains that deleting it destroys JobSets across all namespaces. Recovery plans and manual recovery instructions always omit JobSet CRD deletion. A standalone manual cleanup command must be described as an operator decision after inspecting cluster-wide use, not as a required recovery step. Helm uninstall still removes bundled controller resources; preservation of the CRD does not promise preservation of a bundled controller.

   **Reset inspection contract:** reset does not need the JobSet ownership detector or the recovery namespace inventory to authorize its existing Trainer release uninstall. It uninstalls only the configured `kubeflow-trainer` release in `kubeflow-system`, leaves that namespace intact, and never deletes JobSet CRDs. Ownership-inspection failures therefore do not block reset or trigger fallback cleanup. The retained-resource lookup is best effort: a failed lookup prints that the JobSet CRD *may remain*, with the lookup error, rather than claiming it is absent. Helm uninstall and Trainer CRD cleanup failures still propagate normally. `--skip-phases=deps` continues to bypass dependency removal. This is an explicit reset operation, not automatic recovery; the recovery safety gate does not apply to it.

4. **Gate recovery on every resource its namespace deletion could remove, as well as JobSet controller disruption.** Retaining the CRD alone does not make release uninstall or namespace deletion safe. Before uninstalling Trainer or deleting `kubeflow-system`:

   - Continue to block on TrainJobs and non-Helm TrainingRuntime/ClusterTrainingRuntime instances as ADR-073 requires.
   - When the bundled JobSet controller would be uninstalled, any JobSet instance blocks automatic recovery, even though its CRD is retained.
   - When JobSet is external, instances outside `kubeflow-system` do not alone block recovery. Any JobSet instance inside `kubeflow-system` does block it because namespace deletion destroys that instance.
   - Verify that no external JobSet controller or supporting resource resides in `kubeflow-system`. Inspect namespaced resources and their ownership, including webhook service targets. Refuse namespace deletion when foreign or unclassified resources could be removed; do not rely only on default controller names or labels. Inventory every discoverable, listable namespaced resource type, not just JobSet kinds. Unrelated foreign objects such as a user ConfigMap also block recovery.
   - Classify namespace inventory against the exact Trainer release manifest and live ownership. Permit a release resource only when its identity and complete Helm ownership agree. For controller-created objects such as ReplicaSets and Pods, follow owner references by UID to a verified release resource; a copied label or a dangling, cyclic, or mismatched owner reference is not sufficient. Unknown descendants block recovery. This inventory permission never overrides the TrainJob, runtime, or JobSet blockers above.
   - Use a small, explicit allowlist for resources that are expected outside the rendered manifest: the namespace's default ServiceAccount and `kube-root-ca.crt` ConfigMap, the exact Trainer release's Helm storage records, and controller-generated Events, EndpointSlices, and leader-election Leases whose relationship to verified release resources is established. Define a predicate per kind using its expected identity, content, and ownership/controller evidence; a name or namespace alone is not permission. Unexpected custom fields or references, unrelated release records, and anything outside these predicates block recovery. Add further exceptions only with a concrete fixture and rationale; do not blanket-allow Secrets, ConfigMaps, Events, or Leases.
   - Failed discovery, incomplete namespace inventory, or unresolved ownership blocks automatic recovery. `--auto-approve` bypasses the prompt, not these checks.

   Apply the gate before the first destructive recovery operation. On refusal, report the blocking resources and do not print an unconditional namespace-delete command as a safe workaround. Manual guidance must preserve the same boundaries. Reset does not delete `kubeflow-system` today; this change must not add namespace deletion to reset.

5. **Provide narrowly matched ownership guidance on every install failure path.** Add `failureClassJobSetOwnership` only when the same Helm error identifies `invalid ownership metadata` and a concrete JobSet resource kind/name from the subchart or independently observed JobSet resources. A bare ownership phrase, unrelated Trainer resource, or incidental occurrence of the word JobSet is insufficient. Keep unmatched ownership errors generic and retain the original transcript.

   Route captured failures from both `trainerActionInstall` (including fresh and unknown-release states) and `trainerActionAttemptRecover` through a common diagnostic step. Do not accidentally enable SSA recovery for fresh installs: ownership diagnostics are shared, while the existing ADR-073 recovery eligibility remains unchanged. Recovery reinstall failures also receive these diagnostics without starting another recovery attempt.

   For the JobSet ownership class, report the conflicting object and explain the external-JobSet path: install Trainer with `jobset.install=false`, then `setup init --skip-phases=deps`. Also document `setup reset --skip-phases=deps` for affected older CLI versions. If evidence is ambiguous, describe the workaround as conditional on confirming external ownership; do not assert that the conflicting object necessarily belongs to another release. Never uninstall Trainer, delete CRDs, or delete the namespace in response to this error class. Match this class before considering SSA recovery.

6. **No generic Trainer values passthrough or JobSet version gate.** Detection and the targeted subchart override are the interface. External controller compatibility remains the operator's responsibility; document that the controller must support the JobSet API and features required by Trainer 2.2.1. A present CRD alone does not prove controller health or compatibility.

## Implementation

- **Ownership observation:** add a shared observation structure containing controller mode, evidence, and inspection errors. For init and recovery, query the exact Trainer release manifest and relevant live resources before mutation. Reset does not depend on this detector; its CRD-retention rule is unconditional. Keep CRD existence separate from controller ownership; require the full release-name/namespace identity for live bundled resources. Preserve the observation across recovery, with conflict checks before destructive actions.
- **Helm arguments and diagnostics:** extend `trainerHelmUpgradeArgs` and the injected `trainerHelm.install` operation to carry the selected mode. Add the narrow ownership classifier and common failure diagnostics without broadening ADR-073 recovery eligibility or retry count.
- **Cleanup and recovery:** remove automatic JobSet CRD deletion in reset and recovery. Reset keeps the existing exact-release uninstall and Trainer CRD cleanup; JobSet retained-resource lookup failures produce an unverified warning, not a reset failure. Recovery inventories all namespaced resource types before uninstall, including unrelated foreign resources, and applies the manifest/ownership checks, UID owner-reference traversal, and explicit per-kind exceptions in decision 4. Missing discovery/list permissions or unclassifiable resources block recovery. Keep workload blockers independent of inventory exceptions. Make recovery plans, manual guidance, and retained-resource output agree with actual actions.
- **Documentation:** update `docs/cli-reference/setup.md`, `docs/getting-started/install.md`, and `docs/operations/deployment.md` for detection, ambiguous-state refusal, retained CRDs, reset/re-init behavior, and namespace safety. ADR-073 links here as a proposed amendment until this ADR is accepted.

### Validation

Use `testutil.TestCaseParser` where it fits existing package tests, with realistic chart-derived fixtures:

| Area | Required cases |
|---|---|
| Ownership | Fresh absence; legacy bundled controller with unlabeled CRD; external Helm controller; same release name in another namespace; non-Helm controller; CRD-only installation; partial bundled release; mixed ownership; failed reads |
| Install/upgrade | Fresh and bundled keep subchart enabled; external disables it; unknown stops before mutation; pinned deployed release remains skipped; bundled upgrade never disables JobSet merely because CRD annotations are missing |
| Reset | Bundled, external, unlabeled, and uncertain CRDs are retained; external controller resources remain; ownership detector is not required; failed retained-CRD lookup prints “may remain” and does not block reset; actual uninstall/Trainer cleanup failures propagate; namespace is never deleted; `--skip-phases=deps` still bypasses removal |
| Recovery | Bundled JobSets block controller removal; external JobSets elsewhere do not alone block; JobSets or external controller/support resources in `kubeflow-system` block before uninstall; incomplete inventory blocks; selected mode survives uninstall/reinstall; CRD is retained |
| Namespace inventory | Unrelated foreign ConfigMap blocks before uninstall; verified release resources and UID-linked ReplicaSet/Pod descendants pass inventory checks; dangling/cyclic/wrong-UID owner references block; expected default ServiceAccount, root-CA ConfigMap, exact-release storage records, and verified controller Events/EndpointSlices/Leases pass their predicates; customized or unrelated lookalikes block; discovery/list failure blocks; inventory exceptions never bypass workload blockers |
| Diagnostics | Realistic JobSet ownership error gets guidance on fresh install, retry, and recovery reinstall; unrelated ownership errors and bare phrases stay generic; ownership errors never arm cleanup; fresh SSA failure still cannot enter retry-only recovery |
| Plans/docs | Printed recovery and manual procedures omit CRD deletion and respect namespace blockers; reset/re-init with a retained CRD reports controller uncertainty |

Render the published Trainer 2.2.1 chart with JobSet enabled and disabled to verify resource identities and subchart behavior. Include a Helm-backed lifecycle check before considering implementation complete: start with bundled JobSet and its actual unlabeled CRD, exercise the upgrade, and verify the controller stays installed. Verify the external-release path preserves its controller and CRD through init/reset. Fake-client tests establish decisions and explicit deletion boundaries; they do not establish Helm metadata behavior or Kubernetes namespace garbage collection.

Review existing golden diffs before requesting permission to regenerate them. After implementation, run the repository's required verification suite and documentation link checks.

## Rationale

- **Controller ownership and CRD creation are different facts.** The actual chart and Helm CRD installation behavior invalidate a CRD-label-only detector. Release/controller evidence protects legacy bundled upgrades; retaining CRDs avoids inventing provenance.
- **Complete release identity prevents cross-namespace adoption.** Helm release names are namespace-scoped even when their resources are cluster-scoped.
- **Ambiguity must not change controller ownership.** Neither enabling a second controller nor removing a bundled one is a safe default after inspection fails.
- **Recovery safety follows all destructive actions.** Release uninstall can remove the controller; namespace deletion can remove foreign resources even when the shared CRD remains.
- **Targeted diagnostics preserve ADR-073.** Ownership errors get useful guidance on first install as well as retry, without widening the conditions that authorize automatic cleanup.

## Consequences

- Clusters with an identifiable external JobSet can install Trainer without manually disabling the subchart. Conflicting or unreadable ownership fails with evidence before mutation.
- Legacy bundled installs remain bundled on upgrades despite unlabeled CRDs.
- Reset and recovery retain JobSet CRDs, including bundled ones. Full removal becomes an explicit operator cleanup step. After reset, a retained CRD without a controller is reported as such; it is not proof of a healthy external installation.
- External JobSets outside the recovery namespace can continue using their external controller while Trainer is recovered, provided namespace inspection proves recovery will not remove that controller or its supporting resources.
- External controller health and version compatibility are not certified by this detector.
- Additional release and Kubernetes reads are required. Inability to obtain required evidence results in a diagnostic refusal rather than an inferred ownership decision.

## Alternatives Considered

- **CRD Helm metadata as the sole ownership signal.** Rejected: the published CRD lacks it, and Helm's CRD path does not add it. It would misclassify existing bundled installs.
- **Delete a CRD whenever the controller is bundled.** Rejected: Helm may have reused an existing CRD. Controller ownership does not prove CRD creation or exclusive use.
- **Introduce a new CRD creation marker now.** Deferred: it would need durable, race-safe provenance and migration semantics; it cannot establish ownership retroactively for existing installs. Retention provides the safe behavior for this fix.
- **Generic Trainer `--set` / `--values` passthrough.** Rejected as the primary fix: it leaves cleanup ownership and recovery safety unresolved.
- **Look for a Helm release named `jobset`.** Rejected: external release names and namespaces vary.
- **Always disable JobSet, or use Helm `--take-ownership`.** Rejected: the former breaks bundled lifecycle behavior; the latter adopts resources managed elsewhere.
- **Continue after ownership inspection fails.** Rejected: defaulting the subchart value can either collide with external resources or remove bundled resources.
- **Ignore all external JobSets during recovery.** Rejected: namespace-local instances and controller resources can still be destroyed.

## Notes

- `fullnameOverride: jobset` explains collisions among templated controller/RBAC/webhook resources. Their Helm ownership metadata must not be confused with metadata on the separately installed CRD.
- This ADR remains proposed. Its changes to ADR-073 take effect only after acceptance and implementation.

## References

- [Issue #335](https://github.com/NVIDIA/cluster-readiness-engine/issues/335) — external JobSet installation and cleanup failure.
- [ADR-073](073-setup-retry-convergence.md) — existing Trainer recovery; this proposal amends JobSet cleanup and safety gating while retaining SSA recovery eligibility.
- Issue #180 — webhook Secret field-ownership recovery.
- Issue #321 — overridable Helm chart references.
- Published chart `oci://ghcr.io/kubeflow/charts/kubeflow-trainer:2.2.1`, digest `sha256:8e8e3de0257be804f60dbc8e6784eaddb64b2e158a46cade30ca18b6030183b1`; inspect with `helm show crds` and `helm pull`.
- [Helm 4.2.4 CRD installation source](https://github.com/helm/helm/blob/v4.2.4/pkg/action/install.go) — `installCRDs` creates CRDs separately from templated release resources.
- [`pkg/setup/helm.go`](../../pkg/setup/helm.go), [`pkg/setup/setup.go`](../../pkg/setup/setup.go).
