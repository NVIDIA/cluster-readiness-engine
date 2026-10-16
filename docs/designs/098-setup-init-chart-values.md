# ADR-098: `nvcrectl setup init` Takes Chart Values and Keeps Them Across Runs

> **Status:** Proposed

> **Supersedes:** the `setup init` contract in ADR-092's amendment that "every other value still returns to its default, as before"

## Context

GitHub issue #504 reports `nvcre-manager` v0.7.0 being OOMKilled on a 2,678-node GB300 cluster with about 80,000 pods. The chart requests and limits the manager at 1Gi (`helm/cluster-readiness-engine/values.yaml:34-40`). On that cluster the manager peaked at 3.83 GiB during its initial cache sync and settled at 3.1–3.8 GiB while idle, so at 1Gi it was killed about 15 seconds after every start. An automated qualification harness installed NVCRE the usual way, with `nvcrectl setup init --auto-approve` whenever `nvcrectl setup status` reported a missing component. The run went like this:

1. `setup init` printed `NVCRE v0.7.0 initialized successfully.` and exited 0, because `helm --wait` happened to see the restarting pod Ready.
2. Fifteen seconds later `setup status` reported `missing: nvcreController`.
3. The harness ran `setup init` again, and it failed with `UPGRADE FAILED: context deadline exceeded`.

Raising the limit to 8Gi with `helm upgrade` fixed the manager on the same release. That fix cannot be made durable today, for two reasons.

**`setup init` cannot pass chart values.** Its flags (`pkg/setup/setup.go:152-168`) are `--skip-phases`, `--image`, `--image-pull-secret`, `--chart-ref`, `--trainer-chart-ref`, `--auto-approve`, `--version` and `--rbac-scope`. The only values it gives Helm are the controller image, the pull secret binding and `rbac.*` (`pkg/setup/helm.go:169-176`, `:187-205`). An operator who needs another value has two options. They can upgrade the release with Helm after `setup init`, or skip `setup init` and install the chart with Helm (`docs/operations/deployment.md`). The same gap appears in the mirror guide: `setup init` "has no flag to bind a custom-named pull secret today", so mirror users must bypass it (`docs/operations/deployment.md:115`).

**`setup init` resets what the operator set.** Helm reuses a release's earlier values only when an upgrade supplies none, and `setup init` always supplies the image (ADR-092, amendment). Every run therefore starts from the chart defaults. ADR-092 carved out `rbac.*`: `setup init` reads the installed release's own values with `helm get values -o json` and passes `rbac.scope` and `rbac.runNamespaces` back (`pkg/setup/helm_rbac.go:120-153`). It kept the contract for everything else on purpose: "Every other value still returns to its default, as before." It rejected `--reset-then-reuse-values` because it "would carry every earlier value forward, changing `init`'s contract for values unrelated to this record, and needs Helm 3.14 or later." The CLI reference states the contract (`docs/cli-reference/setup.md:73`): "Every other chart value returns to its default on each run."

Under that contract, the 8Gi limit lasts until the next `setup init`. The harness runs `setup init` exactly when the controller is reported not ready, so one restart at the wrong moment returns the manager to 1Gi, where it is killed on every start.

Three more problems surface once `setup init` accepts and keeps values:

- **The controller image is set on every run.** With no `--image`, `setup init` uses the CLI's own version (`pkg/setup/setup.go:213-219`). An operator's `--set manager.image.tag=…` would be passed first and silently overridden by nvcrectl's own `--set`, which comes last and wins.
- **The pull secret is written to list index 0.** `--image-pull-secret` binds the secret with `--set manager.imagePullSecrets[0].name=nvcrectl-pull-secret` (`pkg/setup/helm.go:202`). If the installed release's list were kept, that write would replace whatever was at index 0, such as an operator's mirror secret. The Deployment would roll out a pod that cannot pull, and the run would fail as `UPGRADE FAILED: context deadline exceeded`, with nothing naming the cause.
- **`--image` with a digest renders an invalid image reference.** `parseImage` returns the digest of `repo@sha256:…` as the tag (`pkg/setup/setup.go:1683-1687`). `setup init` then passes `manager.image.tag=sha256:…`, and the chart renders `repo:sha256:…` (`helm/cluster-readiness-engine/templates/deployment.yaml:71-75`). Nothing tests this path.

### Scope

This record fixes five things:

