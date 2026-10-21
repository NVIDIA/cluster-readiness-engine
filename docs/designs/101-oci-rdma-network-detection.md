# ADR-101: Detect the OCI RDMA NetworkAttachmentDefinition Instead of Assuming `network-operator/sriov-net`

> **Status:** Proposed

## Context

On OCI, every catalog entry that requests RDMA NICs pulls in `_lib/deps/oci-mlnxnics-comm.yaml`. When `mlnxPerNode > 0`, that fragment renders two things together on the worker pod (ADR-087):

- `nvidia.com/mlnxnics: "<mlnxPerNode>"` in `limits` and `requests`
- `k8s.v1.cni.cncf.io/networks: network-operator/sriov-net,...`, one entry per NIC

The NetworkAttachmentDefinition (NAD) name is hard-coded. ADR-087 kept it that way on purpose (its Decision 4), because NVCRE had seen no OCI cluster that named it differently. That no longer holds. AICR's `gb300-oke-ubuntu-training` recipe has no `network-operator` namespace. It ships `nvidia-network-operator/rdma-net`, a `host-device` NAD with empty IPAM (OCI assigns addresses by IPv6 SLAAC) and the annotation `k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics`. It uses `host-device` rather than the sriov CNI because, with oci-hpc-network-device-names 3.x, the NICs run in switchdev mode and the representor naming breaks the sriov CNI.

On an AICR cluster, every NVCRE pod on OCI fails to attach net1..netN until someone creates `network-operator/sriov-net` by hand. The pod still gets its `nvidia.com/mlnxnics` VFs, so the failure is not a scheduling one, and the person reading the result has to work out that the network name is the cause.

