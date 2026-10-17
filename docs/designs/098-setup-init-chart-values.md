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

1. **`setup init` takes chart values for the NVCRE release.** It gains three repeatable flags, `--values`/`-f <file>`, `--set key=value` and `--set-json key=json`. They have Helm's syntax and Helm's precedence: values files apply in the order given, `--set-json` overrides them, and `--set` overrides both. Helm parses them (Implementation), so `--set-json` needs Helm 3.10 or later, as it would with Helm directly. They apply to the NVCRE release only, not to Kubeflow Trainer. Giving any of them, or `--reset-values` (decision 2), with `--skip-phases` including `helm` is an error, since the phase that would use them does not run. Two forms Helm accepts for `--values` are limited, because `setup init` runs Helm more than once (Implementation, "Values files are read once"):
   - `-f -` (stdin) is accepted only with `--auto-approve`.
   - An `http://` or `https://` URL is rejected; download the file first.

2. **`setup init` keeps the values set on the installed release.** This supersedes ADR-092's "every other value returns to its default" contract. Each value is resolved in this order, lowest first:

   > chart defaults → values set on the installed release → this run's `--values` files → this run's `--set-json` → this run's `--set` → values nvcrectl sets itself (the image, decision 4, and the values of flags given on this run)

   The chain holds for each value separately. An operator value replaces only what it names, even when it is given as a parent object: `--set-json 'rbac={"runNamespaces":["team-b"]}'` changes `rbac.runNamespaces` and leaves a kept `rbac.scope` in place. Helm alone would not do this (Implementation, "Building the upgrade").

   "Values set on the installed release" means what `helm get values -o json` returns: the values the release was installed or upgraded with, not the chart defaults. A release Helm has no record of is a first install, so nothing is kept. Any other failure to read the values stops `setup init`, as ADR-092 already does for `rbac.*`. Two exceptions:
   - `manager.image.repository`, `manager.image.tag` and `manager.image.digest` are never kept (decision 4). Other keys under `manager.image`, such as `pullPolicy`, are kept like any value.
   - A new flag, `--reset-values`, drops the kept values and starts from the chart defaults, except `rbac.scope` and `rbac.runNamespaces`. Those are still kept, so a reset never silently widens a namespaced grant (ADR-092). An operator changes them with `--rbac-scope` or `--set`.

3. **A value that has its own flag is set by the flag or by a value, not both.** On a single run, the operator may give a flag or set its value with `--values`, `--set` or `--set-json`, but not both. Setting a parent counts when it sets the value: `--set-json 'manager.image={"tag":"v1"}'` sets the image, while `--set manager.image.pullPolicy=Always` does not. A value kept from the installed release never conflicts with a flag, because the flag is a deliberate change on this run.

   | Value | Flag | When the operator gives both |
   |---|---|---|
   | `manager.image.repository`, `.tag` or `.digest` | `--image` | Rejected; the error names `--image` |
   | `rbac.scope` | `--rbac-scope` | Rejected; the error names `--rbac-scope` |
   | `manager.imagePullSecrets` | `--image-pull-secret` | Combined (decision 5) |

   When the operator gives neither, the default applies. For the image that is the CLI-version image (decision 4). For `rbac.scope` and the pull secrets it is the installed release's value, or the chart default on a first install.

4. **The controller image is worked out on every run and never kept.** The installed release's values always contain the image nvcrectl set on an earlier run, and those cannot be told apart from an image an operator chose. So `manager.image.repository`, `.tag` and `.digest` are dropped from the kept values, and each run uses the first of these:
   - **The operator set `manager.image.repository`, `.tag` or `.digest` on this run.** nvcrectl sets no image values, and the chart fills the rest. An unset tag defaults to the chart's `appVersion`. A published chart's `appVersion` is its release version (`Makefile:306`), which is also the controller image's tag, so `--set manager.image.repository=<mirror>/manager` alone is enough for a mirror.
   - **`--image` was given.** A `repo:tag` reference sets `manager.image.repository` and `manager.image.tag`. A `repo@sha256:…` reference sets `manager.image.repository` and `manager.image.digest`. This fixes the invalid reference described in Context.
   - **Neither.** The CLI-version image, as today.

   Other keys under `manager.image`, such as `pullPolicy`, do not choose the image. Setting one alongside `--image` is not a conflict, and `--image` still applies.

   Because the image is never kept, a digest pinned on an earlier run cannot hold the controller on an old image after the chart and CRDs move to a new version.