- **`setup init` takes chart values** for the NVCRE release.
- **It keeps the values set on the installed release** on later runs.
- **It no longer silently overrides an operator's image** with its own.
- **It no longer overwrites list index 0 of `manager.imagePullSecrets`.**
- **`--image` with a digest produces a valid reference.**

It does not change:

- **The manager's memory footprint.** It grows with the cluster-wide Pod count because Pods are cached unscoped (`pkg/controller/cache.go`). Issue #504 tracks that.
- **Any chart default,** including `manager.resources` and the tolerate-all `manager.tolerations` from ADR-051.
- **The success check** after `helm --wait`.
- **Sizing:** `setup init` gives no warning or advice about memory.
- **The Kubeflow Trainer release's values.**
- **The `--image-pull-secret` flag** keeps its name and keeps taking the token as an argument.

## Decision

1. **`setup init` takes chart values for the NVCRE release.** It gains three repeatable flags, `--values`/`-f <file>`, `--set key=value` and `--set-json key=json`. They have Helm's syntax and Helm's precedence: values files apply in the order given, `--set-json` overrides them, and `--set` overrides both. They apply to the NVCRE release only, not to Kubeflow Trainer. Giving any of them, or `--reset-values` (decision 2), with `--skip-phases` including `helm` is an error, since the phase that would use them does not run.

2. **`setup init` keeps the values set on the installed release.** This supersedes ADR-092's "every other value returns to its default" contract. Each value is resolved in this order, lowest first:

   > chart defaults → values set on the installed release → this run's `--values` files → this run's `--set-json` → this run's `--set` → values nvcrectl sets itself (the image, decision 4, and the values of flags given on this run)

   "Values set on the installed release" means what `helm get values -o json` returns: the values the release was installed or upgraded with, not the chart defaults. A release Helm has no record of is a first install, so nothing is kept. Any other failure to read the values stops `setup init`, as ADR-092 already does for `rbac.*`. Two exceptions:
   - `manager.image` is never kept (decision 4).
   - A new flag, `--reset-values`, drops the kept values and starts from the chart defaults, except `rbac.scope` and `rbac.runNamespaces`. Those are still kept, so a reset never silently widens a namespaced grant (ADR-092). An operator changes them with `--rbac-scope` or `--set`.

3. **A value that has its own flag is set by the flag or by a value, not both.** On a single run, the operator may give a flag or set its value with `--values`, `--set` or `--set-json`, but not both. Setting a parent counts: `--set-json 'manager.image={…}'` or `--set-json 'manager={…}'` sets the image too. A value kept from the installed release never conflicts with a flag, because the flag is a deliberate change on this run.

   | Value | Flag | When the operator gives both |
   |---|---|---|
   | `manager.image` (`repository`, `tag`, `digest`) | `--image` | Rejected; the error names `--image` |
   | `rbac.scope` | `--rbac-scope` | Rejected; the error names `--rbac-scope` |
   | `manager.imagePullSecrets` | `--image-pull-secret` | Combined (decision 5) |

   When the operator gives neither, the default applies. For the image that is the CLI-version image (decision 4). For `rbac.scope` and the pull secrets it is the installed release's value, or the chart default on a first install.

4. **The controller image is worked out on every run and never kept.** The installed release's values always contain the image nvcrectl set on an earlier run, and those cannot be told apart from an image an operator chose. So `manager.image` is dropped from the kept values, and each run uses the first of these:
   - **The operator set any key under `manager.image` on this run.** nvcrectl sets no image values, and the chart fills the rest. An unset tag defaults to the chart's `appVersion`. A published chart's `appVersion` is its release version (`Makefile:306`), which is also the controller image's tag, so `--set manager.image.repository=<mirror>/manager` alone is enough for a mirror.
   - **`--image` was given.** A `repo:tag` reference sets `manager.image.repository` and `manager.image.tag`. A `repo@sha256:…` reference sets `manager.image.repository` and `manager.image.digest`. This fixes the invalid reference described in Context.
   - **Neither.** The CLI-version image, as today.

   Because the image is never kept, a digest pinned on an earlier run cannot hold the controller on an old image after the chart and CRDs move to a new version.

