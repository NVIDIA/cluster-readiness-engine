# ADR-104: Publish the CRDs as a Separate `cluster-readiness-engine-crds` Chart

> **Status:** Proposed
>
> **Amends:** [ADR-064](064-helm-chart-distribution.md), decision 1, the bullet that puts the CRDs in the chart's `crds/` directory. The rest of ADR-064 stands.

## Context

Issue #357 asks for the NVCRE CRDs to ship as their own Helm chart. It comes from the NVIDIA AI Cluster Runtime (AICR) team. AICR installs the chart directly, without nvcrectl: its registry pins `oci://ghcr.io/nvidia/cluster-readiness-engine` at `v0.6.0` with `ownsCRDs: true`, for its Helm, Helmfile, Flux and Argo CD bundles ([AICR ADR-025](https://github.com/NVIDIA/aicr/blob/main/docs/design/025-nvcre-cluster-certification.md)).

### The problem

Helm installs a chart's `crds/` directory on the first install and never again ([Helm: limitations on CRDs](https://helm.sh/docs/topics/charts/#limitations-on-crds)). All seven NVCRE CRDs live there (`Makefile:64-67`). After a plain `helm upgrade`, the new controller runs against the old schema, and the API server silently prunes every field the old schema lacks. The exceptions are the `x-kubernetes-preserve-unknown-fields` subtrees in Workflow and WorkloadRun. The controller stays Ready, and nothing reports the loss.

