# ADR-087: `mlnxPerNode: 0` As a Real Opt-Out, and OCI GB200 NIC Defaults

> **Status:** Proposed

## Context

Rendering `communication/nccl-all-reduce` with `--platform oci` against GB200 nodes emits, on every worker pod:

- `nvidia.com/mlnxnics: "8"` in both `limits` and `requests`
- `k8s.v1.cni.cncf.io/networks: network-operator/sriov-net,...` repeated eight times

A user reported (GitHub issue #350) that their OCI GB200 nodes advertise no `nvidia.com/mlnxnics` extended resource and have no `sriov-net` NetworkAttachmentDefinition, so every pod stays Pending with `FailedScheduling: Insufficient nvidia.com/mlnxnics`. Their nodes do carry the usual NFD RDMA labels (`pci-15b3.present`, `rdma.available`, `network-sriov.capable`), so the hardware is there; what is absent is the device plugin that turns it into a schedulable extended resource, and the NAD that the annotation references.

Three distinct defects produce this, and the first two are not OCI-specific.

**1. OCI GB200 has no platform default, so it inherits an architecture default with no provenance.** `pkg/catalog/entries/_lib/gpu-defaults.yaml` sets the `gb200` architecture default `mlnxPerNode: 8`. Its `platformOverrides.oci` map covers only `l40s` (2) and `gb300` (4), so OCI GB200 falls through to 8. The repository records nothing about which OCI GB200 shape that 8 was measured on. It is not a stable fact about the family either: Oracle's own GB200 guidance lists a different HCA device count across shape revisions, and derives the list at runtime from the instance metadata shape string. Re-pinning some other constant would be a different guess, not a fix.

**2. `mlnxPerNode: 0` is documented as an opt-out that no template implements.** Three places in the codebase state that zero means the templates omit the resource:

- `pkg/catalog/entries/_lib/gpu-defaults.yaml:9-10`: `0 means "do not set nvidia.com/mlnxnics" — templates branch on it`
- `pkg/catalog/catalog.go:46`: `0 means the templates omit nvidia.com/mlnxnics`
- `pkg/catalog/loader.go:111`: `0 means omit nvidia.com/mlnxnics`

No template branches on it. A search for a conditional on `MlnxPerNode` across `pkg/catalog/entries/` and `pkg/platform/overrides/` returns nothing; the only guard in the neighbourhood keys on `.NicResourceName` (`_lib/deps/onprem-gb200-gb300-runtime-patch-comm.yaml:26`), which is a different condition serving a different purpose (ADR-075). Rendering with `mlnxPerNode: 0` therefore emits a literal `nvidia.com/mlnxnics: "0"` and, on OCI, `k8s.v1.cni.cncf.io/networks: null`. Seven `_lib/` fragments plus one inline block in `pkg/platform/overrides/workloadrun.yaml` share this shape. The gap has not been noticed because the one architecture that defaults to zero (`rtxpro6000`) reaches none of them.

**3. A platform override cannot lower a count to zero.** `GPUDefaults` applies platform-override fields only when they are non-zero (`pkg/catalog/gpu_defaults.go:81-86`). `NodeDefaults` has plain `int32` fields, so "absent" and "zero" are indistinguishable after parsing, and the non-zero test is what makes "unspecified fields fall through to the architecture defaults" work. As written, no `platformOverrides` entry can express "this platform wants none".

Defect 1 is what the issue reports. Defect 2 is what makes the documented workaround fail. Defect 3 is what blocks the natural fix for defect 1.

## Decision

1. **Make `mlnxPerNode: 0` mean what the documentation already claims**, in every template that renders `nvidia.com/mlnxnics`: at zero, emit no resource entry and no network attachment annotation. Seven `_lib/deps/` fragments and the inline Azure dependency in `pkg/platform/overrides/workloadrun.yaml` are guarded. The three comments above become true statements and gain a pointer to where the branching lives.

2. **Add an OCI GB200 platform default of `mlnxPerNode: 0`**, so an OCI GB200 target requests no NIC resource and no SR-IOV attachment unless asked. Sites that do run the SR-IOV device plugin opt in with `mlnxPerNode`, globally on `spec` or per category.

3. **Let a platform override express zero** by parsing `platformOverrides` into a pointer-field struct, so absent and zero are distinguishable. The exported `NodeDefaults` return type is unchanged.

4. **Do not make the resource name or the NetworkAttachmentDefinition name configurable.** `nvidia.com/mlnxnics` and `network-operator/sriov-net` stay hard-coded in the OCI fragment. No CRD change.

### Guard placement

The guard is `{{- if gt (int .MlnxPerNode) 0 }}`. `gt` is a `text/template` builtin, so it needs no addition to the hand-rolled function map (`pkg/catalog/loader.go:264-295`), and the existing `int` function already converts the `int32` field.

Where the guard opens and closes is **not** a matter of style. Override dependencies are merged with `mergeMaps` (`pkg/controller/workflow_detect.go:654-665`), which deletes any key whose override value is nil:

```go
for key, val := range override {
    if val == nil {
        delete(result, key)
        continue
    }
```

A guard that leaves a childless `resources:`, `limits:` or `annotations:` key behind therefore does not merely add nothing. It unmarshals to nil and **deletes the base dependency's entire block**, including the `nvidia.com/gpu` request the base runtime carries. Every guard must remove the parent key along with its children. That constraint, not the shape of the YAML, determines the boundary in each file:

| Fragment | Boundary | Why |
|---|---|---|
| `_lib/deps/oci-mlnxnics-comm.yaml` | whole document | The TrainingRuntime exists only to add the annotation and the resource. Collapsing it to nothing also removes the `sriov-net` annotation, the other half of the reported failure. |
| `_lib/deps/togetherai-ib-runtime-patch.yaml` | whole document | Same: its only content is the mlnxnics limits and requests. |
| `_lib/deps/forge-ib-comm.yaml` | the two `nvidia.com/mlnxnics` lines | Its `limits` and `requests` also carry `nvidia.com/gpu`, so the parent keys must survive. |
| `_lib/deps/azure-a100-ib-training.yaml`, `azure-h100-ib-training.yaml`, `azure-ib-with-topo-comm.yaml`, `azure-ib-with-topo-a100-comm.yaml` | the `resources:` key through both child entries | mlnxnics is the only entry under `limits`/`requests`, so the `resources:` key itself must go. The container's `volumeMounts` and the topology ConfigMap document stay outside the guard in all four; the two training fragments also keep their `securityContext` there. |
| `pkg/platform/overrides/workloadrun.yaml` (Azure block) | the whole dependency entry | Inline, not `lib`-sourced, and exists only for mlnxnics. Note it templates the value unquoted, unlike the fragments. |

When a whole-document fragment renders empty, its call site becomes `dependencies:` with no items, which unmarshals to a nil `[]DependencySpec`; `applyOverrides` ranges over it as a no-op and the override's `jobTemplate` still applies.

The two on-prem fragments (`onprem-gb200-gb300-runtime-patch-{comm,training}.yaml`) are **not** changed here. They already guard on `.NicResourceName`, and their `limits` also carry `nvidia.com/gpu`. They do have a related zero-count gap, recorded under Notes.

## Implementation

- `pkg/catalog/gpu_defaults.go`: a `nodeDefaultsOverride` struct with `*int32` fields backs `gpuDefaultsData.PlatformOverrides`; `GPUDefaults` applies each field when its pointer is non-nil instead of when its value is non-zero. `NodeDefaults` (the return type, serialized directly into the `gpu-defaults` goldens) keeps plain `int32` fields.
- `pkg/catalog/entries/_lib/gpu-defaults.yaml`: `platformOverrides.oci.gb200` with `gpusPerNode: 4` and `mlnxPerNode: 0`, written like the sibling `gb300` entry. The header comment gains a pointer to where the branching lives.
- Seven `_lib/deps/` fragments and `pkg/platform/overrides/workloadrun.yaml` gain the guard at the boundaries tabulated above.
- `pkg/catalog/catalog.go:46` and `pkg/catalog/loader.go:111`: the comments become accurate.
- Docs: an OCI row in the architecture-specific resources table in `docs/concepts/platform-detection.md` (which today has no OCI rows at all), and the zero semantics on the `mlnxPerNode` rows of `docs/api-reference/certification.md` and `docs/api-reference/workloadrun.md`. A `Fixed` entry under `[Unreleased]` in `CHANGELOG.md`.
- The `MlnxPerNode` doc comments on `api/v1alpha1/certification_types.go` and `api/v1alpha1/workloadrun_types.go`, which `controller-gen` copies into the CRD `description`. That text is what `kubectl explain` prints, so leaving it saying only "overrides the auto-detected count" would keep the old meaning in the one place an operator reads from the cluster itself. Description-only: no schema change, no new validation.

### Testing plan

Goldens through `testutil.TestCaseParser`, per the package policy in CLAUDE.md:

- `pkg/catalog/testdata/gpu-defaults/gb200-oci/`: pins `{gpusPerNode: 4, mlnxPerNode: 0}`, covering both the new override and the pointer-field change. The existing `l40s-oci` and `rtxpro6000-gcp` cases pin that fall-through and arch-level zero are unaffected.
- A new render suite under `pkg/certification/testdata/certification-render-mlnx/`, modeled on `certification-render-onprem/`: OCI GB200 (no resource, no annotation), OCI GB300 control (still 4), and OCI GB200 with an explicit `mlnxPerNode` (opt back in). `pkg/render/nodes/oci-gb200.yaml` already exists as a fixture.
- `cmd/integration/testdata/reconcile/certification-oci-gb200-nccl/`, modeled on the existing `certification-oci-l40s-nccl/`.
- **One zero-count case per guarded fragment.** Each fragment is reachable only under its own `when` clause, so a case that does not match that clause never renders it and its guard goes untested. Azure alone splits four fragments across two axes, architecture (`a100` / `h100`) and domain (communication / training), so covering it takes four cases, not one. Forge gates on `l40` and not `l40s`; TogetherAI gates on platform with no architecture clause. The suite is sized to that gating, not to the number of platforms.

  The criterion is mutation, not line coverage: deleting any single guard must turn at least one golden red. A case that merely renders a fragment at a nonzero count does not satisfy it, because the guard is a no-op there.

Two existing mechanisms execute the new false branches for free, but neither substitutes for the goldens above. `pkg/catalog/loader.go:374-378` renders every entry template against a zero-value `TemplateData{}` at registration and fails registration on a render error; `TestEveryLibFragmentParsesThroughPlatform` (`pkg/platform/overrides_test.go:37`) asserts every `_lib/` fragment still parses through the platform renderer. Both check that a guard does not *break* the render. Neither checks what it renders, so a guard deleted outright leaves both green: the zero-value data makes the false branch the one taken, and a fragment with no guard at all still parses and still renders. Only a golden that asserts the absent request catches that.

Render verification per the nvcrectl procedure in CLAUDE.md: OCI GB200 must emit neither `mlnxnics` nor `sriov-net`; the same certification with an explicit `mlnxPerNode` must emit both; OCI GB300 (4), OCI L40S (2) and Azure H100 (8) must be unchanged.

## Rationale

- **Zero is the only honest default for OCI GB200.** The count 8 has no recorded provenance, and Oracle's own documentation does not agree with itself across shape revisions. Any nonzero default is a guess that makes pods unschedulable on every cluster whose device plugin does not match it, which is exactly the reported failure. Requesting nothing is always schedulable; it costs a site that does run the plugin one line of configuration, and that site knows its own count.
- **The opt-out should work because it is already promised.** Three comments tell operators that `mlnxPerNode: 0` turns the resource off. An operator who reads them and sets it gets `nvidia.com/mlnxnics: "0"` and a null annotation instead. Fixing the templates is cheaper than deleting the promise from three places, and it is what makes decision 2 expressible at all.
- **Guarding every fragment, not only OCI, keeps one meaning for one field.** A field that means "omit" on OCI and "request zero" on Azure is worse than either behavior alone. The sibling fragments are latent rather than broken today, but the cost of fixing them now is a few lines each, and the alternative is that the next platform default of zero reintroduces the same bug somewhere else.
- **Pointer fields on the override struct, not on `NodeDefaults`.** The override struct is internal; `NodeDefaults` is the return type that the 18 `gpu-defaults` goldens serialize. Changing the internal one distinguishes absent from zero where the distinction is needed and leaves every existing golden byte-identical.

## Consequences

- **OCI GB200 renders change.** The `nvidia.com/mlnxnics` request and the `k8s.v1.cni.cncf.io/networks` annotation disappear from the five communication entries, whose OCI override block is not gated on architecture. The two training entries already gated their OCI block on `l40`/`l40s` and `gb300`, so GB200 training never carried the request and does not change. A site that was relying on the previous default (an OCI GB200 cluster running the SR-IOV device plugin, where 8 happened to be right) must now set `mlnxPerNode: 8` explicitly. That is a visible behavior change and belongs in the release notes; the failure mode if missed is a performance regression, not an outage, whereas the current failure mode for everyone else is pods that never schedule.
- **No other platform or architecture changes.** The guards only alter output where the resolved count is zero, and no existing golden across `cmd/integration/testdata/`, `pkg/*/testdata/` or `test/uat/testdata/` renders a zero count. Azure, TogetherAI, Forge, OCI L40S and OCI GB300 are untouched.
- **`mlnxPerNode: 0` becomes a supported way to turn the NIC request off** on every platform whose templates request `nvidia.com/mlnxnics`, including Azure and TogetherAI, where it previously produced a literal zero request.
- **A guard boundary is load-bearing and must stay that way.** Because `mergeMaps` deletes nil-valued keys, a future edit that leaves a childless `resources:` or `limits:` key inside a false branch would silently delete the base GPU request rather than fail. The tests pin the rendered output; this ADR records the reason.
- **OCI GB200 and GB300 now differ in kind, not only in count.** GB300 keeps its RoCE NIC request (4); GB200 requests nothing. That asymmetry is deliberate and reflects what is actually known about each.

## Alternatives Considered

- **Pin a corrected count (4) for OCI GB200.** Rejected: it does not fix the report. The nodes in the issue advertise no `nvidia.com/mlnxnics` at any count and have no `sriov-net` NAD, so `4` fails to schedule exactly as `8` does. It also inherits the provenance problem, trading one unsourced constant for another.
- **Extend ADR-075 NIC auto-detection to OCI GB200.** Rejected for this change. Detection resolves only the resource *name*, never the count (ADR-075, Alternatives), and it cannot pick a NetworkAttachmentDefinition at all, so the `sriov-net` annotation would still be emitted eight times. It is additive to this fix rather than a substitute, and extending the gate is a larger change than the report warrants. Worth revisiting if OCI sites turn out to advertise a consistent candidate.
- **Make the resource name and the NAD name configurable.** Rejected for this change. `nicResourceName` already exists end to end and is consumed only by the on-prem fragments, so wiring OCI to it is cheap, but it does not fix the reported failure on its own, and a NAD field is a CRD change to both Certification and WorkloadRun. The zero opt-out closes the issue; configurability can follow if a site needs a differently named NAD.
- **Delete the "0 means omit" comments and keep the literal-zero behavior.** Rejected: `nvidia.com/mlnxnics: "0"` is not a useful request on any cluster, and an operator reaching for the field wants the resource gone. Documenting that the field cannot do the one thing it looks like it does is worse than making it do it.

## Notes

- **On-prem is not zero-safe either.** With `nicResourceName: rdma/ib` and `mlnxPerNode: 0`, the `.NicResourceName` guard passes and `rdma/ib: "0"` is rendered. `resolveNICResourceName` floors the detection count at one (`pkg/controller/nic_detect.go:132`), so detection can hand back a name whose request then renders as zero. Those fragments are outside this change because their `limits` also carry `nvidia.com/gpu`, which makes the guard boundary a different question; this is recorded so it is not mistaken for coverage.
- **The count is never detected.** This ADR changes a default and a template branch. `mlnxPerNode` still resolves from the architecture default, the platform override, and the field, never from what the nodes advertise, exactly as ADR-075 left it.
- **The SR-IOV annotation and the resource move together.** Both are gated by the same condition in the OCI fragment, because requesting one without the other has no valid configuration.

## References

- ADR-012: Platform and GPU architecture overrides (override semantics and matching order)
- ADR-046: Shared template library (`_lib/` fragments rendered by both `pkg/catalog` and `pkg/platform`)
- ADR-075: On-prem GB200/GB300 override (`nicResourceName`, NIC auto-detection, the `.NicResourceName` guard precedent)
- GitHub issue #350
- Code citations:
  - `pkg/catalog/entries/_lib/gpu-defaults.yaml:9-10` (the "templates branch on it" claim), `:38-40` (gb200 arch default 8), `:50-57` (`platformOverrides.oci`, no gb200 entry)
  - `pkg/catalog/gpu_defaults.go:81-86` (non-zero test that drops an override of 0)
  - `pkg/catalog/catalog.go:46`, `pkg/catalog/loader.go:111` (the same claim, twice more)
  - `pkg/catalog/entries/_lib/deps/oci-mlnxnics-comm.yaml:18` (the `sriov-net` annotation), `:24,:26` (the resource)
  - `pkg/catalog/entries/_lib/deps/`: `forge-ib-comm.yaml:25,28`, `azure-a100-ib-training.yaml:25,27`, `azure-h100-ib-training.yaml:25,27`, `azure-ib-with-topo-comm.yaml:21,23`, `azure-ib-with-topo-a100-comm.yaml:21,23`, `togetherai-ib-runtime-patch.yaml:21,23`
  - `pkg/platform/overrides/workloadrun.yaml:296,298` (inline Azure block, unquoted value)
  - `pkg/controller/workflow_detect.go:654-665` (`mergeMaps` deletes nil-valued keys)
  - `pkg/catalog/loader.go:264-295` (template function map), `:374-378` (init-time empty render)
  - `pkg/catalog/entries/_lib/deps/onprem-gb200-gb300-runtime-patch-comm.yaml:26` (the `.NicResourceName` guard precedent)
  - `pkg/controller/nic_detect.go:132` (detection count floored at one)
  - `pkg/render/nodes/oci-gb200.yaml` (existing fixture for the render tests)
