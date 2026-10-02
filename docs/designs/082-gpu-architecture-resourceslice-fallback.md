# ADR-082: GPU Architecture Fallback from DRA ResourceSlices

> **Status:** Proposed

## Context

Every GPU-architecture decision NVCRE makes reads one node label, `nvidia.com/gpu.product`: the majority-architecture vote (`gpu.MajorityArchitecture`, `detectGPUArchConsistent`), catalog hardware defaults (`catalog.GPUDefaults`), `gpuArchitecture` override matching, NIC detection, and the CLI's homogeneity check (`cluster.UniformGPUProduct`). NVIDIA GPU Feature Discovery (GFD) writes that label, and GFD ships with the device-plugin stack.

Platforms that claim GPUs through Dynamic Resource Allocation (DRA) instead of a device plugin run the NVIDIA DRA GPU driver and no GFD DaemonSet, so no node carries `nvidia.com/gpu.product`. On such a platform every path above resolves the architecture to `unknown`: the Certification controller fails with `cannot determine GPU architecture`, and offline render has nothing to read at all.

The architecture is still published, just elsewhere. The driver publishes node-local `ResourceSlice` objects with `spec.driver: gpu.nvidia.com`, and each GPU device carries an unqualified `productName` string attribute holding the NVML device name, for example `NVIDIA GB300`. GFD writes the same hardware's label hyphenated, `NVIDIA-GB300`.

NVCRE does not patch nodes (ADR-061), so writing the label back to the Node objects is not an option.

## Decision

1. **Augment at node discovery.** `discoverTargetNodes`, the single function every controller and CLI path already uses for node discovery, fills `nvidia.com/gpu.product` on the in-memory copy of any target node that lacks it, from the `productName` attribute on that node's `gpu.nvidia.com` ResourceSlice. Nothing is persisted. Consumers keep reading the label and need no change.
2. **Normalize to the GFD format.** `productName` is sanitized exactly as GFD builds its label: characters outside `[A-Za-z0-9-_. ]` are dropped and each run of whitespace becomes one hyphen, so identical hardware yields byte-identical labels in a fleet that mixes GFD and DRA nodes. `UniformGPUProduct` compares raw label values and would otherwise report `heterogeneous GPUs` for one product.
3. **Degrade, do not fail.** The lookup is skipped when every node already has the label and costs one `ResourceSlice` List per discovery call. The List reads straight from the API server, never through the informer cache, so a missing grant or an unserved API fails fast instead of starting an informer that never syncs, and a short timeout bounds a slow API server. A List error (for example a missing RBAC grant) or a driver that publishes no `productName` is logged and leaves the nodes unlabeled. That is the same `unknown` outcome as before this change, not a new failure mode.
4. **`nvidia.com/gpu.present` stays mandatory.** Discovery filters on `nvidia.com/gpu.present=true` before the fallback runs, and the controller's Node cache is scoped to that label. The fallback supplies the product, not GPU-node identity. The GPU Operator sets `gpu.present`. A cluster running only the standalone DRA driver must label its GPU nodes itself.
5. **Offline `--gpu-arch`.** `nvcrectl certification render` and `nvcrectl workloadrun render` have no cluster to read ResourceSlices from, so both gain `--gpu-arch` with one rule:
   - The value is normalized with `gpu.ParseProduct`, so `gb300`, `NVIDIA-GB300`, and `NVIDIA GB300` are equivalent. It is rejected unless `gpu-defaults.yaml` lists the architecture, the same way `--platform` is validated.
   - When set, it wins over the `nodeSelector`-derived architecture, matching `--platform`.
   - It applies offline only. `--dry-run` always detects from real nodes, which the fallback now covers.
   - It is never written into `spec.target.nodeSelector`, which is the real API selector for every later reconcile.
6. **`certification run` needs no change.** `nvcrectl certification run --category` already persists `nvidia.com/gpu.present=true` alone, never the discovered product, so its selector matches DRA-only nodes and the controller detects the architecture through the same fallback.
7. **RBAC.** The manager role gains `get`, `list`, `watch` on `resource.k8s.io` `resourceslices`.

## Implementation