5. **`--image-pull-secret` adds its secret to the list rather than replacing it.** When the flag is given, `manager.imagePullSecrets` is the list the merge in "Building the upgrade" produces from the kept values and this run's flags, with `{name: nvcrectl-pull-secret}` added at the end unless an entry with that name is already there. The chart's default list is empty (`helm/cluster-readiness-engine/values.yaml:33`), so with neither a kept nor an operator list the result is `[nvcrectl-pull-secret]`. The single `[0]` write is removed.

   The merge starts from Helm's own reading of the flags, so an operator's index write edits a kept list as Helm would. With a kept `[a, b, c]` and an operator `--set manager.imagePullSecrets[0].name=x`, the list is `[x, b, c, nvcrectl-pull-secret]`. Without the flag, nvcrectl adds nothing to this value. The flag's other two jobs are unchanged: creating the `ghcr.io`-scoped `nvcrectl-pull-secret` Secret, and logging Helm in to GHCR for chart pulls.

6. **Values are checked and shown before anything changes.** When the `helm` phase will run, preflight does three things before the confirmation prompt (`pkg/setup/setup.go:238-240`), and before the deps phase installs anything:
   - Reads the installed release's values.
   - Works out what the operator passed on this run.
   - Applies decision 3, so a conflict stops `setup init` with the cluster unchanged.

   When `--skip-phases` includes `helm`, none of this runs and the NVCRE release is not read, as today (`pkg/setup/setup.go:261-268`).

   Preflight then prints the values the NVCRE release will be given: the merged result from "Building the upgrade", not its inputs, so a kept value that this run overrides is not listed as kept. Each value is printed as its path and where it came from, without the value itself. The printout appears with `--auto-approve` too, so it reaches CI logs, and a kept or operator value can hold anything, including a credential under a key the chart never reads. A list is printed as one path, never flattened into indices.

   Three things are printed with their values, because nvcrectl sets or checks them itself and none can hold a credential: the controller image, the pull-secret list (Secret names, not their contents), and `rbac.scope`. To see the other values, the operator reads their own inputs or runs `helm get values`, which needs access to the release.

   ```text
   [preflight] Values for release "nvcre" (paths only; values are not printed):
     manager.resources.limits.memory            (installed release)
     manager.resources.requests.memory          (this run)
     manager.tolerations                        (installed release)
   [preflight] rbac.scope: namespaced (installed release)
   [preflight] Controller image: ghcr.io/nvidia/cluster-readiness-engine/manager:v0.8.0 (CLI version)
   [preflight] Image pull secrets: [mirror-pull-secret, nvcrectl-pull-secret] (installed release, --image-pull-secret)
   ```

## Implementation

### Reading the operator's values

Decisions 2 to 5 need the operator's values as data. To get them, nvcrectl renders a probe chart: `helm template` on a chart that has no `values.yaml` and a single template, `{{ toJson .Values }}`. Helm parses the operator's flags exactly as it would for an upgrade, including the `--set` syntax (comma-separated pairs, `{a,b}` lists, escaped dots, list indices, `null`). The probe chart is two small files written to a temporary directory created with mode 0700. The probe needs no cluster access. It runs up to twice:
- **With the operator's flags alone,** when the operator passed `--values`, `--set` or `--set-json`. Call the result *O*. Decisions 3 and 4 read it, because only what the operator passed on this run can conflict with a flag or choose the image.
- **With the kept-values file in front of the operator's flags,** when the operator passed flags and there are kept values. Call the result *P*: Helm's own reading of the kept values and this run's flags together. Without operator flags *P* is the kept values; without kept values it is *O*.

An operator's `--set-json` needs Helm 3.10 or later. On an older Helm, the probe fails with Helm's own error during preflight, before anything changes.

### Values files are read once

Helm runs up to three times on one `setup init`: the probe twice, then the upgrade. A values source that can be read only once would reach the first run and leave the later ones with nothing. Examples are stdin (`-f -`), process substitution (`/dev/fd/N`) and a named pipe. A regular file could also change between the two probes. So before Helm runs at all, nvcrectl reads each `--values` argument once and writes a copy to the temporary directory, with mode 0600. Both probes receive the copies, so they see the same bytes.

Two forms Helm accepts are limited, and both are rejected during validation, before anything is read or installed:
- **`-` (stdin) without `--auto-approve`.** Without `--auto-approve`, stdin is the input for the confirmation prompt (`pkg/setup/setup.go:233`) and for the deps recovery prompt (`:547`). With `--auto-approve`, stdin is read once like any other source.
- **An `http://` or `https://` URL.** Copying it once would mean nvcrectl fetching it with its own HTTP client, which would not behave like Helm's getters (proxies, TLS, credentials). Passing it through would fetch it once per probe, and the copies could differ. The error says to download the file first.

