# ADR-100: One Fragment Owns Each Variable in a Composed NCCL Profile

> **Status:** Accepted

## Context

Catalog arms build their NCCL environment by composing shared fragments from `pkg/catalog/entries/_lib/nccl/`. An arm lists the fragments it wants and the loader splices each one in order, either into `trainer.env` or, through `toMpiArgs`, into `trainer.args` as `-x NAME=VALUE` pairs:

```yaml
            env:
{{ lib "nccl/oci-gb300-roce-env.yaml" . | indent 12 }}
            args:
{{ lib "nccl/oci-roce-mpirun-args.yaml" . | indent 12 }}
{{ lib "nccl/oci-roce-env.yaml" . | toMpiArgs | indent 12 }}
{{ lib "nccl/oci-gb300-roce-env.yaml" . | toMpiArgs | indent 12 }}
```

The two destinations behave differently when two fragments carry the same name. `trainer.env` is a server-side map list keyed on name (`listType=map`, `listMapKey=name` on Kubeflow's `Trainer.Env`), so a repeated name makes the API server reject the whole Workflow. `trainer.args` is an atomic list of strings; a repeated name is legal, renders without complaint, and is resolved at run time by `mpirun`, which takes the last `-x` for a name.

That difference shaped the catalog. The env lists were curated by hand to never repeat a name, and `_lib/nccl/oci-gb200-roce-env.yaml:16-21` records the reasoning: `NCCL_IB_GID_INDEX` is deliberately left out of the GB200 delta because the shared profile already carries it, and "there is no reason to carry one shape that is legal in args and fatal in env." The args lists were never curated the same way, because nothing rejects them.

An exhaustive sweep of every arm, on every platform and architecture, across both the Certification and the WorkloadRun render paths, finds nine duplicate contributions. All nine are on OCI and all nine are in `trainer.args`:

| Arm | Name | Values, in composition order | Contributing fragments |
|---|---|---|---|
| OCI GB300, on `nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall` | `NCCL_IB_SPLIT_DATA_ON_QPS` | `0` then `1` | `oci-roce-env.yaml` then `oci-gb300-roce-env.yaml` |
| OCI GB300, same three entries | `NCCL_IB_GID_INDEX` | `3` then `3` | `oci-roce-env.yaml` then `oci-gb300-roce-env.yaml` |
| OCI GB200, same three entries | `NCCL_DEBUG` | `INFO` then `INFO` | `oci-roce-env.yaml` then `gb200-env.yaml` |

No env list repeats a name anywhere in the catalog.

Only `NCCL_IB_SPLIT_DATA_ON_QPS` carries values that disagree, and the value the ranks receive today is the right one. `1` is a measured GB300 finding: each GB300 node has four 800 Gbps RoCE rails, and untuned all-reduce plateaued near half the per-node fabric ceiling, the signature of traffic concentrating on a subset of ECMP paths. Enabling adaptive routing and splitting data across queue pairs raised peak bus bandwidth by about 70% on a multi-rack all-reduce and regressed no configuration measured.

The behaviour is correct. What it rests on is not. The GB300 fragment states the dependency as prose (`oci-gb300-roce-env.yaml:34-35`): "SPLIT_DATA_ON_QPS overrides the shared nccl/oci-roce-env.yaml default of `0`; this fragment is always included after that one, and mpirun -x takes the last value." Nothing enforces it. Reordering the two `lib` calls in an entry, or factoring the GB300 arm so the shared fragment lands later, still renders, still produces plausible args, and silently reverts a measured 70% tuning win. Three things make that worse than an ordinary latent bug:

- **A reader of the rendered output cannot tell which value wins.** The same args viewed as "what NCCL will see" read as a contradiction, and the resolution lives in `mpirun`'s precedence rules rather than anywhere in this repo.
- **Each duplicate costs two arguments.** Kubeflow caps `Trainer.Args` at 128 items and the OCI GB300 all-reduce arm already renders 70.
- **The coverage is thin where it matters.** Of the seven OCI GB300 arms, only `nccl-all-reduce` and `nccl-loopback` are rendered by any test. The all-gather and alltoall arms, two of the three the conflicting duplicate lives on, are rendered by no test in the repo, so an inverted order there would be invisible.

This is not operator-facing. ADR-097 added `spec.env`, and `applyEnv` rewrites every `-x` pair for a name rather than the first, so an operator who sets `NCCL_IB_SPLIT_DATA_ON_QPS` themselves gets their value whatever the catalog's ordering. The problem is the catalog's own internal consistency.

## Decision

**A variable is owned by exactly one fragment in any composition. Where two fragments both have an opinion about a name, split the shared one at the point of divergence rather than letting the more specific one shadow it.**

Concretely, for the two compositions that violate this today:

1. **Add `_lib/nccl/oci-roce-base-env.yaml`**, holding the eight names in the shared OCI RoCE profile that GB300 never retunes. `_lib/nccl/oci-roce-env.yaml` becomes that base plus the two names GB300 does retune:

   ```yaml
   {{ lib "nccl/oci-roce-base-env.yaml" . }}
   - name: NCCL_IB_GID_INDEX
     value: "3"
   - name: NCCL_IB_SPLIT_DATA_ON_QPS
     value: "0"
   ```

   The three OCI GB300 arms compose `oci-roce-base-env.yaml` with `oci-gb300-roce-env.yaml`. Every other OCI consumer keeps composing `oci-roce-env.yaml` and gets the same ten names with the same ten values.

2. **Add `_lib/nccl/gb200-tuning-env.yaml`**, holding `gb200-env.yaml` minus `NCCL_DEBUG`. `gb200-env.yaml` becomes `NCCL_DEBUG` plus that fragment, so its existing call sites are unchanged. The three OCI GB200 arms, which already get `NCCL_DEBUG` from the shared OCI profile, compose the tuning fragment instead.

3. **Pin the invariant with a test**, a golden-file suite in `pkg/certification` that renders the full cross product of catalog entries, platforms and architectures and records every name an arm forwards more than once. The golden is the duplicate list, empty when the catalog is clean, so a regression on any platform appears as a new row rather than as a failure someone has to interpret.

`lib` registers itself into the function map it renders with (`pkg/catalog/loader.go:709-717`), so fragments nest recursively. `_lib/nccl/aws-gb300-roce-env.yaml:10` already relies on this; nothing new is required of the loader.

## Implementation

The fragment split moves `NCCL_IB_GID_INDEX` and `NCCL_IB_SPLIT_DATA_ON_QPS` from positions 6 and 7 of the rendered profile to positions 9 and 10. `mpirun` resolves `-x` by name, so the order of distinct names carries no meaning, and the names and values are unchanged. Fourteen goldens record the old order and are regenerated; for the thirteen that are not GB300, the check is that the sorted multiset of `NAME=VALUE` pairs is identical before and after, so the only difference is position.

The GB300 golden is the one whose content genuinely changes: `certification-render-rdma/oci-gb300-nccl-control/expected.json` loses two `-x` pairs, 70 args to 66, keeping `NCCL_IB_GID_INDEX=3` and `NCCL_IB_SPLIT_DATA_ON_QPS=1` once each.

The GB300 arms' `env:` lists are not touched. They compose `oci-gb300-roce-env.yaml` alone and are already free of duplicates; the launcher container keeps the same six variables it has today.

The invariant suite asserts on rendered output rather than on fragment sources. `_lib/nccl/gcp-rtxpro6000-tcp-env.yaml:16-22` writes `NCCL_P2P_LEVEL` twice, in the two branches of a single `{{- if }}/{{- else if }}` keyed on `.GpusPerNode`, and only one ever renders; a source-level check reports four false duplicates. It reports same-value duplicates alongside conflicting ones, flagged apart: a repeat that agrees with itself is not a correctness bug, but it is wasted budget against the 128-item cap and it is the cheap early warning that a composition has started to overlap.

One name makes that warning worth more than tidiness. `isMNNVLEnabledInJobTemplate` (`pkg/controller/workflow_controller.go:4125-4144`) decides whether `diagnose` runs its no-NVL screening stage by scanning `trainer.args` and returning on the **first** `NCCL_MNNVL_ENABLE` it finds, while `mpirun` takes the last. An arm that forwarded that name twice at different values would send the controller down one branch and the ranks down the other. No arm does today, and the fragments say the separation is deliberate (`aws-efa-transport-env.yaml:8`, `aws-gb300-roce-transport-env.yaml:6`, `oci-gb200-roce-env.yaml:24`), but nothing enforced it before this suite. A same-value duplicate of that name is one edit away from a conflicting one, which is why the suite records both.

## Rationale

The alternative the issue prefers is to drop `NCCL_IB_SPLIT_DATA_ON_QPS` from the shared profile entirely and give each non-GB300 OCI arm an explicit `0`. It removes the invariant just as completely, and it is less machinery. Two things argue against it.

It costs roughly nine hand-written literals across the communication, loopback and training entries, and each one is a place a future OCI arm can forget. The failure mode of forgetting is silent: the variable is simply absent and NCCL falls back to its own default.

That default is not stable across the versions in play. NCCL documents `NCCL_IB_SPLIT_DATA_ON_QPS` as defaulting to `0` since 2.20, having defaulted to `1` in 2.18 and 2.19. An arm that relies on the default rather than stating it is correct only for images new enough, and the catalog pins several images it does not build. Stating `0` once in a fragment every OCI arm already composes keeps the value explicit without asking each arm to remember it.

Collapsing duplicates inside `toMpiArgs` was also considered. One Go change fixes the argument count and makes the rendered output readable, and it needs no catalog edit at all. It does not fix what this ADR is about: the winning value would still be decided by splice order, with the rule moved from a YAML comment into a Go function. It would also change rendered args on every platform that happens to carry a duplicate, which is a wider blast radius than the problem justifies.

## Consequences

Each arm's composition is now order-independent for the names involved. Rearranging the `lib` calls in an OCI arm changes the order of the rendered `-x` pairs and nothing else. The prose invariant in `oci-gb300-roce-env.yaml` is deleted rather than reworded, because there is no longer a dependency to describe.

The GB300 arms render four fewer arguments, 66 against the 128 cap.

The catalog gains two fragments. `oci-roce-base-env.yaml` is a profile defined by subtraction, which is a weaker thing to name than a profile defined by what it is for: it is the OCI RoCE settings GB300 does not retune. The fragment header says so, since the name cannot. The same holds for `gb200-tuning-env.yaml`.

`pkg/platform/testdata/apply-env/mpi-replaces-every-pair/` is a hand-written fixture from ADR-097 whose premise is that some catalog arms forward a name twice. Its inputs are literal args, so the case stays valid and keeps pinning that `applyEnv` rewrites every pair rather than the first. Its header comment cites the OCI arms as the motivating example and is updated to say that the catalog no longer produces them, since a user's own `spec.env` can still contain repeats and that is what the fixture now guards.

The invariant suite renders every entry at every platform and architecture, so it is the first test that walks the whole catalog cross product. It will catch duplicates introduced on platforms with no golden of their own, which is most of them, and it costs a full render per combination on every `make test`.

## Alternatives Considered

**Explicit `0` on each non-GB300 OCI arm, with the shared profile silent.** The issue's stated preference. Rejected above: nine forgettable literals, and a default that differs across NCCL 2.18 through 2.20.

**Dedupe inside `toMpiArgs`, last value wins.** Rejected above: it preserves the order dependency while hiding it better, and it changes rendered output on platforms that have nothing to do with this issue.

**A `mergeEnv` template helper that merges several fragments by name at the call site.** The most general fix, and it would let an arm say "these fragments, merged, last wins" in one expression. Rejected as more machinery than two duplicate names justify, and because it puts the precedence rule back into a function rather than removing it. Worth revisiting if a third composition develops the same overlap.

**Split the profile into head and tail fragments around the two retuned names, preserving rendered order exactly.** Churns one golden instead of fourteen. Rejected because the split point is an artifact of where GB300 happens to diverge, so neither fragment name describes anything a reader would recognize, and the catalog would carry three fragments where two suffice.

## Notes

The nine duplicates were found by three independent enumerations that agree: static resolution of every arm's fragment closure, an evaluator that renders the templates and composes arms the way `applyOverrides` does across the full platform and architecture cross product, and a scan of every committed golden for repeated `-x` names. The second mutation-tested its own detector; the third independently confirmed that no env list repeats a name in any rendered output.

The sweep also confirms the issue named every instance. Nothing outside OCI duplicates a name, in args or env, on either render path.

## References

- Issue [#507](https://github.com/NVIDIA/cluster-readiness-engine/issues/507)
- [ADR-031](031-platform-aware-nccl-config.md): platform-aware NCCL configuration, which introduced the shared fragments
- [ADR-088](088-oci-gb200-roce-and-rdma-device-access.md): the OCI GB200 arms, and why the GB300 tuning stays GB300-only
- [ADR-097](097-certification-category-env.md): `spec.env`, which rewrites every `-x` pair for a name
- `pkg/catalog/loader.go:709-717`, `:724-741`: `lib` registered into its own function map, enabling nested fragments
- `pkg/catalog/loader.go:337-353`: `toMpiArgs`
- NCCL environment variables: `NCCL_IB_SPLIT_DATA_ON_QPS` defaults to `0` since 2.20, `1` in 2.18 and 2.19
