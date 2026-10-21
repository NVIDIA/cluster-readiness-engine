# ADR-103: Detect the OCI RDMA NetworkAttachmentDefinition Instead of Assuming `network-operator/sriov-net`

> **Status:** Proposed

## Context

On OCI, every catalog entry that requests RDMA NICs pulls in `_lib/deps/oci-mlnxnics-comm.yaml`. When `mlnxPerNode > 0`, that fragment renders two things together on the worker pod (ADR-087):

- `nvidia.com/mlnxnics: "<mlnxPerNode>"` in `limits` and `requests`
- `k8s.v1.cni.cncf.io/networks: network-operator/sriov-net,...`, one entry per NIC

The NetworkAttachmentDefinition (NAD) name is hard-coded. ADR-087 kept it that way on purpose (its Decision 4). The name fits one cluster layout: a NVIDIA Network Operator install using the SR-IOV CNI, with the NAD rendered into the `network-operator` namespace. NVCRE works there today, but not on clusters laid out any other way. AICR, for example, installs the Network Operator into `nvidia-network-operator`, so any NAD it renders lands there rather than in `network-operator`. Separately, an OCI GB300 cluster running oci-hpc-network-device-names 3.x puts the NICs in switchdev mode, where the representor naming breaks the SR-IOV CNI. A platform built that way provides a `host-device` NAD instead, with empty IPAM (OCI assigns addresses by IPv6 SLAAC) and the annotation `k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics`, under whatever name and namespace it chose, such as `nvidia-network-operator/rdma-net`.

On such a cluster, every NVCRE pod on OCI fails to attach net1..netN until someone creates `network-operator/sriov-net` by hand. The pod still gets its `nvidia.com/mlnxnics` VFs, so the failure is not a scheduling one, and the person reading the result has to work out that the network name is the cause.