### Building the upgrade

The kept values and the operator's flags are not handed to the upgrade as separate layers. Helm deep-merges `-f` files, but `--set-json` at a key replaces the whole subtree under it. With a kept file in front, an operator's `--set-json 'rbac={"runNamespaces":["team-b"]}'` would drop the kept `rbac.scope: namespaced`. The chart's `cluster` default would then fill it in, and a namespaced grant would widen to cluster-wide with no error. Likewise, `--set-json 'manager={"replicas":2}'` would drop a kept `manager.resources.limits.memory: 8Gi` and a kept `manager.imagePullSecrets` list. So nvcrectl merges first and gives the upgrade one values file:

1. **Merge the kept values under *P*.** Start from the kept values and lay *P* over them. Maps merge key by key, and wherever *P* has a key, *P*'s value wins, including a list and including `null`. A kept value survives under a subtree an operator `--set-json` replaced, as a chart default would, which is decision 2's per-value chain. An operator's `null` still removes a kept value. An index write over a kept list keeps Helm's reading, because *P* already holds it.
2. **Add nvcrectl's own values:** the image values from decision 4 when it sets them, the pull-secret list from decision 5 when `--image-pull-secret` is given, and `rbac.scope` when `--rbac-scope` is given. Decision 3 guarantees that the operator set none of these keys on this run, except the pull-secret list, which decision 5 builds from the merged list.
3. **Write the result to one temporary file,** with mode 0600, removed when Helm returns. The preflight printout (decision 6) is made from this file.

`nvcreHelmUpgradeArgs` then assembles `helm upgrade --install` from the fixed arguments as today (release, chart, namespace, `--create-namespace`, `--version`, `--wait`, `--timeout`) and `-f <that file>`. The operator's flags reach Helm only through the probes; their effect reaches the upgrade through the merged file. A `null` in that file removes the chart default, as it would on the command line.

ADR-092's dedicated carry of `rbac.*` folds into the kept values. `pkg/setup/helm_rbac.go` keeps `--rbac-scope` validation. The preflight summary shows the scope with its source, for example `rbac.scope: cluster (--rbac-scope)`, in place of a separate "Setting rbac.scope" message.

### Helm version

nvcrectl does not check the Helm version, and this record does not add a check. The only flag nvcrectl adds to the upgrade is `-f`, and the probes pass the operator's own flags. The conditional `--set-json` for `rbac.runNamespaces` that `main` passes today (`pkg/setup/helm_rbac.go:89-98`) goes away with the rest of the `rbac.*` carry. What nvcrectl needs is then what the default `oci://` chart reference already needs: Helm 3.8 or later, the release where OCI support became generally available.

An operator's own `--set-json` needs Helm 3.10 or later, as it would with Helm directly. `--set` and `--values` work on any Helm 3 or Helm 4 release.

### Files

- `pkg/setup/setup.go`:
  - add the flags `--values`/`-f`, `--set`, `--set-json` (string arrays) and `--reset-values`
  - reject the value flags and `--reset-values` when the `helm` phase is skipped
  - when the `helm` phase will run, read the installed release's values, run the probes and build the merged values in preflight
  - make the phase summary (`initPhaseSummary`, `:1101-1104`) name the image's source
  - `parseImage` (`:1683`) returns a digest as a digest, not as a tag
- `pkg/setup/helm_values.go` (new):
  - read the installed release's values (generalized from `readReleaseRBACValues`)
  - copy the values sources and run the probe chart
  - detect conflicts
  - decide the image
  - merge the kept values under *P* and add nvcrectl's own values
  - write the values file
  - print the summary
- `pkg/setup/helm.go`: `nvcreHelmUpgradeArgs` takes the values file and passes it with `-f`; the `--set` writes for the image and `imagePullSecrets[0]` are removed.
- `pkg/setup/helm_rbac.go`: only the `--rbac-scope` handling remains.

### Documentation

- **`docs/cli-reference/setup.md`:**
  - document the new flags, including that `-f -` needs `--auto-approve` and that URLs must be downloaded first
  - add a "Values across runs" section covering decisions 2 to 5
  - replace "Every other chart value returns to its default on each run." (`:73`)
  - rewrite the "no flag to bind a custom-named pull secret" clause and its "install the chart directly with Helm" workaround (`:77`): `--set manager.imagePullSecrets[0].name=…` now does it
  - state the Helm requirement: 3.8 or later, and 3.10 or later for an operator's own `--set-json`