5. **`--image-pull-secret` adds its secret to the list rather than replacing it.** When the flag is given, `manager.imagePullSecrets` is built as follows:
   - Start from this run's list if the operator set one, otherwise the installed release's list, otherwise an empty list.
   - Add `{name: nvcrectl-pull-secret}` unless an entry with that name is already there.
   - Pass the whole list with one `--set-json manager.imagePullSecrets=…`, after the operator's flags. The `[0]` index write is removed.

   The operator's entries keep their order and positions, so an operator `--set manager.imagePullSecrets[0].name=…` that Helm applies after the `--set-json` writes the same value again. Without the flag, nvcrectl passes nothing for this value, and Helm uses the operator's list or the kept one. The flag's other two jobs are unchanged: creating the `ghcr.io`-scoped `nvcrectl-pull-secret` Secret, and logging Helm in to GHCR for chart pulls.

6. **Values are checked and shown before anything changes.** Preflight does three things before the confirmation prompt (`pkg/setup/setup.go:238-240`), and before the deps phase installs anything:
   - Reads the installed release's values.
   - Works out what the operator passed on this run.
   - Applies decision 3, so a conflict stops `setup init` with the cluster unchanged.

   It then prints what the NVCRE release will be given, as in the example below. The printout appears with `--auto-approve` too, so a CI log records what was applied. Lists are printed whole at their path, never flattened into indices.

   ```text
   [preflight] Values kept from the installed release:
     manager.resources.limits.memory=8Gi
     rbac.scope=namespaced
   [preflight] Values from this run (--values, --set, --set-json):
     manager.resources.requests.memory=4Gi
   [preflight] Controller image: ghcr.io/nvidia/cluster-readiness-engine/manager:v0.8.0 (CLI version)
   [preflight] Image pull secrets: [mirror-pull-secret, nvcrectl-pull-secret] (installed release, --image-pull-secret)
   ```

## Implementation

### Reading the operator's values

Decisions 3 to 5 need the operator's values as data. To get them, nvcrectl renders a probe chart with the operator's own flags: `helm template` on a chart that has no `values.yaml` and a single template, `{{ toJson .Values }}`. Helm parses the flags exactly as it will for the upgrade, including the `--set` syntax (comma-separated pairs, `{a,b}` lists, escaped dots, list indices, `null`). The probe chart is two small files written to a temporary directory created with mode 0700. The probe needs no cluster access, and it runs only when the operator passed `--values`, `--set` or `--set-json`.

The probe's output is used only to detect conflicts, to decide the image and to build the pull-secret list. The upgrade receives the operator's original flags unchanged, so Helm's own merge semantics, including `null` removing a chart default, apply exactly as the operator wrote them.

### Building the upgrade

`nvcreHelmUpgradeArgs` assembles `helm upgrade --install` in this order:

1. The fixed arguments, as today: release, chart, namespace, `--create-namespace`, `--version`, `--wait`, `--timeout`.
2. `-f <kept values>`, when the installed release has any. This is a temporary file written with mode 0600 and removed when Helm returns. It holds the release's values minus `manager.image`, and with `--reset-values`, only `rbac.scope` and `rbac.runNamespaces`.
3. The operator's `--values`, `--set-json` and `--set` flags, in the order given.
4. nvcrectl's own values:
   - the image values from decision 4, when it sets them
   - `--set-json manager.imagePullSecrets=…` from decision 5, when `--image-pull-secret` is given
   - `--set rbac.scope=…`, when `--rbac-scope` is given

Decision 3 guarantees that nvcrectl's values and the operator's never name the same key on one run, so step 4's position matters only against the kept file, which it overrides. ADR-092's dedicated carry of `rbac.*` folds into step 2. `pkg/setup/helm_rbac.go` keeps `--rbac-scope` validation and the "Setting rbac.scope" message.

Everything `setup init` passes already works on Helm 3.10, the release that added `--set-json` (nvcrectl already passes `--set-json` for `rbac.runNamespaces`, `pkg/setup/helm_rbac.go:98`). This record adds no new Helm version requirement.

### Files

- `pkg/setup/setup.go`:
  - add the flags `--values`/`-f`, `--set`, `--set-json` (string arrays) and `--reset-values`
  - reject the value flags and `--reset-values` when the `helm` phase is skipped
  - move the reading of the installed release's values and the probe into preflight
  - make the phase summary (`initPhaseSummary`, `:1101-1104`) name the image's source
- `pkg/setup/helm_values.go` (new):
  - read the installed release's values (generalized from `readReleaseRBACValues`)
  - run the probe chart
  - detect conflicts
  - decide the image
  - merge the pull-secret list
  - write the kept-values file
  - print the summary