This affects nearly every upgrade. Every minor release from v0.5.0 to v0.8.0 changed the CRDs, mostly by adding fields (#353, #444, #495, #503).

| Install path | CRDs updated on upgrade? |
|---|---|
| `nvcrectl setup init` | Yes: it server-side-applies them before every upgrade (`pkg/setup/helm.go:155-167`, issue #145) |
| `helm` or Helmfile | No. The operator applies them by hand (`docs/operations/deployment.md:165`) |
| Argo CD | Yes: it renders `crds/` with `--include-crds` on every sync |
| Flux | Only with `upgrade.crds: CreateReplace`; the default is `Skip` |

Every other consumer rebuilds the fix from #145 on its own. AICR did so in [NVIDIA/aicr#2725](https://github.com/NVIDIA/aicr/pull/2725): an `apply-crds.sh` that runs `helm show crds | kubectl apply --server-side --force-conflicts`. A manual apply must use `--server-side`, because the Workflow (789 KB) and Job (645 KB) CRDs exceed the 256 KiB client-side annotation limit.

### Helm 3 and Helm 4 differ

A prototype on Kind (Notes) found two differences that shape the decisions:

- **Installing a chart's `crds/` over existing CRDs.** Helm 3.19.2 skips existing CRDs. Helm 4.3.1 re-applies them under the shared `helm` field manager. That strips another release's ownership annotations, labels and `helm.sh/resource-policy` until that release next upgrades.
- **Taking over CRDs another writer applied.** Helm 4 applies server-side without forcing conflicts. A server-side apply by `nvcrectl-setup` or `kubectl` leaves that manager co-owning `spec.versions`, and any Helm 4 apply that changes the schema conflicts with it. CRDs created only by Helm, of either version, are recorded under `helm` and do not conflict. Helm 3 never conflicts.

### Why ADR-064 is revisited

ADR-064 put the CRDs in `crds/` because `controller-gen` emits them there and the reference chart (NVSentinel) does the same. Its only statement on currency, "CRD and manager RBAC stay in sync with API changes via `make manifests`", is about the repository, not about clusters after an upgrade. Three of its premises no longer hold:

1. **nvcrectl is not the only installer.** The upgrade gap was fixed only inside nvcrectl (#145). Since then the chart has become a direct install surface, for air-gapped sites and for AICR.
2. **The schema is not stable.** `v1alpha1` gains fields in every minor release.
3. **One signed chart.** ADR-074 signs the chart because it carries the CRDs. A second chart carrying them must join that contract.

The chart path, registry and Trainer dependency in ADR-064 are also out of date. Those are not why it is revisited.

Out of scope: the CRDs' content, nvcrectl's apply for CRDs no release owns, Trainer and JobSet CRDs (ADR-078), and storage version migration.

## Decision

1. **Publish `cluster-readiness-engine-crds` on every release**, at `oci://ghcr.io/nvidia/cluster-readiness-engine-crds`, with the main chart's version. It contains only the seven CRDs. They are rendered from `templates/`, so Helm owns them and `helm upgrade` updates them, including removing fields.

2. **The CRDs survive `helm uninstall`.** Each carries `helm.sh/resource-policy: keep`, controlled by `crds.keep` (default `true`). Deleting a CRD deletes every NVCRE object in the cluster. The chart carries no annotations for other tools (Alternatives, 8).

3. **The main chart carries its CRDs in a conditional `crds` subchart.** The CRDs move to `helm/cluster-readiness-engine/charts/crds/crds/`. The main `Chart.yaml` lists the subchart with `condition: crds.enabled`, and `values.yaml` sets `crds.enabled: true`. This is kube-prometheus-stack's layout.
   - The default behaves exactly like today's `crds/`.
   - `crds.enabled=false` turns the CRDs off in every tool, because it is a value.
   - CRDs-chart users set it. A per-tool skip flag is not enough: AICR cannot set one per component, its Argo CD Applications always include CRDs, and on Helm 4 a missed skip strips the CRDs chart's ownership (Context).

4. **One source.** `make manifests` writes to the subchart. `make helm-package` copies those files into the CRDs chart, so both charts always carry identical CRDs.

5. **Order: CRDs first on upgrade, controller first on rollback.** Both charts release at the same version. Newer CRDs with an older controller are safe while schema changes are additive, because new fields are optional. A release that removes a field the previous controller still writes would have those writes pruned in this window, on upgrade and on rollback alike. So a field is removed from the CRDs only once the controller of the previous release no longer writes it. The reverse, an older schema under a newer controller, is the failure this record prevents.

6. **Adopt existing CRDs once with `--take-ownership`** (Helm 3.17 or later). Existing CRDs carry no release metadata, so without it `helm install` fails with `invalid ownership metadata`.

   On Helm 4, CRDs that nvcrectl, AICR's script or a manual `kubectl apply --server-side` also applied need `--force-conflicts` too. It goes on the takeover and again on the first CRDs-chart upgrade that changes the schema: when that writer's schema matched the chart's, forcing the takeover alone leaves it a co-owner. The writer must be stopped first, or the conflict returns. The flag only reaches the seven CRDs in the CRDs chart's release.

7. **nvcrectl skips its CRD apply when a Helm release owns the CRDs.** If any NVCRE CRD carries `meta.helm.sh/release-name`, `setup init` does not apply the CRDs and prints which release manages them. Otherwise it applies them as today. This keeps `nvcrectl-setup` from re-taking `spec.versions` on clusters that use the CRDs chart. nvcrectl does not install or version-check the CRDs chart (Alternatives, 10).

8. **Sign and attest the CRDs chart like the main chart**, in the same `helm-publish` job, through `attest.yml`, and verify both charts at the release gate (ADR-074).

## Implementation

### Charts

```
helm/cluster-readiness-engine/
├── Chart.yaml           # + dependencies: [{name: crds, version: 0.0.0, condition: crds.enabled}]
├── Chart.lock           # + generated by helm dependency build
├── values.yaml          # + crds.enabled: true
└── charts/crds/
    ├── Chart.yaml       # name: crds
    └── crds/*.yaml      # written by make manifests

helm/cluster-readiness-engine-crds/
├── Chart.yaml
├── values.yaml          # crds.keep: true
└── templates/crds.yaml  # renders files/*.yaml; files/ is staged at package time, not committed
```

`templates/crds.yaml` reads each file with `.Files.Get` and `fromYaml`, so CRD text never passes through the template engine. It adds the `helm.sh/chart`, `app.kubernetes.io/*` labels and the keep annotation, and writes the result with `toYaml`.

The CRD path also changes in:

- `Makefile:64-67`;
- `cmd/integration/integration_test.go:58`;
- `.coderabbit.yaml:27`;
- `CLAUDE.md` and `AGENTS.md`;
- `docs/operations/faq.md:68`;
- comments in `pkg/setup`.

nvcrectl reads the CRDs through `helm show crds`, which includes the subchart, so that code is unaffected. A CRDs chart release Secret is about 500 KB of Helm's 1 MiB limit.

### Build and release

- **`make` targets:**
  - `helm-lint` and `helm-package` cover both charts;
  - `helm-push` writes both digests, adding `CRDS_CHART_DIGEST_FILE`.
- **`release.yml`:** gains an `attest-crds-chart` job, and the release gate verifies both charts.
- **`docs/operations/verifying-artifacts.md`:** gains the CRDs chart's `cosign verify` command. It lands after the first release that publishes the chart, because `docs-verify.yml` runs the page's commands against the newest release.

### nvcrectl

`installHelmRelease` checks the seven CRDs for `meta.helm.sh/release-name` before `fetchChartCRDs`, and skips `applyChartCRDs` when it finds one. The annotation parsing is shared with `jobset_ownership.go`.

### Documentation

- **`deployment.md`:** an "Install with Helm directly" section replaces the clause at line 165. It covers:
  - the commands below and decision 5's order;
  - adoption per decision 6;
  - the Helm 4 conflict error, `conflict with "kubectl": .spec.versions`;
  - a `kubectl --show-managed-fields` check that lists any other owner of `spec.versions`. A failed upgrade changes nothing; it is rerun once with `--force-conflicts` after the check names only retired writers.
- **`NOTES.txt`:** names the CRDs chart.
- **`RELEASE.md`:** both charts.
- **Release notes:** the path inside the packaged chart moves from `crds/` to `charts/crds/crds/`.
- **ADR-064:** gains an "Amended by" note once this record is accepted.

```bash
# New install
helm install nvcre-crds oci://ghcr.io/nvidia/cluster-readiness-engine-crds --version <v> -n nvcre --create-namespace
helm install nvcre oci://ghcr.io/nvidia/cluster-readiness-engine --version <v> -n nvcre --set crds.enabled=false
# Upgrade: CRDs first
helm upgrade nvcre-crds oci://ghcr.io/nvidia/cluster-readiness-engine-crds --version <new> -n nvcre
helm upgrade nvcre oci://ghcr.io/nvidia/cluster-readiness-engine --version <new> -n nvcre
```

### Migration

Nothing changes for an operator who does nothing. The CRDs chart is opt-in, and the main chart's default is unchanged.

| Who | To adopt the CRDs chart |
|---|---|
| Direct Helm or Helmfile | Install it per decision 6, set `crds.enabled=false` on the main chart, then upgrade CRDs first. Helmfile orders the two with `needs:` |
| Flux | The same, with `dependsOn`. Remove `upgrade.crds: CreateReplace` |
| Argo CD | Two steps. First add the CRDs Application and let it sync. Then disable automated pruning on the main Application, set `crds.enabled=false`, confirm that the CRDs Application is the one tracking the CRDs, and re-enable pruning. While both Applications render the CRDs, either may hold Argo CD's tracking for them, whatever the tracking mode, and a main Application that still tracks them when they leave its desired state prunes them, and every NVCRE object with them |
| nvcrectl users | Upgrade nvcrectl to a version with decision 7, then adopt per decision 6 |
| Tooling reading `crds/` from the unpacked chart | Switch to `helm show crds` |

On Helm 4, installing the main chart with `crds.enabled=true` beside the CRDs chart strips the CRDs chart's ownership until its next upgrade. In that window, `helm uninstall` of the CRDs chart deletes the CRDs. The docs make `crds.enabled=false` part of every CRDs-chart install.

Removing the CRDs from the main chart later would be breaking. With the subchart in place, a future record can flip the default after at least two releases of the CRDs chart.

AICR can adopt with its existing CRDs-chart pattern, as for `prometheus-operator-crds`:
- add an `nvcre-crds` component;
- set `crds.enabled: false` and drop `ownsCRDs` on `nvcre`, in the same bundle.

The details belong in AICR's tracker ([NVIDIA/aicr#2525](https://github.com/NVIDIA/aicr/issues/2525)).

### Tests

- **`test/helm`, golden files:**
  - the CRDs chart with `crds.keep` true and false;
  - the main chart with `crds.enabled` true and false.
- **`test/helm`, assertion:** the CRDs chart's CRDs are identical, by name and `spec`, to the subchart's.
- **`pkg/setup`, golden cases:** `setup init` with release-owned and unowned CRDs.

## Rationale

- **It fixes the gap once.** Consumers stop scripting a CRD apply that fails silently when forgotten.
- **It is the pattern consumers already run:** `prometheus-operator-crds` with kube-prometheus-stack, `mariadb-operator-crds`, `slinky-slurm-operator-crds`.
- **A value works everywhere.** Skip flags differ by tool, and a missed one corrupts ownership on Helm 4.
- **It removes fields correctly.** Upgrades and adoption both drop fields the chart no longer has (Notes).
- **No new privileges.** Helm's user already needs cluster-scoped writes, and no in-cluster component gains CRD access (ADR-092).

## Consequences

### Positive

- **Ordinary upgrades keep the CRDs current.** Direct Helm, Helmfile and Flux installs need nothing more than `helm upgrade`.
- **Consumers can drop their scripts.** AICR could retire `apply-crds.sh` for NVCRE, and its current script keeps working meanwhile.

### Negative

- **Two charts** to publish, sign and document.
- **Two releases to order.** The wrong order reproduces today's behavior.
- **Helm 4 migrations from another writer need `--force-conflicts`**, and a missed flag can surface releases later.
- **The CRD path changes inside the packaged chart.**
- **Main-chart-only users remain exposed** to stale CRDs (Notes).

## Alternatives Considered

1. **Document the manual apply.** This is the status quo. Rejected: every consumer repeats it, and it fails silently.
2. **CRDs in the main chart's `templates/`, behind `crds.enabled` (cert-manager style).** Rejected:
   - Defaulting it on breaks existing upgrades on ownership.
   - Defaulting it off collides with `crds/` on fresh installs.

   The chosen subchart keeps the CRDs in `crds/`, so the main release never owns them.
3. **The CRDs chart as a dependency of the main chart.** Rejected: the main release would own the CRDs, with alternative 2's problems.
4. **A pre-upgrade hook that applies the CRDs.** Rejected:
   - it needs a `kubectl` image and a ServiceAccount with CRD write access;
   - Argo CD and Flux run hooks differently.
5. **The controller applies its own CRDs.** Rejected:
   - the manager would need CRD write access, which ADR-092 kept off it;
   - two controller versions in a rolling update would fight over the schema.
6. **Remove the CRDs from the main chart now,** as #357 literally asks. Deferred: it breaks main-chart-only installs. The subchart makes this a later default flip.
7. **Keep top-level `crds/` and have users skip it.** This was the first draft. Rejected: AICR cannot skip per component, and on Helm 4 a missed skip strips ownership.
8. **Argo CD `Prune=false,Delete=false` on the CRDs.** Rejected:
   - shipping them in the chart is not needed to protect the migration; disabling pruning on the main Application for the switch does that;
   - cascade deletion is the Argo CD operator's choice.
9. **Reset `managedFields` before adoption instead of forcing.** Rejected: the API server records the existing fields under `before-first-apply`, which conflicts the same way (Notes).
10. **nvcrectl installs the CRDs chart, or checks its version against the controller.** Deferred: no known cluster runs nvcrectl together with the CRDs chart. Decision 7 is enough to keep the two from conflicting.

## Notes

### Prototype results

The prototype used both charts built from the CRDs at `3e78eef2`, on throwaway Kind clusters with Helm 3.19.2 and 4.3.1. "Other writer" is a `kubectl apply --server-side` as `nvcrectl-setup` or `kubectl`.

| Scenario | Helm 3.19.2 | Helm 4.3.1 |
|---|---|---|
| Upgrade today's release to the subchart layout | CRDs untouched | Same |
| CRDs chart, then main chart with `crds.enabled=true` | Ownership intact | **Ownership stripped** |
| CRDs chart, then main chart with `crds.enabled=false` | Ownership intact | Same |
| Helm created the CRDs; adopt with `--take-ownership` | Succeeds | Succeeds |
| Other writer, different schema; adopt with `--take-ownership` | Succeeds, extra field removed | **Conflict.** Succeeds with `--force-conflicts`, extra field removed |
| Other writer, same schema; adopt, then upgrade to v2 | Succeeds | **Upgrade conflicts**, even after a forced takeover. One forced upgrade fixes it |
| Reset `managedFields`, then adopt without force | Not tested | Conflicts (`before-first-apply`) |

### Other notes

- **A controller-side schema check is deferred.** It would make silent pruning visible on every path, including main-chart-only Helm, and needs its own design.
- **#357 says Flux upgrades CRDs on its own.** It does so only with `CreateReplace`.
- **Not tested:**
  - Flux `CreateReplace` as a prior writer;
  - mixing Helm versions on one CRDs-chart release;
  - the Argo CD migration with automated pruning enabled, which the `deployment.md` guidance is validated against before it ships.

## References

- Issue #357; issue #145
- [NVIDIA/aicr#2525](https://github.com/NVIDIA/aicr/issues/2525), [NVIDIA/aicr#2725](https://github.com/NVIDIA/aicr/pull/2725), [AICR ADR-025](https://github.com/NVIDIA/aicr/blob/main/docs/design/025-nvcre-cluster-certification.md)
- ADR-064 (amended), ADR-065, ADR-074, ADR-078, ADR-092
- [Helm: limitations on CRDs](https://helm.sh/docs/topics/charts/#limitations-on-crds); [Helm 3.17.0](https://github.com/helm/helm/releases/tag/v3.17.0) (`--take-ownership`)