- **`docs/getting-started/install.md`:** change the prerequisite "Helm 3.x" (`:14`) to "Helm 3.8 or later"
- **`docs/operations/deployment.md`:**
  - show setting chart values through `setup init`
  - rewrite the mirror pull-secret guidance (`:115`): a custom-named secret no longer requires bypassing `setup init`
  - rewrite the statement that "every other chart value returns to its default, as before" (`:286`)
  - state in the resource requirements section (`:157-178`) and the large-cluster tuning table (`:367`) that manager memory follows the cluster-wide Pod count, not the node count, with the issue #504 data point (3.83 GiB peak at about 80,000 pods), and show how to set it through `setup init`
- **ADR-092:** add a dated "Superseded" note under the amendment bullet that states the contract, as ADR-074 does. This change makes that note.
- **`CHANGELOG.md`:**
  - under Added: the new flags
  - under Changed: `setup init` now keeps values set on the installed release
  - under Fixed: `--image` with a digest

### Testing plan

Two golden-file tests in `pkg/setup` already pin behaviour this record changes, and they stay golden files (`testutil.TestCaseParser`), matching the package's `testdata/`:
- `TestHelmChartArgs` (`pkg/setup/helm_test.go:52`) drives `nvcreHelmUpgradeArgs` from `pkg/setup/testdata/helm-chart-args/`. `mirror-refs/expected.json` pins the `manager.imagePullSecrets[0].name=` write that decision 5 removes.
- `TestParseImage` (`pkg/setup/setup_test.go:206`) drives `parseImage` from `pkg/setup/testdata/parse-image/`. `digest-reference/expected.json` pins `"tag": "sha256:abc123def456"`, which decision 4 fixes.

Both goldens change, and regenerating them needs the user's approval, per CLAUDE.md. New structured output, such as the merged values and the summary, gets new cases under `pkg/setup/testdata/` in the same pattern, not a parallel table. Reads of the installed release keep faking Helm with a shell script, as `TestReadReleaseRBACValues` does (`pkg/setup/helm_rbac_test.go:104`).

Cases to cover:
- **Kept values:**
  - no release (nothing kept)
  - a release with no values
  - partial `manager.resources` (kept as written, so Helm deep-merges it with the defaults)
  - `null` entries (kept)
  - `manager.image.repository`, `.tag` and `.digest` dropped, `manager.image.pullPolicy` kept
  - with `--reset-values`, only `rbac.*` kept
  - an unreadable release (error)
  - with `--skip-phases=helm`, the release is not read at all
- **Merge over kept values:**
  - kept `rbac.scope: namespaced` with `--set-json 'rbac={"runNamespaces":["team-b"]}'` keeps `scope: namespaced`
  - kept `manager.resources.limits.memory: 8Gi` and a kept pull-secret list, with `--set-json 'manager={"replicas":2}'`, keep both
  - an operator `null` removes a kept value
  - an operator list replaces a kept list
- **Conflicts:**
  - rejected: `--image` with `--set manager.image.tag`, with `-f` setting `manager.image.repository`, and with `--set-json 'manager={…}'` containing an image
  - allowed, with `--image` still used: `--image` with `--set manager.image.pullPolicy=Always`
  - rejected: `--rbac-scope` with `--set rbac.scope`; allowed: `--set rbac.scope` alone
  - rejected: the value flags or `--reset-values` with `--skip-phases=helm`
- **Values files:**
  - a source that can be read once (a named pipe), reaching both probes with the same content
  - `-f -` with `--auto-approve`, given to both probes
  - `-f -` without `--auto-approve` (rejected)
  - an `https://` URL (rejected)
  - the copies and the values file are written with mode 0600 and removed afterwards
- **Image:** operator-set (nvcrectl sets nothing), `--image repo:tag`, `--image repo@sha256:…` (sets `digest`, not `tag`), the CLI default, and a digest in the installed release that is not kept.
- **Pull secrets:**
  - each combination of flag, operator list and kept list, including an operator list that already holds `nvcrectl-pull-secret`
  - an operator index write over a kept list (kept `[a, b, c]` with `--set manager.imagePullSecrets[0].name=x` gives `[x, b, c, nvcrectl-pull-secret]`)
  - the chart's default list is empty, pinned so a non-empty default cannot slip past the probe
- **Upgrade arguments:** the fixed arguments plus one `-f`; no `--set` or `--set-json` from nvcrectl.
- **Probe, against real Helm:** use `requireHelm` from `test/helm/render_test.go:105`, which fails in CI without Helm and skips locally. Cover:
  - comma-separated `--set` pairs, lists, escaped dots and `null`
  - the precedence `-f` < `--set-json` < `--set`
  - the second run with the kept-values file in front
  - the merged values file rendered against the real chart for the parent-key `--set-json` cases above, checking that `rbac.scope: namespaced` still renders a RoleBinding and no ClusterRoleBinding