- `pkg/setup/helm.go`: `nvcreHelmUpgradeArgs` takes the resolved inputs and builds the order above; the `imagePullSecrets[0]` write is removed.
- `pkg/setup/helm_rbac.go`: only the `--rbac-scope` handling remains.

### Documentation

- **`docs/cli-reference/setup.md`:**
  - document the new flags
  - add a "Values across runs" section covering decisions 2 to 5
  - replace "Every other chart value returns to its default on each run."
- **`docs/operations/deployment.md`:**
  - show setting chart values through `setup init`
  - rewrite the mirror pull-secret guidance (`:115`): a custom-named secret no longer requires bypassing `setup init`
  - state in the resource requirements section (`:157-178`) and the large-cluster tuning table (`:367`) that manager memory follows the cluster-wide Pod count, not the node count, with the issue #504 data point (3.83 GiB peak at about 80,000 pods), and show how to set it through `setup init`
- **`CHANGELOG.md`:**
  - under Added: the new flags
  - under Changed: `setup init` now keeps values set on the installed release
  - under Fixed: `--image` with a digest

### Testing plan

`pkg/setup` permits table tests. Helm is faked with a shell script, as `TestReadReleaseRBACValues` does (`pkg/setup/helm_rbac_test.go:104`), except where noted.

- **Kept values:**
  - no release (nothing kept)
  - a release with no values
  - partial `manager.resources` (kept as written, so Helm deep-merges it with the defaults)
  - `null` entries (kept)
  - `manager.image` (dropped)
  - with `--reset-values`, only `rbac.*` kept
  - an unreadable release (error)
- **Conflicts:** `--image` with `--set manager.image.tag`, with `-f` setting `manager.image.repository`, and with `--set-json manager={…}` containing an image (all rejected); `--rbac-scope` with `--set rbac.scope` (rejected); `--set rbac.scope` alone (allowed); the value flags or `--reset-values` with `--skip-phases=helm` (rejected).
- **Image:** operator-set (nvcrectl sets nothing), `--image repo:tag`, `--image repo@sha256:…` (sets `digest`, not `tag`), the CLI default, and a digest in the installed release that is not kept.
- **Pull secrets:** each combination of flag, operator list and kept list, including an operator list that already holds `nvcrectl-pull-secret`; check that the result is passed as one `--set-json` and never as `[0]`.
- **Argument order:** the kept file comes before the operator's flags, which come before nvcrectl's own values.
- **Probe, against real Helm:** use `requireHelm` from `test/helm/render_test.go:105`, which fails in CI without Helm and skips locally. Cover comma-separated `--set` pairs, lists, escaped dots, `null`, and the precedence `-f` < `--set-json` < `--set`.
- **Output:** the summary lines, with and without `--auto-approve`.

## Rationale

- **It closes the #504 failure with no change to how a harness calls `setup init`.** A harness that pins an nvcrectl version must move to a release that includes this change. After that, once an operator sets the memory, a later `setup init` with no flags, which is what the harness runs, keeps it. The flags also let an operator or a harness set it through nvcrectl in the first place.
- **Chart values are already the documented interface** (`docs/operations/deployment.md`, key chart values). Accepting them in Helm's syntax costs operators nothing new to learn, and it covers values beyond memory, such as placement and the mirror pull secret, without a flag for each.
- **Keeping every value matches what an operator expects.** Once `--set` exists, a value that survives some runs and not others, depending on its key, is a trap. ADR-092 carried `rbac.*` because a reset there was unsafe. #504 shows a reset of `manager.resources` is just as harmful.
- **Doing the carry-forward in nvcrectl, not with a Helm flag,** is what makes the exceptions possible: dropping `manager.image` and merging the pull-secret list. It also works on any Helm version that supports `--set-json`.
- **The probe chart reuses Helm's parser.** Values are interpreted by the same Helm binary that runs the upgrade, so detection and installation cannot disagree.
- **Checking in preflight** means a rejected run changes nothing on the cluster, including the Trainer release the deps phase would otherwise install first.

## Consequences

