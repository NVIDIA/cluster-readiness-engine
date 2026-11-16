# ADR-110: Select the Network Fabric with `spec.fabric`, Detection, and Today's Mapping as the Fallback

> **Status:** Proposed
> Hardware verification of the AWS GB300 EFA render on stock EKS is pending (issue #576).

## Context

AWS GB300 (`p6e-gb300r.36xlarge`) can be built two ways, and NVCRE can render only one of them.

- **RoCE rack.** The NICs come only through DRA. `aws-net-k8s-dra-plugin` publishes one ResourceSlice per GPU node with `spec.driver: dra.networking.k8s.aws`, and each RoCE device carries `dra.vpc.amazonaws.com/deviceType: roce`. The DeviceClass `roce.networking.k8s.aws` selects on that driver and attribute. Node allocatable has no `vpc.amazonaws.com/efa` key at all, not even `"0"`.
- **Stock EKS.** A standard AMI and launch template, plus the EFA device plugin. On this instance cards 1-8 accept no ENIs and EFA is capped at 2, so allocatable shows `vpc.amazonaws.com/efa: "1"` (single-EFA layout) or `"2"`, and there are no `dra.networking.k8s.aws` slices. The only ResourceSlices on the measured cluster came from `compute-domain.nvidia.com`.

Both are the same instance type, so neither the instance type nor the platform and GPU architecture labels can tell them apart.

Every AWS GB300 override in the catalog and in `pkg/platform/overrides/workloadrun.yaml` matches on `platform: aws` and `gpuArchitecture: gb300` alone. Each one strips the EFA/OFI NCCL plugins and requests 8 devices from `roce.networking.k8s.aws`. On a stock EKS cluster those pods never schedule. AICR's GB300 lane is built exactly that way, measures about 45 GB/s bus bandwidth with a plain TrainJob, and cannot move its NCCL validation onto NVCRE.

The fabric is not a new concept in NVCRE. The catalog already encodes it, but implicitly and in file names: `azure-*-ib`, `forge-ib`, `togetherai-ib`, `mistral-gb300-ib`, `nscale-*-ib`, `aws-*-efa`, `aws-gb300-roce`, `oci-*-roce`, `gcp-gb200-roce`, `gcp-h100-tcpxo`. GB300 alone already spans three of them (AWS RoCE, GCP and OCI RoCE, Mistral IB). An AWS-only switch would have been wrong within one release.

AICR will pass the fabric explicitly from its recipe. Its value set, defined in AICR ADR-028 ([NVIDIA/aicr#3506](https://github.com/NVIDIA/aicr/pull/3506)), is `efa | ib | roce | tcpxo`. When an explicit value disagrees with the cluster, AICR requires the run to fail, not to switch fabrics.

## Decision

1. **One field, `spec.fabric`, on Certification and WorkloadRun.** It is an enum, `efa | ib | roce | tcpxo`, and it sits at spec level beside `gangScheduler`, not in `CategoryOptions`. The fabric is a property of the cluster, not of a test, and one Certification rendering two fabrics against the same nodes is not something NVCRE should be able to express. The Certification spec is already immutable.

2. **Resolution order.** Every consumer resolves the fabric the same way: both controllers and `nvcrectl ... render --dry-run`.

   1. **`spec.fabric` set:** use it, after validating it against the cluster (Decision 4). A failed validation fails the run (`FabricValidationFailed`). NVCRE never substitutes the detected fabric for an explicit one.
   2. **Unset, on a detection target:** detect from the nodes (Decision 3).
   3. **Unset and inconclusive, or not a detection target:** use `defaultFabric(platform, arch)`, which is today's mapping written down:

      | Platform | GPU architecture | Default |
      |---|---|---|
      | aws | gb300 | `roce` |
      | aws | any other | `efa` |
      | azure, forge, togetherai, mistral, nscale | any | `ib` |
      | oci | any | `roce` |
      | gcp | h100 | `tcpxo` |
      | gcp | gb200, gb300 | `roce` |
      | gcp (other), akamai, onprem, unknown | any | empty |

      An empty default means NVCRE renders no RDMA fabric of its own there: TCP (Akamai, GCP RTX PRO 6000), or a site-specific fabric that `nicResourceName` already handles (on-prem). The enum has no `tcp` value, so those targets resolve to empty.

3. **Detection runs only on AWS GB300** (`isFabricDetectionTarget`), the one target where the catalog renders more than one fabric. No other target lists ResourceSlices or gets a detection event. The **target nodes** here, and in Decision 4, are the nodes the job can run on (`fabricNodes`): the nodes of the primary GPU architecture that have the GPUs the run requests, the set the GCP H100 detections use, so a node that never runs the job cannot fail an explicit value or make detection inconclusive. A Certification applies the capacity filter per category; a WorkloadRun uses its resolved `gpusPerNode`. Two signals are read, each on every target node:

   - **EFA:** `vpc.amazonaws.com/efa` in node allocatable at a quantity of at least 1.
   - **RoCE:** at least one device with `dra.vpc.amazonaws.com/deviceType == roce` in a ResourceSlice whose `spec.driver` is `dra.networking.k8s.aws` and whose `spec.nodeName` is the node. Filtering on `roce.networking.k8s.aws` would never match, because that is the DeviceClass name. Matching any ResourceSlice would be a false positive on stock EKS, where `compute-domain.nvidia.com` publishes slices.

   | EFA on every node | RoCE on every node | Result |
   |---|---|---|
   | yes | no | `efa`, detected |
   | no | yes | `roce`, detected |
   | no signal on any node | | the default, silently |
   | both, or a signal on only some nodes | | the default, and a Normal `FabricDetection` event saying what was found and to set `spec.fabric` |
   | the ResourceSlice list is refused | | the default, and a `FabricDetection` event: without the RoCE signal, EFA alone cannot be told apart from both |

   Detection only ever overrides the default on a positive, unambiguous signal, so no signal means no change.

4. **Validation of an explicit value.**
   - The value must be one the catalog renders on that target. AWS GB300 renders `roce` and `efa`. A target with a non-empty default renders only that default. A target with an empty default (on-prem, Akamai, GCP RTX PRO 6000) passes this check with any value, since nothing in the catalog keys on it there; the node checks below still apply. Without this check, `fabric: ib` on AWS GB300 would match no fabric block and render a GB300 workload with no NIC at all.
   - `efa` requires `vpc.amazonaws.com/efa` of at least 1 on every target node, on every platform.
   - `roce` on AWS requires the RoCE signal above on every target node. A refused ResourceSlice list fails the check with a message that says so, rather than claiming the nodes lack RoCE. `nvcrectl ... --dry-run` lists with the user's kubeconfig, so there it warns and keeps the value, as it does for a refused OCI network list (ADR-103).
   - `ib` and `tcpxo` have no cluster signal NVCRE can read, so they are accepted as given.

   A validation failure names the nodes that lack the fabric. It runs only when the field is set, so it cannot change any existing run. A WorkloadRun whose target matches no node yet has nothing to validate against; its Workflow then fails on its own node discovery.

5. **The EFA device count comes from allocatable, not from a field.** The same read that says EFA also says how many: the minimum quantity across the target nodes, so every node can satisfy the request. The templates read it as `{{ .EFAPerNode }}`. Offline render reads no cluster, so it uses 1, the AWS single-EFA layout. `--dry-run` reads the real count.

6. **`when.fabric` is a new override matcher**, a `StringMatchSpec` like `platform`, evaluated against `OverrideContext.Fabric`. It is nil on every catalog override except the AWS GB300 ones. A nil matcher matches anything, so every other platform is untouched by construction. The existing AWS GB300 RoCE blocks gain `fabric: {equals: roce}`, and each catalog entry gets a sibling `fabric: {equals: efa}` block modelled on `aws-gb200-efa-*`. The sibling goes **last** in the entry's override list, not next to the RoCE block. Overrides are recorded by index in `applied-overrides`, so a block inserted mid-list would shift the recorded index of every later override on every other platform. The RoCE block is already the last override that applies on AWS GB300 in every entry, so a final EFA block keeps the same relative order.

   The WorkloadRun platform overrides (`pkg/platform/overrides/workloadrun.yaml`) do **not** use `when.fabric`. The WorkloadRun stores them on its Workflow verbatim, and the Workflow re-evaluates them with no `spec.fabric`, so a matcher there would be re-decided without the user's value. A matcher on those blocks would also change the stored override list of every WorkloadRun on every platform. The WorkloadRun therefore resolves the fabric itself and the template renders either the RoCE or the EFA blocks, which is how it already renders `NicResourceName`. `OverrideConfig.Fabric` is set only where the fabric chose the render, so every other WorkloadRun renders exactly as before.

7. **The resolved fabric is shown only where it chose the render:** when `spec.fabric` is set, or when the target is AWS GB300. In those cases the render carries the `nvcrectl.nvidia.com/detected-fabric` annotation, and `status.categoryStatuses[].resolvedFabric` (Certification) and `status.resolvedFabric` (WorkloadRun) record the value. Everywhere else the fabric was never in question, and showing it would add a line to every existing golden without telling anyone anything.

8. **`--fabric` on `certification render` and `workloadrun render`**, for parity with `--platform` and `--gpu-arch`. It takes precedence over `spec.fabric`.

## Implementation

- **`api/v1alpha1`:**
  - `type Fabric string` with a kubebuilder enum.
  - `Fabric` on `CertificationSpec` and `WorkloadRunSpec`.
  - `WhenSpec.Fabric`.
  - `ResolvedFabric` on `CertificationCategoryStatus` and `WorkloadRunStatus`.

  The enum carries no CEL rule, so ADR-105's cost budget is unaffected.
- **`pkg/controller/fabric_detect.go`:**
  - `defaultFabric`, `isFabricDetectionTarget`, `renderableFabrics` and `resolveFabric`.
  - The ResourceSlice list follows `gpu_resourceslice_detect.go`: the same 5-second timeout and the same current-pool-generation rule (`currentPoolSlices`, shared with `CountDRAGPUs`). A NotFound or unserved `resource.k8s.io/v1` counts as no RoCE devices, since such a cluster cannot allocate the RoCE claim either. A 403 says nothing about the devices, so it is reported as refused (Decisions 3 and 4). Any other list error (a timeout, a 5xx) is retried, never terminal.
  - No new RBAC: `resourceslices` list is already granted cluster-wide in `manager-role.yaml`.
  - `FabricResolution` is the one result type the controllers and the CLI share. The CLI wrappers are in `workflow_detect_export.go`: `ResolveFabric`, `OfflineFabric`, `FabricNodes` (the node set the Certification resolves on, for the dry-run) and `IsFabricListRefused`.
- **`buildOverrideContext` takes the resolved fabric as a parameter.** It does not read it from somewhere the caller may have forgotten to fill. This is deliberate. `when.config` is a shipped matcher dimension that can never fire, because `OverrideContext.Config` is assigned nowhere, and nothing fails. A new parameter makes the compiler find every caller, and a test pins that a `when.fabric` override actually matches, not just that it parses.
- **Certification:**
  - `prepareCategoryBuild` resolves the fabric on the category's capable node set, after the capacity filter. The up-front OCI network check builds categories too, with `preflight` set; it resolves without nodes, so it reads nothing from the cluster for the fabric.
  - The result feeds `BuildConfig` (`Fabric`, `EFAPerNode`) and the override context.
  - `pruneUnmatchableOverrides` then drops the fabric blocks that do not match, so a created Workflow never carries `when.fabric`.
- **WorkloadRun:**
  - The controller resolves the fabric the same way, on the job's nodes (`fabricNodes`), and the platform override template selects the AWS GB300 RoCE or EFA blocks from it (Decision 6). A failed node discovery is retried rather than read as "no nodes", so the template never commits to a fabric decided without them; each retry emits a `WorkflowBuildRetrying` Warning, so the run never waits in silence.
  - The user's own `spec.overrides` may carry `when.fabric`. The Workflow controller re-applies them with no `spec.fabric`, so before the Workflow is created they pass through `resolveFabricOverrides`. That step drops the non-matching ones and clears the matcher on the ones that match. It runs once the fabric is known, from `spec.fabric` or from discovered nodes; with neither, the Workflow controller resolves it from the nodes it discovers.
- **Workflow.** A hand-written Workflow can still use `when.fabric`. The controller then resolves the fabric with no explicit value (detect, else default), and only when some remaining override sets the matcher. It records the result in `status.orchestration.resolvedFabric` and reads it back on every later reconcile, the way it reads the detected platform. Discovery runs once, but the overrides are applied again on each reconcile and the Jobs are created from that later pass, so without the recorded value the Jobs would lose the `when.fabric` changes the dependencies were created with. `nvcrectl workflow render --dry-run` resolves it the same way. All three dry-run paths go through `render.ResolveDryRunFabric`, so they report a resolution, and a refused list, in the same words.

## Rationale

- **The order puts the caller's knowledge first** and the inferred knowledge second, and the historical mapping is the floor. AICR always takes path 1. Standalone users on stock EKS get EFA without writing anything. Existing users get exactly what they got before.
- **Detection is gated to the one target that needs it.** That keeps the new I/O, and the new event, off every other platform. It also means the fallback table is never consulted on those targets for anything but `when.fabric`, which no override outside AWS GB300 uses.
- **Validating only explicit values keeps the failure mode honest.** A wrong explicit value is the user's mistake, and fails loudly. An inconclusive detection is the cluster's ambiguity, and degrades to today's behaviour with an event.

## Consequences

- **Every existing workload render is unchanged, and this is enforced.** The GB300 fixtures in `cmd/integration/testdata/reconcile/certification-aws-gb300-nccl`, `workloadrun-mpi-aws-gb300` and `test/uat/testdata/aws/gb300/nodes.yaml` carry neither signal, so they take the fallback path. The only line each integration golden gains is the `resolvedFabric: roce` status line from Decision 7. AWS GB300 CLI renders gain the `detected-fabric: roce` annotation and a `fabric=roce` term in the `applied-overrides` summary. Every other render is byte-identical, which was checked by diffing `nvcrectl certification render` output from before and after the change for every NCCL and training entry on AWS (H100, GB200, B300, GB300) and on GCP, OCI, Azure, Mistral and on-prem GB300.
- **Two GB300 MPI launcher fragments are renamed** so their names stop claiming a fabric: `aws-gb300-roce-command.yaml` becomes `aws-gb300-mpirun-command.yaml`, and `aws-gb300-roce-mpirun-args.yaml` becomes `aws-gb300-mpirun-args.yaml`. Both carry only OpenMPI's TCP bootstrap, which the EFA arm reuses, and their rendered output is unchanged.
- **A stock-EKS AWS GB300 cluster renders EFA without configuration.** An AWS GB300 RoCE cluster renders RoCE, now on a positive signal rather than by default.
- **The AWS GB300 default stays `roce`.** Flipping it to `efa` would move every existing render. Stock EKS detects EFA anyway, so flipping the default buys nothing.
- **The two enums are maintained separately.** The `efa | ib | roce | tcpxo` value set matches AICR ADR-028 by agreement, not by construction. If either side adds a value, the other repository gets an issue before the change ships.
- **Open item: hugepages.** The GB300 EFA fragments request no hugepages. The GB200 EFA overlay requests `hugepages-2Mi: 10256Mi`, but the RoCE rack advertises `hugepages-2Mi: "0"`, and whether stock-EKS GB300 nodes advertise any is unconfirmed. A request a node cannot satisfy leaves its pods Pending. libfabric's EFA provider falls back to regular pages when none are available. The hardware run on issue #576 decides whether to add them.

## Alternatives Considered

- **An AWS-only `interconnect: efa | roce` field, defaulting to `roce`.** This was option 1 as first proposed. It is AWS-shaped, and `mistral-gb300-ib` already puts GB300 on a third fabric. A general fabric selector costs nothing more.
- **Reusing `nicResourceName` (ADR-075), so that `vpc.amazonaws.com/efa` selects EFA.** That field means "which RDMA NIC resource to request", and its detection is gated by `isOnPremNICOverrideTarget`, which is false for every cloud platform. Reusing it would mean widening that gate and giving one field two unrelated jobs.
- **Detection only, with no field.** AICR needs a value that wins over detection and fails when the cluster disagrees. Mixed or partial signals also need a way for the user to decide.
- **Flipping the AWS GB300 default to EFA**, on the grounds that the RoCE render reflects one internal rack. Detection already gives stock EKS the EFA render, and the flip would move every existing render instead.
- **Keying detection on EFA alone.** Two signals give three outcomes: EFA, RoCE, or neither. With one signal, a RoCE cluster and an unlabelled fixture would be indistinguishable, and the RoCE render would rest on the default rather than on evidence.
- **Showing the resolved fabric on every platform.** It would add one line to almost every golden, while the fabric was never in question on those targets.

## Notes

- `when.config` is dead today (`OverrideContext.Config` is never assigned). This ADR does not fix it; a separate issue tracks it.
- `fabric` is not exposed as a CEL variable in `when.expression`. A CEL-expressed fabric condition could not be pruned by `resolveFabricOverrides`, so it would reintroduce the problem that function solves.

## References

- Issue #576: feat: add an EFA variant for AWS GB300 and a way to select it
- AICR ADR-028 (draft): [NVIDIA/aicr#3506](https://github.com/NVIDIA/aicr/pull/3506)
- ADR-075: NIC resource auto-detection for on-prem GB200/GB300
- ADR-082: GPU architecture from ResourceSlices
- ADR-103: OCI RDMA network detection
- ADR-105: Kubernetes floor and CRD CEL cost budget
