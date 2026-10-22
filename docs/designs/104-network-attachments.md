# ADR-104: Multus Network Attachments Paired With Extended Resources

> **Status:** Proposed

## Context

Modern AI clusters attach workload pods to the east-west fabric through the NVIDIA Network Operator: Multus secondary networks (a NetworkAttachmentDefinition per rail, addressed by NV-IPAM) plus an extended resource per rail from the RDMA or SR-IOV device plugin. A pod reaches the fabric only when its spec names both. Multus does not match the two by position: each NetworkAttachmentDefinition names its own resource in its `k8s.v1.cni.cncf.io/resourceName` annotation, and Multus hands the CNI a device that the kubelet allocated to the pod for that resource. What has to hold is that every attached network's resource is requested in a large enough count. `resources.limits` is a map and keeps no order. The order of the `k8s.v1.cni.cncf.io/networks` annotation still matters, because it decides which network becomes `net1`, `net2` and so on inside the pod, and that is what rail and HCA mapping keys on.

NVCRE can express only part of that ([issue #400](https://github.com/NVIDIA/cluster-readiness-engine/issues/400)):

- `nicResourceName` plus `mlnxPerNode` request **one** extended resource, `mlnxPerNode` times, on the on-prem GB200/GB300 and HGX B200/B300 entries (ADR-075, ADR-094).
- The only Multus annotation is hard-coded for OCI: `network-operator/sriov-net` repeated `mlnxPerNode` times (`pkg/catalog/entries/_lib/deps/oci-mlnxnics-comm.yaml`).

A Spectrum-X site with one resource and one NAD per rail (`nvidia.com/r0` to `nvidia.com/r7`, NADs `r0` to `r7`) therefore cannot describe its fabric. NCCL falls back to the pod network, which runs over the north-south interfaces, and the certification never exercises the fabric it is meant to certify. The issue author asked for the networks and resources to be settable on both Certification and WorkloadRun, with the order of the networks matching the order of the resources.

## Decision

1. **Add `networkAttachments` to `CategoryOptions` and to `WorkloadRunSpec`.** It is a list of `{network, resourceName}`:
   - `network` names a NAD as `<name>` or `<namespace>/<name>` (DNS-1123 label namespace, DNS-1123 subdomain name).
   - `resourceName` follows the `nicResourceName` rules: a fully qualified extended resource name outside the reserved `kubernetes.io` and `k8s.io` domains. It must be the resource the named NAD declares.
   - Each entry is one interface and requests one unit of its resource. Entries sharing a `resourceName` add up, so a second interface on one network is a second entry. There is no per-entry count: a count above one would reserve devices that no interface uses.
   - At most 16 entries.

2. **Render.** Each TrainingRuntime replicatedJob other than the MPI `launcher` gets the networks annotation, in list order, on its pod template, and its worker container (named `node`, else the first) gets limits and requests for each resource. Those add to a quantity the catalog already rendered for the same resource, so an entry on `nvidia.com/mlnxnics` on TogetherAI raises its request of 8 to 9 rather than replacing it; where nothing was rendered, as on OCI with a list or with `mlnxPerNode: 0`, the entries' count is the whole request. A site whose Multus networks are the platform's own NICs therefore sets `mlnxPerNode: 0`, so the request is the listed entries alone: otherwise two entries on `nvidia.com/mlnxnics` on TogetherAI request 10, which an 8-NIC node cannot schedule. The annotation replaces any value already rendered.

3. **What the list replaces.** It is mutually exclusive with `nicResourceName` in one options block and disables NIC auto-detection. On OCI the default NIC wiring is itself Multus (`sriov-net` plus `nvidia.com/mlnxnics`), so the list replaces it rather than appending to it. Azure, TogetherAI and Forge request `nvidia.com/mlnxnics`, and nscale its `rdma` DRA claim, without Multus; their NCCL environment names those HCAs, so the list adds to that request, and `mlnxPerNode: 0` drops it as on any other run (ADR-087). A per-category `networkAttachments` or `nicResourceName` replaces whichever of the two the spec level set. The list cannot be combined with `rdmaDeviceAccess: hostPath`, which requests no device; that rule holds per options block at admission and on each category's merged options in the controller and in `nvcrectl`. A conflict found on a category's merged options fails the whole Certification, not just that category: the controller sets it `Failed` with reason `WorkflowValidationFailed` and a message naming the category, and `nvcrectl certification render` aborts the entire render, naming the category.

4. **CLI.** `nvcrectl certification run` gains `--network-attachments network=resource,...` and the issue's zipped pair `--multus-networks` / `--extended-resources`, which renders to the same list and fails on a length mismatch. `--cert-file` users write the YAML.

5. **Workflow names.** `categoryOptionsSuffix` gains a `net<N>-<hash8>` token over the (network, resourceName) pairs in list order, so two categories differing only in attachments do not share a Workflow. Because a category's `nicResourceName` replaces a spec-level list, it also gains a `nic<hash8>` token when a category sets `nicResourceName`, so a category that inherits the list and one that replaces it with a NIC resource do not share a Workflow either. Issue #430 is reworking how option differences reach the name; these tokens follow whatever it settles on.

6. **Precedence.** On Certification the list is applied after platform overrides resolve and is final; Certification has no user overrides. On WorkloadRun the platform overrides are applied after the runtime is built (by the Workflow controller, or by `nvcrectl workloadrun render` once it knows the platform), and an override replaces a scalar quantity. So the list is applied once the platform overrides are built: it goes on the runtime, and every platform override patch that requests one of the same resources is raised by the same count. Azure H100 plus one `nvidia.com/mlnxnics` entry therefore requests 9 on both paths, whether or not the override matches. The user's own `spec.overrides` are applied last, when the Workflow reconciles, and win, as they do over every other WorkloadRun field.

## Implementation

- `api/v1alpha1/network_attachment_types.go`: the `NetworkAttachment` type with Pattern and length markers. Two type-level CEL rules on `CategoryOptions` (inherited by `CertificationSpec` through the inline embed, so they bind at the spec level and under `categories[].options`) and one on `WorkloadRunSpec`.
- `pkg/platform/network_attachments.go`: `ValidateNetworkAttachments` (the per-entry and cap rules, for offline use) and `ApplyNetworkAttachmentsToDependencies`.
- `pkg/platform/transform.go`: `ApplyResolvedWorkflowTransforms` gains the attachments as step 4, after image replacement and before workload metadata. Both the controller and offline render already funnel through this stage, after platform overrides resolve, so the explicit list wins over any annotation an override rendered.
- WorkloadRun applies the attachments to its runtime dependency and, through `ApplyNetworkAttachmentsToOverrides`, to its platform override patches, after `BuildOverrides` (controller and `nvcrectl workloadrun render`).
- `pkg/catalog`: `BuildConfig.NetworkAttachments` and `TemplateData.HasNetworkAttachments`; `oci-mlnxnics-comm.yaml` is gated on its absence.
- `ResolveOptions` clears the inherited counterpart when a category sets either field. `CheckResolvedNetworkAttachments` checks the merged options per category in `createWorkflowForCategory` and `renderCertification`. NIC detection is skipped in both controllers and both dry-run paths when the list is set.
- `readCertification` and `readWorkloadRun` mirror the CRD rules, since offline render never reaches an API server.

## Rationale

- One list of entries rather than two parallel lists keeps each network next to the resource its NAD declares, so a reader sees both in one place and the CLI rejects a length mismatch up front. Multus does not need the pairing; the user and the resource request do.
- Applying the attachments in the post-resolve stage, rather than in every catalog fragment, covers every entry and platform with one code path and keeps a platform override from clobbering an explicit choice.
- Hashing the pairs in list order, not sorted, follows from the annotation order: two lists with the same entries in a different order give the pod's interfaces different names, and are different configurations.
- Gating only the OCI fragment, not every platform's NIC request, keeps an InfiniBand platform working when a user adds a Multus network to it; `mlnxPerNode: 0` remains the one way to drop a platform NIC request.

## Consequences

- Sites with per-rail resources and NADs can certify their east-west fabric from the Certification or WorkloadRun alone.
- NCCL tuning for the rails (`NCCL_IB_HCA`, the RoCE GID index) is not first-class. It is site-specific and remains available through `env` (and `spec.overrides` on WorkloadRun).
- On Azure, TogetherAI, Forge and nscale the platform's own NIC request stays alongside the attachments unless `mlnxPerNode: 0` is set.
- Workflow names change for categories that already set `nicResourceName`. A category whose Workflow is already recorded keeps it, since the controller finds it through the recorded `workflowRef`.
- Known gaps, not addressed here:
  - **A wrong `resourceName` is not caught.** `{network: r0, resourceName: nvidia.com/r1}` passes every check, and the pods stay in `ContainerCreating` until the timeout because Multus finds no device for `r0`'s resource.
  - **Transport fallback.** The on-prem B300 entries set no NCCL environment, and pass/fail thresholds exist only if the user sets them. If RDMA setup fails, NCCL falls back to `NET/Socket` and the run can still pass. A follow-up should fail a run with attachments on transport fallback, by pinning the transport or checking for `NET/IB` in the log.
  - **Resources no node advertises.** A listed resource that no target node advertises leaves the pods Pending until the timeout. A follow-up should check the resources against node allocatable, which the controller already reads for NIC detection, and fail or warn up front.

## Alternatives Considered

- **Two parallel lists** (`nicResourceNames`, `multusNetworks`). Closest to the issue's CLI, but the alignment is easy to get wrong. Rejected for the API, kept as CLI sugar.
- **Derive the resources from the NADs.** Users would name only the networks, and the controller would read each NAD's `k8s.v1.cni.cncf.io/resourceName` and request it, which also closes the mismatch gap. Rejected for now: it needs RBAC to read `network-attachment-definitions` in every namespace a workload can name, a dependency on the Multus CRD being installed, and a cluster at render time, which offline `nvcrectl certification render` does not have. Sites that run the network-resources-injector admission webhook (k8snetworkplumbingwg) already get the requests derived from the NADs, outside NVCRE. If the explicit `resourceName` proves to be the common source of mistakes, making it optional and deriving it when the controller can read the NAD is a compatible follow-up.
- **Gate every platform's NIC fragment on the list.** Makes "the list replaces the NIC wiring" true everywhere, but on Azure, TogetherAI, Forge and nscale that wiring is how NCCL reaches InfiniBand and their NCCL environment names those HCAs, so adding one Multus network would silently break the run.
- **Platform override only** (`pkg/platform/overrides` per site). Fits fixed fabrics but not an operator certifying their own on-prem cluster, which is the issue's case.
- **Template variable in each fragment.** The NIC fragments differ per platform and entry; a Go transform in the existing post-resolve stage is one place instead of many.

## Notes

- The proposal on the issue was agreed by the issue author ("Shape A"), including the WorkloadRun half.
- The suffix tokens are deliberately local. If #430 lands a broader options hash or rejects duplicate categories, these tokens are replaced by that mechanism.

## References

- [Issue #400](https://github.com/NVIDIA/cluster-readiness-engine/issues/400), issue #430
- ADR-075 (on-prem NIC resource and detection), ADR-077 and ADR-079 (post-resolve transforms), ADR-087 (`mlnxPerNode: 0`), ADR-088 (`rdmaDeviceAccess`), ADR-094 (HGX B300)