- `pkg/controller/gpu_resourceslice_detect.go`: `augmentGPUProductLabels`, called from `discoverTargetNodes` after the `gpu.present` filter. Reconcilers pass their uncached `APIReader` for the List; CLI clients are already uncached. It documents the driver attribute contract it depends on.
- `pkg/controller/workflow_controller.go`: the `resourceslices` kubebuilder RBAC marker documents the grant. `make manifests` runs with `output:rbac:none`, so `manager-role.yaml` is maintained by hand to match.
- `pkg/gpu`: `ParseProduct` accepts the space-separated form. `ProductLabelValue` mirrors GFD's sanitization. The label key is exported as `gpu.ProductLabel`.
- `pkg/catalog/gpu_defaults.go`: `ParseGPUArchFlag` normalizes and validates `--gpu-arch` against the `gpu-defaults.yaml` keys.
- `pkg/certification/certification.go`: `--gpu-arch` threads through `renderCertification`, `resolveWorkflowsOffline`, and `syntheticRenderNode` as a parameter.
- `pkg/workloadrun/workloadrun.go`: `--gpu-arch` on offline render.
- Tests: golden cases under `pkg/controller/testdata/augment-gpu-product-labels/`, `pkg/certification/testdata/{certification-render-onprem,render-platform-flag}/`, `pkg/workloadrun/testdata/render-platform-flag/`, and `pkg/gpu/testdata/{parse-product,product-label-value}/`.
- Docs: `docs/concepts/platform-detection.md`, the CLI references, and the RBAC table in `docs/operations/deployment.md`.

## Rationale

- Augmenting at the one shared discovery point keeps a single source of truth. Every consumer, current and future, sees the same label whatever its source. The alternative is a second detection path threaded through each consumer.
- Matching GFD's format makes the fallback invisible to exact-match consumers. A mixed fleet behaves like a fully GFD-labeled one.
- Degrading preserves existing behavior on clusters the fallback cannot help. A missing grant costs the controller one fast failed List and a log line per discovery, not a reconcile failure.
- One `--gpu-arch` rule for both commands, shaped like `--platform`, gives operators a single mental model. Validation turns a typo into an error instead of a plausible render with fallback defaults and no architecture overrides.

## Consequences

1. Discovery costs one extra List call on clusters where some target node lacks the label. Clusters where every node is GFD-labeled pay nothing.
2. The fallback depends on the driver's attribute name and driver name. A rename in the driver silently returns affected clusters to `unknown` architecture. The comment in `gpu_resourceslice_detect.go` names the contract so that breakage is diagnosable.
3. The label NVCRE sees can differ from the label on the Node object. `kubectl get node` shows no `gpu.product` on a DRA-only node that NVCRE treats as labeled.
4. `UniformGPUProduct` sees the fallback product, so `certification run` on a DRA-only or mixed GFD and DRA fleet passes the homogeneity check when every node reports the same product, and still rejects a mixed-architecture fleet before the Certification is created.
5. Offline `--gpu-arch` accepts only architectures `gpu-defaults.yaml` lists. Supporting new hardware offline means adding its defaults there, which new hardware needs anyway.

## Alternatives Considered

1. **Write the label to the Node.** This would make the label visible to everything, but NVCRE does not patch nodes (ADR-061), and it would need `patch` on nodes cluster-wide.
2. **Teach each consumer to read ResourceSlices.** Every current and future consumer would need the second source, and they could drift. Augmentation keeps them unchanged.
3. **Compare `gpu.ParseProduct` values in `UniformGPUProduct` instead of normalizing.** This fixes that one consumer and leaves every other exact-match comparison exposed. Normalizing at the source fixes all of them.
4. **Fail the reconcile when the ResourceSlice List fails.** This turns a missing optional grant into an outage on clusters that never needed the fallback.
5. **`--gpu-arch` as a fallback only when the `nodeSelector` has no label.** Two commands would then have opposite precedence for one flag, and the flag would differ from `--platform`.

## Notes

- Driver attribute source: `kubernetes-sigs/dra-driver-nvidia-gpu`, `GpuInfo.Attributes()` in `cmd/gpu-kubelet-plugin/deviceinfo.go`, value from NVML `GetName()`.
- GFD label format: `NVIDIA/k8s-device-plugin`, `sanitise` in `internal/lm/resource.go`, which drops characters outside `[A-Za-z0-9-_. ]` and joins the whitespace-separated fields with hyphens.

## References

- [ADR-012: Feature — Platform and GPU Architecture Overrides](012-platform-gpu-overrides.md)
- [ADR-061: Remove Remediation Controller — Failed Node Attribution via Certification CR](061-nvcre-nvsentinel-remediation-decoupling.md)
- [ADR-075: On-Prem GB200/GB300 Override](075-onprem-gb200-gb300-override.md)
- [Kubernetes: Dynamic Resource Allocation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
- `pkg/controller/workflow_controller.go` — `discoverTargetNodes`
- `pkg/controller/cache.go` — Node cache scoped to `nvidia.com/gpu.present`
- `pkg/cluster/discover.go` — `DiscoverGPUNodes`, `UniformGPUProduct`
- `docs/concepts/platform-detection.md` — ResourceSlice fallback section