The usual Kubernetes split is that the platform owns the secondary network and the workload refers to it. The platform can be an admin, or a controller that renders NADs from a CR (the NVIDIA Network Operator's `HostDeviceNetwork` and `SriovNetwork`, with `networkNamespace`). Device plugins and NADs are tied together by the `k8s.v1.cni.cncf.io/resourceName` annotation: Multus uses it to hand the pod the device the resource request allocated. That annotation is the one fact that holds on every cluster that provides `nvidia.com/mlnxnics`, whatever the NAD is called.

There is a precedent for reading cluster state the catalog needs at render time. GKE TCPXO network names are detected by the controller from node allocatable, passed to the templates through `BuildConfig.GKETCPXONetworks` with no CRD field, and fall back to `DefaultGKETCPXONetworks()` when detection refuses (issue #432).

## Decision

1. **Resolve the NAD from the cluster before rendering, keyed on its `resourceName` annotation.** On an OCI target with a resolved `mlnxPerNode > 0`, the Certification controller lists NetworkAttachmentDefinitions in every namespace and keeps those annotated `k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics`, the resource the OCI fragment requests. The chosen `<namespace>/<name>` reaches the template through a new `BuildConfig.OCISecondaryNetwork` field. No API change.

2. **Resolution rules.** They are the same for the controller and `nvcrectl certification render --dry-run`:

   | Matches | Result |
   |---|---|
   | One | Use it. |
   | Several, all equivalent | Pick deterministically: a NAD in the Certification's namespace first, otherwise the first `<ns>/<name>` in sorted order. Emit a Normal `SecondaryNetworkDetection` event naming the choice and the equivalents it skipped. |
   | Several that differ | Fail. List the candidates and the top-level config keys that differ. |
   | Zero | Fail. Say what was found (NADs carrying another `resourceName`, or none) and: "create one, or set mlnxPerNode: 0 to certify without RDMA". |
   | List error, RBAC denial, or the NAD API not served | Fail with that error's own message. Never treat it as zero matches. |

   **Equivalent** means the same `resourceName` (true by construction) and the same `spec.config` after parsing it as JSON and removing the top-level `name` key. A config that does not parse as JSON is never equivalent to anything. Two NADs that differ only in name, namespace, whitespace or key order are equivalent. A different `type`, `ipam`, or any other top-level key makes them differ.

3. **Fail closed, before any workload exists.** A failure returns a typed error from `createWorkflowForCategory` before `entry.Build`. Both callers then call `setCertificationFailed` with a new reason, `SecondaryNetworkDetectionFailed`, and emit a Warning event with the same message. Like a Workflow name collision, this is terminal: the spec is immutable, so the operator fixes the cluster (or sets `mlnxPerNode: 0`) and re-creates the Certification. The controller never renders the default name on OCI. The `nvidia.com/mlnxnics` request is never rendered without its attachment, because that would certify a transport other than the one the operator asked for.

4. **Offline render keeps a named default and says so.** `nvcrectl certification render` without `--dry-run` has no cluster to read. It renders `catalog.DefaultOCISecondaryNetwork = "network-operator/sriov-net"`, today's value, and prints one stderr note: the name is a default, and the controller detects the real network when it reconciles. `--dry-run` runs the controller's resolution and fails closed with the controller's message, because dry-run exists to predict what reconcile will do.

   This differs from GKE TCPXO, where the controller falls back to the default with a Warning. GKE's default names networks GKE itself provisions on A3 Mega node pools. The OCI default names an object that may not exist, and attaching nothing is the failure this ADR fixes.

5. **Read-only, uncached, narrow RBAC.** The list goes through the manager's `APIReader` as an `unstructured` `k8s.cni.cncf.io/v1` `NetworkAttachmentDefinitionList`. Going uncached means no informer, so no `watch` verb and no permanent cache of every NAD in the cluster. Detection runs once per category Workflow creation, and only on OCI with `mlnxPerNode > 0`. `nvcre-manager-role` gains `get` and `list` on `k8s.cni.cncf.io/network-attachment-definitions`, and nothing else. This is a read, so it goes in the cluster-wide manager role, not in `nvcre-run-role` (ADR-092). Without that grant, the result is an explicit RBAC failure under rule 2.

6. **Document Multus `namespaceIsolation` rather than solve it.** With `namespaceIsolation: true`, Multus rejects a pod that references a NAD in another namespace, except the `default` namespace and any listed in `globalNamespaces`. Detection can pick a NAD in another namespace, and that is how both known clusters work. Sites with isolation enabled must add the NAD's namespace to `globalNamespaces` or provide a NAD in the workload namespace, which detection then prefers.

### Scope

- **WorkloadRun is unaffected.** `pkg/platform/overrides/workloadrun.yaml` has no OCI override: a WorkloadRun on OCI requests neither `nvidia.com/mlnxnics` nor the networks annotation, so it has nothing to resolve. Adding OCI RDMA to WorkloadRun is a separate change.
- **Other platforms are unaffected.** `entry.Build` renders every override block and prunes the ones that do not match. On non-OCI targets, the OCI block renders with the default name and is then pruned. Detection does not run there, so no event is emitted and no permission is needed.
- **The resource name stays `nvidia.com/mlnxnics`** on OCI. It becomes a named constant, so detection and the fragment cannot disagree.

## Implementation

- `pkg/catalog/catalog.go`, `loader.go`: add `BuildConfig.OCISecondaryNetwork`, `TemplateData.OCISecondaryNetwork` (set to `DefaultOCISecondaryNetwork` when empty), and the constants `DefaultOCISecondaryNetwork` and `OCIMlnxResourceName`.
- `_lib/deps/oci-mlnxnics-comm.yaml`: `{{ repeat (int .MlnxPerNode) (printf "%s," .OCISecondaryNetwork) | trimSuffix "," }}`, with the resource rendered from the same constant.
- `pkg/controller/oci_network_detect.go`: a pure `selectOCISecondaryNetwork(nads, workloadNamespace)` holding the rules and the equivalence test, a `listNetworkAttachmentDefinitions(ctx, reader)` that turns list errors into `ociNetworkDetection.Err`, and `resolveOCISecondaryNetwork(ctx, reader, platform, mlnxPerNode, namespace)`, gated on `platform == oci && mlnxPerNode > 0`. An exported wrapper in `workflow_detect_export.go` serves the CLI.
- `pkg/controller/certification_controller.go`: resolve after `mlnxPerNode` is final and before `Build`. Return a `secondaryNetworkError` on failure, and handle it at both call sites alongside `nameCollisionError`. Emit the equivalents event only from the reconcile whose Create succeeds, like the GCP H100 detection warnings.
- `pkg/controller/helpers.go`: `ReasonSecondaryNetworkDetectionFailed`, `ReasonSecondaryNetworkDetection`.
- `pkg/certification/certification.go`: under `--dry-run`, resolve and return an error on failure. Offline, print the default note when the rendered output contains an OCI attachment.
- `helm/cluster-readiness-engine/templates/manager-role.yaml`, plus a `+kubebuilder:rbac` marker on the Certification reconciler: `get;list` on `k8s.cni.cncf.io/network-attachment-definitions`.
- `hack/crds/`: the NAD CRD, so envtest can serve the API.

### Testing plan

- `pkg/controller` (golden, `TestCaseParser`). Detection cases:
  - one match
  - none, with no NADs at all and with NADs carrying another `resourceName`
  - several equivalent: whitespace and key order, names only, and preferring the workload namespace
  - several that differ: IPAM, CNI type, and config that is not JSON
  - a matching `resourceName` annotation on a different resource
  - a list error
  - `mlnxPerNode = 0` and a non-OCI platform, where no lookup happens: the reader fails the test if called
- `pkg/certification` (golden): the detected name flows into the annotation. The existing OCI goldens keep the default name offline.
- Integration: `certification-oci-l40s-nccl` gains a `rdma-net` NAD, so its golden carries the detected name. A new case with no matching NAD pins the Failed condition and its reason.
- `test/helm`: the role grants exactly `get` and `list` on NADs.

## Rationale

- **`resourceName` is what makes the attachment work, not a naming convention.** The NPWG multi-network spec does not define the annotation; Multus does. Multus reads it from the NAD, looks up the device IDs the kubelet allocated to the pod for that resource, and passes the next one to the CNI as `runtimeConfig.deviceID`, one per repeated attachment. Without it, no device ID is passed. `host-device` then needs a fixed `device`, `hwaddr`, `kernelpath` or `pciBusID` in the NAD, which names the same interface for every attachment, so the second of the `mlnxPerNode` attachments cannot move it. A NAD that actually attaches allocated `nvidia.com/mlnxnics` VFs therefore carries the annotation. NADs in general need not, but this detection only looks for the ones that pair with that resource.
- **Equivalent duplicates are common and harmless.** A Network Operator CR with a `networkNamespace` per tenant renders identical NADs in several namespaces. Refusing those would turn routine multi-tenant setups into failures, while a deterministic pick with an event keeps the choice visible.
- **Differing duplicates are not NVCRE's call.** Two NADs serving the same VFs with different CNI or IPAM mean the platform has two opinions. Choosing one would certify whichever transport sorted first.
- **Terminal failure matches what the operator has to do.** Nothing NVCRE does on retry changes the answer. The fix is a cluster change or a spec change, and the spec is immutable.

## Consequences

- AICR `gb300-oke-ubuntu-training` clusters attach `nvidia-network-operator/rdma-net` with no manual step. A cluster that already provides `network-operator/sriov-net` keeps attaching it, provided the NAD carries the `resourceName` annotation. NADs the Network Operator renders from `SriovNetwork` or `HostDeviceNetwork` always do. A NAD written without it now fails closed with the zero-match message, where before it attached by name.
- An OCI cluster that requests NICs but has no matching NAD now fails at Workflow creation with a reason and a message, instead of producing pods that start without their RDMA interfaces.
- The manager needs `get`/`list` on NADs. A chart upgrade carries it. An install that removes it gets an explicit RBAC failure on OCI with NICs, and nothing changes anywhere else.
- `--dry-run` now fails on an OCI cluster without a matching NAD. Offline render output does not change.

## Alternatives Considered

- **An optional override field** on Certification (e.g. `secondaryNetworks`), additive in `v1alpha1`, following the `nicResourceName` pattern: the field when set, detection when unset. This is the clean remedy for "several that differ" and for a wrong pick. It is deferred to keep this change free of API changes, and left to the reviewer as a follow-up.
- **NVCRE creates its own NAD** (host-device, empty IPAM, `resourceName` annotation) in the workload namespace when none matches. It sidesteps `namespaceIsolation` and works on clusters with no NAD at all. It also needs create and delete RBAC on NADs, lifecycle and cleanup handling, and it copies platform-specific CNI choices into NVCRE, including the switchdev and sriov issue above. Left to the reviewer as a follow-up.
- **Fall back to the default name in the controller, with a Warning**, as GKE TCPXO does. Rejected: the default may not exist, and an attachment that silently fails is the defect being fixed.
- **Match on namespace or name conventions** (`*/sriov-net`, `*/rdma-net`). Rejected: it encodes today's two clusters and breaks on the third.
- **Cached, watched NAD informer.** Rejected: it needs `watch`, holds every NAD in memory for a lookup made once per Workflow, and buys nothing, since the read happens only at creation time.

## Notes

- The equivalence test compares only the top level of `spec.config` after removing `name`. Nested differences are reported under their top-level key (for example `ipam`).
- `namespaceIsolation` is documented in `docs/concepts/platform-detection.md` next to the OCI rows.

## References

- Issue #542: OCI workload pods reference a hard-coded `network-operator/sriov-net` NAD
- ADR-075: NIC resource auto-detection
- ADR-087: `mlnxPerNode: 0` as a real opt-out, and OCI GB200 NIC defaults
- ADR-092: Per-namespace write access for the controller
- Issue #432: GKE TCPXO network detection
- `pkg/catalog/entries/_lib/deps/oci-mlnxnics-comm.yaml`
- Multus reads `k8s.v1.cni.cncf.io/resourceName` and passes the allocated device ID: https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/pkg/k8sclient/k8sclient.go
- `host-device` CNI, `runtimeConfig.deviceID` and its fixed-device fallback: https://github.com/containernetworking/plugins/blob/main/plugins/main/host-device/host-device.go
- NPWG multi-network spec, which does not define the annotation: https://github.com/k8snetworkplumbingwg/multi-net-spec
- Multus `namespaceIsolation` and `globalNamespaces`: https://github.com/k8snetworkplumbingwg/multus-cni/blob/master/docs/configuration.md