- **Values set with `helm upgrade` now survive `setup init`.** This is intended, and it is a change of contract. An operator who relied on `setup init` to restore the chart defaults uses `--reset-values`.
- **A kept value outlives a change of chart default.** If a later chart raises a default the operator had pinned, the pin wins. The chart has no `values.schema.json`, so a kept key that a later chart removed is ignored silently rather than failing the upgrade. `--reset-values` clears both cases.
- **Releases installed by earlier `setup init` versions upgrade cleanly.** Their stored values are the image, which is dropped; `manager.imagePullSecrets[0]`, which is kept and merged; and `rbac.*`, which was already kept. Values an operator added with `helm upgrade`, such as the #504 workaround's memory and tolerations, are kept from the first run of the new version.
- **`setup init` runs one more local Helm command** (`helm template`) when value flags are given. It needs no cluster access.
- **The installed release's values are read before the confirmation prompt** rather than in the helm phase. A cluster where `helm get values` fails now stops `setup init` before the deps phase, where before it failed after Trainer was installed.
- **An operator who wants the GHCR chart login and their own pull secrets can now use both,** since the lists are combined rather than one rejected.

## Alternatives Considered

### Raise the chart's memory defaults

**Rejected.** The request is reserved on every cluster, and the size that works depends on the cluster's Pod count. A default that fits about 80,000 pods over-reserves on small clusters, including the Kind and KWOK UAT lanes, and still fails on a larger one. Operators need to set the value either way. Raising only the limit costs small clusters nothing; whether to do that is a separate decision.

### Size the manager automatically from the cluster

**Rejected.** `setup init` would compute a limit once, at install, from a Pod count that changes. The formula would tie the CLI to the controller's cache layout, which issue #504 is likely to change. A warning in preflight is a possible follow-up; a computed value is not.

### Dedicated flags such as `--manager-memory`

**Deferred.** Each new need would add a flag. A dedicated flag can be added later on top of `--set`.

### Helm's `--reuse-values` or `--reset-then-reuse-values`

**Rejected.**
- `--reuse-values` ignores the new chart's defaults on upgrade.
- `--reset-then-reuse-values` needs Helm 3.14 or later.
- Both carry `manager.image` forward, including a pinned digest that would outrank the new tag.
- Neither can combine the pull-secret list.

### Carry forward only `manager.resources`

**Rejected.** It would fix #504 alone, but once `--set` exists every value the operator sets should behave the same way. Otherwise the next value someone tunes resets just as memory did.

### Let the flag or the value win silently

**Rejected.** Whichever loses, the operator asked for something and did not get it, and nothing says so.

### Reject `--image-pull-secret` when a list is present, instead of combining

**Rejected.** It needs the same reading of the kept list and the operator's values as combining does, and it gives a worse result. A pod can name several pull secrets, so there is nothing to choose between.

### Parse `--set` in Go

**Rejected.** There were two ways to do it:
- **Write a parser by hand.** Helm's `--set` grammar has comma-separated pairs, `{a,b}` lists, escaped dots and list indices, and a hand-written copy can drift from the Helm binary that runs the upgrade.
- **Import `helm.sh/helm`.** The module is not a dependency today, and adding it would pull a large dependency tree into `go.mod` for one parser.

The probe chart has neither problem.

## Notes

- **Until this ships,** the #504 workaround holds only until the next `setup init`. After `setup init`, run `helm upgrade --reuse-values` with the sizing. The cluster passes as long as nothing re-runs `setup init`, and a harness re-runs it only when a component is reported missing.
- **Follow-ups, each needing its own record or change:**
  - make the manager's memory independent of the cluster's Pod count (#504)
  - check after `helm --wait` that the manager stays up before reporting success
  - add a preflight warning when the cluster's Pod count exceeds what the memory limit covers
  - rename `--image-pull-secret` and read the token from an environment variable or a file instead of the command line

## References

- GitHub issue #504: `nvcre-manager` OOMKilled on large clusters; `setup init` cannot size it or detect it
- ADR-041: kubeadm-style `setup init` and `setup reset` with phases
- ADR-051: the controller's tolerate-all default, unchanged here
- ADR-092: per-namespace controller RBAC, whose amendment introduced the `rbac.*` carry-forward this record generalizes and the contract it supersedes
- `pkg/setup/setup.go`, `pkg/setup/helm.go`, `pkg/setup/helm_rbac.go`
- `helm/cluster-readiness-engine/values.yaml`, `helm/cluster-readiness-engine/templates/deployment.yaml`
- `docs/cli-reference/setup.md`, `docs/operations/deployment.md`