- **Output:** the summary lists the merged values' paths with their sources, and never a value outside the image, the pull-secret names and `rbac.scope`, with and without `--auto-approve`. Include a case where a kept value under a key the chart does not read holds a token-like string that must not appear.

## Rationale

- **It closes the #504 failure with no change to how a harness calls `setup init`.** A harness that pins an nvcrectl version must move to a release that includes this change. After that, once an operator sets the memory, a later `setup init` with no flags, which is what the harness runs, keeps it. The flags also let an operator or a harness set it through nvcrectl in the first place.
- **Chart values are already the documented interface** (`docs/operations/deployment.md`, key chart values). Accepting them in Helm's syntax costs operators nothing new to learn, and it covers values beyond memory, such as placement and the mirror pull secret, without a flag for each.
- **Keeping every value matches what an operator expects.** Once `--set` exists, a value that survives some runs and not others, depending on its key, is a trap. ADR-092 carried `rbac.*` because a reset there was unsafe. #504 shows a reset of `manager.resources` is just as harmful.
- **Doing the carry-forward in nvcrectl, not with a Helm flag,** is what makes the exceptions possible: dropping the image, combining the pull-secret list, and keeping kept values under an operator's parent-key `--set-json`. It also keeps nvcrectl's own arguments to a single `-f`, within what Helm 3.8 accepts, the floor the default OCI chart reference already sets.
- **The probe chart reuses Helm's parser.** The operator's flags are read by the same Helm binary that runs the upgrade, so nvcrectl never interprets `--set` syntax itself.
- **Merging in Go after the probe** gives decision 2's per-value chain exactly. Layering the kept file and the operator's flags on one Helm command would not (see Alternatives).
- **Checking in preflight** means a rejected run changes nothing on the cluster, including the Trainer release the deps phase would otherwise install first.

## Consequences

- **Values set with `helm upgrade` now survive `setup init`.** This is intended, and it is a change of contract. An operator who relied on `setup init` to restore the chart defaults uses `--reset-values`.
- **A kept value outlives a change of chart default.** If a later chart raises a default the operator had pinned, the pin wins. The chart has no `values.schema.json`, so a kept key that a later chart removed is ignored silently rather than failing the upgrade. `--reset-values` clears both cases.
- **Releases installed by earlier `setup init` versions upgrade cleanly.** Their stored values are the image repository and tag, which are dropped; `manager.imagePullSecrets[0]`, which is kept and combined; and `rbac.*`, which was already kept. Values an operator added with `helm upgrade`, such as the #504 workaround's memory and tolerations, are kept from the first run of the new version.
- **An operator's parent-key `--set-json` behaves differently from plain Helm in one respect.** Values kept from the installed release survive under it, as chart defaults would. To remove a kept value, set it to `null` or use `--reset-values`.
- **`setup init` runs up to two more local Helm commands** (`helm template`), only when value flags are given. They need no cluster access.
- **nvcrectl's Helm requirement stays at 3.8.** The conditional need for 3.10 that `main` has when `rbac.runNamespaces` is set goes away. Only an operator who passes `--set-json` needs 3.10.
- **The installed release's values are read before the confirmation prompt** rather than in the helm phase, and only when the `helm` phase will run. A cluster where `helm get values` fails now stops `setup init` before the deps phase, where before it failed after Trainer was installed.
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

### Layer the kept file and the operator's flags on the upgrade command

**Rejected.** This was the first version of this record: `-f <kept values>`, then the operator's flags, then nvcrectl's own `--set`s. Helm deep-merges `-f`, but `--set-json` replaces the subtree it names, so a parent-key `--set-json` drops kept values under it and the chart default comes back. With a kept `rbac.scope: namespaced`, `--set-json 'rbac={"runNamespaces":["team-b"]}'` renders the cluster-wide `nvcre-run-rolebinding` instead of a RoleBinding in `team-b`, without error. That inverts ADR-092's guarantee. Merging in Go after the probe avoids it, and it also means the upgrade needs only `-f` from nvcrectl, so the pull-secret list no longer has to be written by index to stay within Helm 3.8.

### Pass values files to Helm as given

**Rejected.** Helm runs more than once on each `setup init`, so stdin, process substitution and named pipes would reach only the first run, and a file edited during the run could differ between the two probes.

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
