# ADR-101: Multus Network Attachments Paired With Extended Resources

> **Status:** Proposed

## Context

Modern AI clusters attach workload pods to the east-west fabric through the NVIDIA Network Operator: Multus secondary networks (a NetworkAttachmentDefinition per rail, addressed by NV-IPAM) plus an extended resource per rail from the RDMA or SR-IOV device plugin. A pod reaches the fabric only when its spec names both, and the Network Operator wires them together by position: the n-th network in the `k8s.v1.cni.cncf.io/networks` annotation is attached to a device of the n-th requested resource.

NVCRE can express only part of that ([issue #400](https://github.com/NVIDIA/cluster-readiness-engine/issues/400)):

- `nicResourceName` plus `mlnxPerNode` request **one** extended resource, `mlnxPerNode` times, on the on-prem GB200/GB300 and HGX B200/B300 entries (ADR-075, ADR-094).
- The only Multus annotation is hard-coded for OCI: `network-operator/sriov-net` repeated `mlnxPerNode` times (`pkg/catalog/entries/_lib/deps/oci-mlnxnics-comm.yaml`).

A Spectrum-X site with one resource and one NAD per rail (`nvidia.com/r0` to `nvidia.com/r7`, NADs `r0` to `r7`) therefore cannot describe its fabric. NCCL falls back to the pod network, which runs over the north-south interfaces, and the certification never exercises the fabric it is meant to certify. The issue author asked for the networks and resources to be settable on both Certification and WorkloadRun, with the order of the networks matching the order of the resources.

## Decision

1. **Add `networkAttachments` to `CategoryOptions` and to `WorkloadRunSpec`.** It is a list of `{network, resourceName, count}`:
   - `network` names a NAD as `<name>` or `<namespace>/<name>` (DNS-1123 label namespace, DNS-1123 subdomain name).
   - `resourceName` follows the `nicResourceName` rules: a fully qualified extended resource name outside the reserved `kubernetes.io` and `k8s.io` domains.
   - `count` is 1 to 64 and defaults to 1. Entries sharing a `resourceName` add up.
   - At most 16 entries.

2. **Render.** Each TrainingRuntime replicatedJob other than the MPI `launcher` gets the networks annotation, in list order, on its pod template, and its worker container (named `node`, else the first) gets limits and requests for each resource. The annotation replaces any value already rendered.

3. **The list owns the NIC wiring.** It is mutually exclusive with `nicResourceName` in one options block and disables NIC auto-detection. On OCI it replaces the default `sriov-net` attachments and their `nvidia.com/mlnxnics` request rather than appending to them. A per-category `networkAttachments` or `nicResourceName` replaces whichever of the two the spec level set. It cannot be combined with `rdmaDeviceAccess: hostPath` (which requests no device) or with `mlnxPerNode: 0` (the NIC opt-out).

4. **CLI.** `nvcrectl certification run` gains `--network-attachments network=resource[:count],...` and the issue's zipped pair `--multus-networks` / `--extended-resources`, which renders to the same list and fails on a length mismatch. `--cert-file` users write the YAML.

5. **Workflow names.** `categoryOptionsSuffix` gains a `net<N>-<hash8>` token over the (network, resourceName, count) tuples in list order, so two categories differing only in attachments do not share a Workflow. Issue #430 is reworking how option differences reach the name; this token follows whatever it settles on.

## Implementation

- `api/v1alpha1/network_attachment_types.go`: the `NetworkAttachment` type with Pattern, length and range markers, and `EffectiveCount()`. Three type-level CEL rules on `CategoryOptions` (inherited by `CertificationSpec` through the inline embed, so they bind at the spec level and under `categories[].options`) and two on `WorkloadRunSpec`.
- `pkg/platform/network_attachments.go`: `ValidateNetworkAttachments` (the per-entry and cap rules, for offline use) and `ApplyNetworkAttachmentsToDependencies`.
- `pkg/platform/transform.go`: `ApplyResolvedWorkflowTransforms` gains the attachments as step 4, after image replacement and before workload metadata. Both the controller and offline render already funnel through this stage, after platform overrides resolve, so the explicit list wins over any annotation an override rendered.
- WorkloadRun applies the attachments to its runtime dependency at construction (controller and `nvcrectl workloadrun render`). No WorkloadRun platform override renders a networks annotation, so nothing later competes with it.
- `pkg/catalog`: `BuildConfig.NetworkAttachments` and `TemplateData.HasNetworkAttachments`; `oci-mlnxnics-comm.yaml` is gated on its absence.
- `ResolveOptions` clears the inherited counterpart when a category sets either field. NIC detection is skipped in both controllers and both dry-run paths when the list is set.
- `readCertification` and `readWorkloadRun` mirror the CRD rules, since offline render never reaches an API server.

## Rationale

- One list of pairs rather than two parallel lists keeps each network next to its resource. Parallel lists must stay aligned by index and fail silently when they do not; the CLI accepts the flat pair as sugar and rejects a mismatch up front.
- Applying the attachments in the post-resolve stage, rather than in every catalog fragment, covers every entry and platform with one code path and keeps a platform override from clobbering an explicit choice.
- Hashing the tuples in list order, not sorted, follows from the positional pairing: two lists with the same entries in a different order attach different devices to different networks, and are different configurations.

## Consequences

- Sites with per-rail resources and NADs can certify their east-west fabric from the Certification or WorkloadRun alone.
- NCCL tuning for the rails (`NCCL_IB_HCA`, the RoCE GID index) is not first-class. It is site-specific and remains available through `spec.overrides` and `env`.
- A Certification that lists attachments on a platform whose catalog renders its own NIC request other than OCI (Azure, TogetherAI, Forge `nvidia.com/mlnxnics`) still gets that request alongside the attachments. Those platforms do not use Multus today; if one does, its fragment gains the same `HasNetworkAttachments` gate.

## Alternatives Considered

- **Two parallel lists** (`nicResourceNames`, `multusNetworks`). Closest to the issue's CLI, but the alignment is easy to get wrong. Rejected for the API, kept as CLI sugar.
- **Platform override only** (`pkg/platform/overrides` per site). Fits fixed fabrics but not an operator certifying their own on-prem cluster, which is the issue's case.
- **Template variable in each fragment.** The NIC fragments differ per platform and entry; a Go transform in the existing post-resolve stage is one place instead of many.

## Notes

- The proposal on the issue was agreed by the issue author ("Shape A"), including the WorkloadRun half.
- The suffix token is deliberately local. If #430 lands a broader options hash or rejects duplicate categories, this token is replaced by that mechanism.

## References

- [Issue #400](https://github.com/NVIDIA/cluster-readiness-engine/issues/400), issue #430
- ADR-075 (on-prem NIC resource and detection), ADR-077 and ADR-079 (post-resolve transforms), ADR-087 (`mlnxPerNode: 0`), ADR-088 (`rdmaDeviceAccess`), ADR-094 (HGX B300)