The usual Kubernetes split is that the platform owns the secondary network and the workload refers to it. The platform can be an admin, or a controller that renders NADs from a CR (the NVIDIA Network Operator's `HostDeviceNetwork` and `SriovNetwork`, with `networkNamespace`). Device plugins and NADs are tied together by the `k8s.v1.cni.cncf.io/resourceName` annotation: Multus uses it to hand the pod the device the resource request allocated. That annotation is the one fact that holds on every cluster that provides `nvidia.com/mlnxnics`, whatever the NAD is called.

ADR-101 (`networkAttachments`, PR #510) already gives the operator an explicit way out: a `networkAttachments` list on `CategoryOptions` names the NADs and their resources, and on OCI it replaces `oci-mlnxnics-comm.yaml` outright (the fragment is gated on `not .HasNetworkAttachments`). What remains is the default: a Certification that sets no list still attaches a name that may not exist.

There is a precedent for reading cluster state the catalog needs at render time. GKE TCPXO network names are detected by the controller from node allocatable, passed to the templates through `BuildConfig.GKETCPXONetworks` with no CRD field, and fall back to `DefaultGKETCPXONetworks()` when detection refuses (issue #432).

## Decision

Throughout, the **workload namespace** is the Certification's namespace. The Workflow is created in `certification.Namespace`, and the Job and TrainJob follow it, so the two are always the same.

1. **Resolve the NAD from the cluster when no `networkAttachments` list is set.** Detection runs only on an OCI target whose resolved category options set no `networkAttachments` (ADR-101) and resolve `mlnxPerNode > 0`, which is exactly when `oci-mlnxnics-comm.yaml` renders. It lists NetworkAttachmentDefinitions in every namespace and keeps those annotated `k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics`, the resource the OCI fragment requests. The chosen `<namespace>/<name>` reaches the template through a new `BuildConfig.OCISecondaryNetwork` field. An explicit list always wins and skips detection entirely, including its RBAC. No API change.

2. **Resolution rules.** They are the same for the controller and `nvcrectl certification render --dry-run`. The **candidates** are the matches in the workload namespace if there are any, otherwise all matches. A NAD in the workload namespace therefore always wins over NADs elsewhere, and under Multus `namespaceIsolation` it is the only one the pod can attach.

   | Candidates | Result |
   |---|---|
   | Exactly one | Use it. |
   | Several, all equivalent | Pick the first `<ns>/<name>` in sorted order. Emit a Normal `SecondaryNetworkDetection` event naming the choice and the equivalents it skipped. |
   | Several that differ | Fail. List the candidates and the top-level config keys that differ. When one of them is `network-operator/sriov-net`, add that a copy created by hand as a workaround for issue #542 should be deleted. |
   | Zero | Fail. Say what was found (NADs carrying another `resourceName`, or none) and the ways forward: create a NAD for `nvidia.com/mlnxnics`, list the networks explicitly with `networkAttachments`, use `rdmaDeviceAccess: hostPath` on a cluster that exposes `/dev/infiniband` without a device plugin (ADR-088), or set `mlnxPerNode: 0` to certify without RDMA NICs. |
   | List error | See Decision 3. Never treated as zero matches. |

   **Equivalent** means the same `resourceName` (true by construction) and the same `spec.config` after parsing it as JSON and removing the top-level `name` key. Two NADs that differ only in name, namespace, whitespace or key order are equivalent. A different `type`, `ipam`, or any other top-level key, `cniVersion` included, makes them differ. A config that does not parse as JSON is never equivalent to anything. That includes an empty `spec.config`, where Multus reads the CNI config from disk by the NAD's name: NVCRE cannot see that config, so it cannot check that two such NADs agree, and it fails closed.

   The "differ" and zero-match messages name `networkAttachments` as the fix for a cluster detection cannot read unambiguously.

3. **Fail closed, before any workload exists, and retry only what can change.** A failure returns from `createWorkflowForCategory` before `entry.Build`, so no Workflow is created.
   - **Terminal:** zero matches, several that differ, `Forbidden`, and the NAD API not being served (`meta.IsNoMatchError`). A retry cannot change these. The function returns a typed `secondaryNetworkError`. Both callers handle it alongside `nameCollisionError`: `setCertificationFailed` with a new reason, `SecondaryNetworkDetectionFailed`, and a Warning event with the same message. The operator fixes the cluster or the spec and re-creates the Certification, since the spec is immutable.
   - **Retryable:** any other list error (a 5xx, a timeout, a dropped connection). The function returns `retryableCreateError`, and the callers requeue without touching status, the same path a failed live read takes today (#384).

   The controller never renders the default name on OCI. The `nvidia.com/mlnxnics` request is never rendered without its attachment, because that would certify a transport other than the one the operator asked for.

4. **No default in the catalog; every caller sets the name.** `buildTemplateData` does not fill in `OCISecondaryNetwork`, and `Build` returns an error when `MlnxPerNode > 0` and the field is empty. Because `Build` renders every platform's override blocks, the OCI block renders on every target, so each caller sets the field explicitly:
   - **Controller, OCI target:** the name detection returned.
   - **Controller, other targets, or OCI with a `networkAttachments` list:** `catalog.DefaultOCISecondaryNetwork`. The OCI block is pruned or gated off, so the value never reaches a pod.
   - **Offline `nvcrectl certification render`:** `catalog.DefaultOCISecondaryNetwork = "network-operator/sriov-net"`, today's value, with one stderr note: the name is a default, and the controller detects the real network when it reconciles.
   - **`--dry-run`:** the controller's resolution, with its messages, so dry-run predicts reconcile. The one exception is `Forbidden`. Dry-run lists NADs with the user's kubeconfig, not the controller's service account, and a user without cluster-wide `list` on NADs would otherwise see a failure the controller never hits. Dry-run prints a warning for it and renders the default. Every other outcome is fatal, as in the controller.

   This differs from GKE TCPXO, where the controller falls back to the default with a Warning. GKE's default names networks GKE itself provisions on A3 Mega node pools. The OCI default names an object that may not exist, and attaching nothing is the failure this ADR fixes.

5. **Read-only, uncached, narrow RBAC.** The list goes through the manager's `APIReader` as an `unstructured` `k8s.cni.cncf.io/v1` `NetworkAttachmentDefinitionList`. Going uncached means no informer, so no `watch` verb and no permanent cache of every NAD in the cluster. Detection runs once per category Workflow creation, under the gate in Decision 1. `nvcre-manager-role` gains `get` and `list` on `k8s.cni.cncf.io/network-attachment-definitions`, and nothing else. This is a read, so it goes in the cluster-wide manager role, not in `nvcre-run-role` (ADR-092).

6. **Document Multus `namespaceIsolation` rather than solve it.** With `namespaceIsolation: true`, Multus rejects a pod that references a NAD in another namespace unless that namespace is listed in `globalNamespaces`. `default` is allowed implicitly only while `globalNamespaces` is unset; once it is set, `default` must be listed too. Sites with isolation enabled have two options: list the platform NAD's namespace in `globalNamespaces`, or provide a NAD in the workload namespace, which detection then uses whatever exists elsewhere (Decision 2). An explicit `networkAttachments` list is a third.

### Scope

- **WorkloadRun is unaffected.** `pkg/platform/overrides/workloadrun.yaml` has no OCI override: a WorkloadRun on OCI requests neither `nvidia.com/mlnxnics` nor the networks annotation unless its own `networkAttachments` says so (ADR-101), so it has nothing to resolve.
- **Other platforms are unaffected.** Detection does not run there, so no event is emitted and no permission is needed.
- **The resource name stays `nvidia.com/mlnxnics`** on OCI. It becomes a named constant, so detection and the fragment cannot disagree.

## Implementation

This change is written against ADR-101's (#510) versions of `oci-mlnxnics-comm.yaml`, `catalog.go`, `loader.go`, `pkg/certification/certification.go` and `platform-detection.md`, and lands after it.

- `pkg/catalog/catalog.go`, `loader.go`: add `BuildConfig.OCISecondaryNetwork`, `TemplateData.OCISecondaryNetwork` (copied as is, never defaulted), the `Build` check from Decision 4, and the constants `DefaultOCISecondaryNetwork` and `OCIMlnxResourceName`.
- `_lib/deps/oci-mlnxnics-comm.yaml`: keep ADR-101's gate, `{{- if and (gt (int .MlnxPerNode) 0) (not .HasNetworkAttachments) }}`, and render `{{ repeat (int .MlnxPerNode) (printf "%s," .OCISecondaryNetwork) | trimSuffix "," }}`. Rewrite the header comment, which names `sriov-net` as fact.
- `pkg/controller/oci_network_detect.go`:
  - a pure `selectOCISecondaryNetwork(nads, workloadNamespace)` holding the rules and the equivalence test
  - a `listNetworkAttachmentDefinitions(ctx, reader)` that classifies list errors as terminal or retryable
  - `resolveOCISecondaryNetwork(ctx, reader, platform, mlnxPerNode, networkAttachments, namespace)`, gated as in Decision 1

  An exported wrapper in `workflow_detect_export.go` serves the CLI.
- `pkg/controller/certification_controller.go`: resolve after `mlnxPerNode` and the options are final, and before `Build`. Return `secondaryNetworkError` or `retryableCreateError` as in Decision 3, and handle the former at both call sites alongside `nameCollisionError`. Emit the equivalents event only from the reconcile whose Create succeeds, like the GCP H100 detection warnings.
- `pkg/controller/helpers.go`: `ReasonSecondaryNetworkDetectionFailed`, `ReasonSecondaryNetworkDetection`.
- `pkg/certification/certification.go`: set the field per Decision 4 on both paths. Under `--dry-run`, resolve, warn on `Forbidden`, and return an error on every other failure.
- `helm/cluster-readiness-engine/templates/manager-role.yaml`, plus a `+kubebuilder:rbac` marker on the Certification reconciler: `get;list` on `k8s.cni.cncf.io/network-attachment-definitions`.
- `hack/crds/`: the NAD CRD, so envtest can serve the API.
- Text that names `sriov-net` as what OCI renders:
  - `api/v1alpha1/certification_types.go` (the `mlnxPerNode` doc comment), then `make manifests` for the generated CRD description
  - `pkg/catalog/entries/_lib/gpu-defaults.yaml` (the OCI GB200 comment)
  - `docs/concepts/platform-detection.md` (the OCI rows, plus the detection rules and `namespaceIsolation` next to them)
  - `docs/api-reference/certification.md` (`mlnxPerNode`)

### Testing plan

- `pkg/controller` (golden, `TestCaseParser`). Detection cases:
  - one match, in the workload namespace and elsewhere
  - a single match in the workload namespace that differs from a NAD elsewhere: the local one wins
  - none: no NADs at all, NADs carrying another `resourceName`, and a NAD with the right value under the wrong key (`k8s.cni.cncf.io/resourceName`, the DRA attribute key, instead of `k8s.v1.cni.cncf.io/resourceName`)
  - several equivalent outside the workload namespace: whitespace and key order, names only
  - several equivalent in the workload namespace, beside a NAD elsewhere that differs: the sorted pick among the local ones, with the event
  - several that differ: IPAM, CNI type, `cniVersion`, config that is not JSON, empty `spec.config`, and a hand-made `network-operator/sriov-net` beside the platform's NAD, which adds the delete hint
  - list errors: `Forbidden` and NoMatch (terminal), a 5xx and a timeout (retryable)
  - no lookup, where the reader fails the test if called: `mlnxPerNode = 0`, a non-OCI platform, and a `networkAttachments` list
- `pkg/controller` (golden): an OCI render with `mlnxPerNode > 0` carries only the name detection returned, never `DefaultOCISecondaryNetwork`.
- `pkg/catalog`: `Build` fails when `MlnxPerNode > 0` and `OCISecondaryNetwork` is empty.
- `pkg/certification` (golden): the detected name flows into the annotation under `--dry-run`, and the existing OCI goldens keep the default offline.
- Integration: `certification-oci-l40s-nccl` gains a `rdma-net` NAD, so its golden carries the detected name. New cases pin the Failed condition and reason with no matching NAD, and the requeue on a retryable list error.
- `test/helm`: the role grants exactly `get` and `list` on NADs.

## Rationale

- **`resourceName` is what makes the attachment work, not a naming convention.** The NPWG multi-network spec does not define the annotation; Multus does. Multus reads it from the NAD, looks up the device IDs the kubelet allocated to the pod for that resource, and passes the next one to the CNI as `runtimeConfig.deviceID`, one per repeated attachment. Without it, no device ID is passed. `host-device` then needs a fixed `device`, `hwaddr`, `kernelpath` or `pciBusID` in the NAD, which names the same interface for every attachment, so the second of the `mlnxPerNode` attachments cannot move it. A NAD that actually attaches allocated `nvidia.com/mlnxnics` VFs therefore carries the annotation. NADs in general need not, but this detection only looks for the ones that pair with that resource.
- **A NAD in the workload namespace is a deliberate local choice.** Under `namespaceIsolation` it is the only one the pod can use, and a site that adds one has most likely tailored it, so it would rarely be equivalent to the cluster-wide NAD. Applying the equivalence rule across both would fail exactly the sites Decision 6 tells to add one.
- **Equivalent duplicates elsewhere are common and harmless.** A Network Operator CR with a `networkNamespace` per tenant renders identical NADs in several namespaces. Refusing those would turn routine multi-tenant setups into failures, while a deterministic pick with an event keeps the choice visible.
- **Differing duplicates are not NVCRE's call.** Two NADs serving the same VFs with different CNI or IPAM mean the platform has two opinions. Choosing one would certify whichever transport sorted first. `networkAttachments` lets the operator settle it.
- **Terminal only where a retry cannot help.** Zero matches, differing matches, a missing grant and a missing API need a cluster or spec change, and the spec is immutable. A transient API error says nothing about the cluster, and ending a run on one is the failure #384 fixed for Workflow reads.
- **No catalog default, so forgetting to detect is an error.** A default in the loader would let any controller path that skipped detection render `network-operator/sriov-net` silently. Making each caller set the name keeps Decision 3's promise checkable.

## Consequences

- Clusters whose RDMA network has another name or namespace attach it with no manual step and no `networkAttachments` list. A cluster that already provides `network-operator/sriov-net` keeps attaching it, provided the NAD carries the `resourceName` annotation. NADs the Network Operator renders from `SriovNetwork` or `HostDeviceNetwork` always do. A NAD written without it now fails closed with the zero-match message, where before it attached by name.
- A cluster that applied the issue #542 workaround, a hand-made `network-operator/sriov-net` beside the platform's NAD, goes from working to "several that differ" if the copy differs in any top-level key. The message tells the operator to delete the copy or list the network with `networkAttachments`.
- An OCI cluster that requests NICs but has no matching NAD now fails at Workflow creation with a reason and a message, instead of producing pods that start without their RDMA interfaces.
- The manager needs `get`/`list` on NADs. A chart upgrade carries it. An install that removes it gets an explicit `Forbidden` failure on OCI with NICs and no `networkAttachments`, and nothing changes anywhere else.
- `--dry-run` now fails on an OCI cluster without a matching NAD. Offline render output does not change.

## Alternatives Considered

- **Require `networkAttachments` on OCI** and drop the default wiring. Rejected: every OCI Certification would have to restate what the platform already publishes through the `resourceName` annotation. ADR-101 stays the explicit override, and detection is the default.
- **NVCRE creates its own NAD** (host-device, empty IPAM, `resourceName` annotation) in the workload namespace when none matches. It sidesteps `namespaceIsolation` and works on clusters with no NAD at all. It also needs create and delete RBAC on NADs, lifecycle and cleanup handling, and it copies platform-specific CNI choices into NVCRE, including the switchdev and sriov issue above. Left to the reviewer as a follow-up.
- **Fall back to the default name in the controller, with a Warning**, as GKE TCPXO does. Rejected: the default may not exist, and an attachment that silently fails is the defect being fixed.
- **Match on namespace or name conventions** (`*/sriov-net`, `*/rdma-net`). Rejected: it encodes today's two layouts and breaks on the third.
- **Cached, watched NAD informer.** Rejected: it needs `watch`, holds every NAD in memory for a lookup made once per Workflow, and buys nothing, since the read happens only at creation time.

## Notes

- The equivalence test compares only the top level of `spec.config` after removing `name`. Nested differences are reported under their top-level key (for example `ipam`).
- ADR-101 notes that NVCRE cannot check that a listed `resourceName` matches its NAD. This ADR's listing could, but that is outside its scope.

## References

- Issue #542: OCI workload pods reference a hard-coded `network-operator/sriov-net` NAD
- ADR-101 (#510): Multus network attachments paired with extended resources
- ADR-075: NIC resource auto-detection
- ADR-087: `mlnxPerNode: 0` as a real opt-out, and OCI GB200 NIC defaults
- ADR-088: OCI GB200 RoCE overrides, and `rdmaDeviceAccess` for hostPath RDMA
- ADR-092: Per-namespace write access for the controller
- Issue #384: retryable Workflow creation errors
- Issue #432: GKE TCPXO network detection
- `pkg/catalog/entries/_lib/deps/oci-mlnxnics-comm.yaml`
- Multus reads `k8s.v1.cni.cncf.io/resourceName` and passes the allocated device ID: https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/pkg/k8sclient/k8sclient.go
- `host-device` CNI, `runtimeConfig.deviceID` and its fixed-device fallback: https://github.com/containernetworking/plugins/blob/main/plugins/main/host-device/host-device.go
- NPWG multi-network spec, which does not define the annotation: https://github.com/k8snetworkplumbingwg/multi-net-spec
- Multus `namespaceIsolation` and `globalNamespaces`: https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/docs/configuration.md
