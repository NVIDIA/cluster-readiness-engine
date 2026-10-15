# ADR-096: Mokka GPU Simulation for Local Development and a UAT Runtime Lane

> **Status:** Accepted

## Context

NVCRE certifies GPU clusters, and until now nobody could run a Certification end to end without one. The two test layers that run without hardware stop short of execution, by design.

**envtest (ADR-014)** runs the controllers against a real API server with no scheduler, no kubelet and no garbage collector. Pod, TrainJob and JobSet status comes from fixtures and status patches between steps, and pod logs come from `input_logs_<pod>.txt` through a fake `PodLogFetcher` (`cmd/integration/integration_test.go`). It is the right place for reconcile logic, and it is blind to anything a running pod would do.

**The Kind + KWOK UAT lane (ADR-049)** adds a real scheduler, Kubeflow Trainer and JobSet, and compares rendered pods with hand-written goldens. The GPU nodes are KWOK nodes, so nothing ever runs on them. A custom Stage flips every Job-owned pod from `Running` to `Succeeded` after 10–15 s (`test/uat/tilt/kwok-stage-pod-complete.yaml:34`), and no pod produces a log. DRA is a stub: `test/uat/tilt/dra-stub/main.go` creates a ComputeDomain's channel ResourceClaimTemplate within about two seconds and `reconcileResourceClaims` (line 131) marks every claim allocated with an empty result list. There are no IMEX daemons, no ComputeDomain status, no finalizers, and no termination grace period. Every node label, allocatable count and ResourceSlice is YAML someone on this team wrote. Every UAT test waits for `Succeeded`; none exercises a failure path.

Three consequences follow, and recent history shows each one.

**Fixtures encode our assumptions, so they cannot catch our assumptions being wrong.** The Nscale UAT fixture (`test/uat/testdata/nscale/b200/nodes.yaml`, ResourceSlices from line 83) publishes one device per node with no `type` attribute, on nodes with no allocatable `nvidia.com/gpu`. That routes `nvcrectl cluster info` to `CountDRAGPUs`, which counts only devices whose `type` is `gpu` (`pkg/controller/gpu_resourceslice_detect.go:156-159`), as the real NVIDIA DRA driver writes them. The architecture-label path still works against the fixture, because it skips only `vfio` devices (`:96-101`). Against that fixture `cluster info` counts zero GPUs, which is the bug #419 fixed after it was found on a real Nscale cluster. The UAT lane cannot catch it coming back. Several recent PRs say the same thing in other words: #487 records that the reconcile goldens had captured the bug as expected output, and #454 found that hostname pinning in the goldens had masked a missing node selector.

**The riskiest external interaction NVCRE has, the ComputeDomain lifecycle on GB200 and GB300, has never been exercised against the real controller off hardware.** Two defects in that area were found on real machines. #235 (`18cf77a4`) deleted the ComputeDomain under live worker pods and every rank died with CUDA error 719; the fix is the drain barrier in `pkg/controller/pod_drain.go`. The requeue at `pkg/controller/workflow_deps.go:689` exists because pods that reference a channel template the ComputeDomain controller has not created yet stay stuck permanently. The stub reproduces neither the timing nor the teardown.

**A contributor cannot run NVCRE at all without a GPU cluster.** Reproducing a report, trying a catalog change, or demonstrating the product means borrowing hardware. That cost falls hardest on the people NVCRE most wants as contributors now that it is open source.

[Mokka](https://github.com/NVIDIA/k8s-test-infra) (NVIDIA/k8s-test-infra, chart `nvml-mock`) simulates the software contracts around NVIDIA GPUs on ordinary CPU nodes: a mock NVML library, the real `nvidia-smi`, PCI and NUMA trees, NVLink and fabric identity, mock IMEX channels, and a Fabric Manager stand-in. The NVIDIA components that read those contracts then run unmodified: the device plugin, GPU Feature Discovery, the DRA driver and its ComputeDomain daemons, DCGM. Mokka's own rule of thumb is to use it when software reads hardware state, and real hardware when software executes work. It has no CUDA: its `libcuda` shim exports about fifteen stub functions and no driver API that `libcudart` can load, so NCCL, PyTorch and Megatron cannot run on it.

## Decision

1. **Adopt Mokka as the GPU simulation layer for one Kind-based environment with two consumers.** The same cluster profiles, scripts and stub image serve local development and CI. The `nvml-mock` chart is pinned to `0.5.0`.

2. **Local development is a goal of equal weight with CI.** `make mokka-up PROFILE=<name>` brings up a Kind cluster with simulated GPUs and NVCRE installed from the working tree, and `make mokka-down` removes it. A Tilt mode rebuilds and reloads the controller into that cluster. A user-facing page, "Run NVCRE locally without GPUs", documents it and states plainly what passes locally and what still needs hardware.

3. **UAT gains a second lane.** ADR-049's lane becomes the *UAT render lane*. The new *UAT runtime lane* runs on Mokka. Neither replaces the other, and neither replaces envtest:

   | Lane | Owns |
   |---|---|
   | envtest (ADR-014) | Reconcile logic, status transitions, log parsing against recorded logs |
   | UAT render (KWOK, ADR-049) | Pod-spec goldens, CSP-specific rendering (EFA, AWS RoCE, GKE TCPXO, Nscale RDMA), scale |
   | UAT runtime (Mokka, this ADR) | Real allocation, real GFD and DRA producer output, probes, MPI bootstrap, the ComputeDomain lifecycle, the kubelet log path, drain and cleanup |

4. **Cluster profiles are chosen per test case from Mokka's shipped GPU profiles.** Kind sets every node's `providerID` to `kind://`, which `nodePlatform` maps to `onprem` (`pkg/controller/workflow_detect.go`), so each profile exercises NVCRE's on-prem path for that architecture:

   | Profile | Allocation | NVCRE path exercised |
   |---|---|---|
   | `h100` | Device plugin | On-prem HGX MPI |
   | `gb200`, `gb300` | Device plugin for GPUs, DRA driver for ComputeDomains only | On-prem NVL72: ComputeDomain, `NCCL_MNNVL_ENABLE`, clique topology |
   | `b200` | Device plugin | On-prem HGX B200 override (ADR-094) |
   | Two profiles on different nodes | Device plugin | Mixed fleets, `MajorityArchitecture` |

   B300 and RTX PRO 6000 have no shipped Mokka profile and need a custom one. Cloud-specific overrides never trigger on Kind; they stay on the render lane.

5. **Workloads run through a stub image that replaces only the measuring binary.** It is selected with the existing `spec.image` field (ADR-077), so no API changes. OpenMPI, `sshd`, `orted` and the kubelet log path are real. Only the NCCL test binary is replaced, by a script that replays a recorded nccl-tests log on rank 0. Variants that exit non-zero, print no results, or print results below a threshold give the lane the failure paths the render lane does not have.

6. **The runtime lane asserts behaviour, not pod specs.** Terminal status and reason, detected platform and architecture, BandwidthMeasurement results and transport, lifecycle ordering (ComputeDomain `Ready` before workers run; ComputeDomain outlives every non-terminal worker pod on failure and deletion paths), and that no ResourceClaims, ResourceClaimTemplates or ComputeDomains remain afterwards. Pod-spec goldens stay with the render lane.

7. **CI runs the lane nightly and on demand, then per PR once it is stable.** On demand means `workflow_dispatch` by a maintainer, against `main` or a `pull-request/N` branch, which exists only after a maintainer has vetted the PR. The lane adds no `pull_request` trigger and no label gate: it runs on self-hosted `linux-amd64-cpu32` runners, and a label would not keep unvetted fork code off them. Promotion to per-PR means adding the `pull-request/N` push trigger the render lane already uses, decided on measured wall-clock time and flake rate.

8. **No CRD, controller or catalog change.** This ADR adds test infrastructure, make targets, a stub image and a documentation page.

## Implementation

### Layout

```text
test/uat/mokka/
  kind/               Kind configs per profile (workers labelled mokka.nvidia.com/type=sgpu)
  values/             nvml-mock, DRA driver and NVCRE values per profile
  manifests/          device plugin and GFD DaemonSets
  stub/               Dockerfile, all_reduce_perf_mpi, recorded logs, mpirun wrapper
  setup.sh            brings up one profile; shared by make mokka-up and the CI lane
  *_test.go           runtime lane tests, build tag uat_mokka
```

### Make targets

| Target | Purpose |
|---|---|
| `mokka-up PROFILE=<name>` / `mokka-down` | Local environment |
| `tilt-mokka PROFILE=<name>` | Local environment with controller hot reload |
| `setup-test-uat-mokka`, `test-uat-mokka-run`, `cleanup-test-uat-mokka` | CI lane, mirroring the ADR-049 targets |

### Profile recipes

These are the versions and flags proven locally (see Notes).

**Every profile:** `nvml-mock` 0.5.0 with `nodeSelector: {mokka.nvidia.com/type: sgpu}`; NFD 0.19.0; Kubeflow Trainer 2.2.1; the Prometheus Operator stripped-down CRDs; NVCRE from the local chart with the ADR-049 values.

- **Device plugin:** `nvcr.io/nvidia/k8s-device-plugin:v0.18.2` with `--nvidia-driver-root=/var/lib/nvml-mock/driver`, `--driver-root-ctr-path=/var/lib/nvml-mock/driver` and `--device-discovery-strategy=nvml`.
- **GFD:** the `gpu-feature-discovery` binary from the same image, with `--driver-root-ctr-path=/var/lib/nvml-mock/driver`, `--device-discovery-strategy=nvml` and `--machine-type-file=/var/lib/nvml-mock/driver/config/machine-type`, writing to the NFD `features.d` hostPath. Mokka's own `tests/e2e/go/assets/gfd-mock.yaml` pins GFD v0.8.2, which rejects `--nvidia-driver-root` and crash-loops.
- **`nvidia.com/gpu.present`:** GFD does not write it and NVCRE requires it, so `setup.sh` labels the GPU nodes, standing in for the GPU Operator.

**`gb200` and `gb300` add:**

- `imex.mockChannels.enabled=true` and a `topology` overlay naming the worker nodes in one clique.
- The DRA driver chart `dra-driver-nvidia-gpu` 0.5.0 from `helm.ngc.nvidia.com/nvidia`, with `nvidiaDriverRoot=/var/lib/nvml-mock/driver`, `altProcDevices=/var/lib/nvml-mock/imex/proc-devices`, `resources.gpus.enabled=false` and `resources.computeDomains.enabled=true`. This is the GPU Operator's own split on GB200: the device plugin allocates GPUs and the DRA driver handles ComputeDomains.
- The driver's image overridden with Mokka's IMEX-shim build: target `daemon` of Mokka's `deployments/nvml-mock/Dockerfile.compute-domain-daemon`, built `FROM nvcr.io/nvidia/dra-driver-nvidia-gpu:v0.5.0` with `nvidia-imex-595` from Ubuntu's public multiverse repository and `nvidia-imex-shim`, which runs the real IMEX daemon in no-GPU mode. Because `nvidia-imex` is proprietary, this image is built in the job or on the developer's machine and never published.
- NVCRE's vendored `hack/crds/resource.nvidia.com_computedomains.yaml` is not applied; the real driver installs its CRD.

### Stub image

`test/uat/mokka/stub/` builds an Ubuntu 24.04 image with `openmpi-bin` (4.1.6), `openssh-server` and `openssh-client`, and:

- `/usr/local/bin/all_reduce_perf_mpi`: rank 0 (`OMPI_COMM_WORLD_RANK`) prints a recorded nccl-tests log with Kubernetes timestamps stripped; other ranks exit 0. The first log is `pkg/nccl/testdata/real_a100_allreduce_2node_pdt.log`.
- `/usr/local/mpi/bin/mpirun`: a wrapper that runs `ulimit -n 1048576` and execs `/usr/bin/mpirun`. Kind's containerd gives containers `RLIMIT_NOFILE` of 1073741816, and OpenMPI 4.1 closes every descriptor up to that limit when it forks, so an unwrapped `mpirun` spins at full CPU and never launches.
- Variants selected by image tag or environment: non-zero exit, no result rows, a long run for deletion tests.

### Initial test cases

| Case | Profile | Asserts |
|---|---|---|
| `cluster-info` | `h100`, `gb200` | `nvcrectl cluster info` resolves `onprem` and the architecture from GFD-written labels |
| `nccl-all-reduce-pass` | `h100` | Certification `Succeeded`; BandwidthMeasurement `Complete/JobSucceeded` with the replayed rows and transport |
| `nccl-all-reduce-workload-failed` | `h100` | Stub exits non-zero, Job `Failed/WorkloadFailed` |
| `nccl-all-reduce-threshold` | `h100` | `options.thresholds.busBandwidthGBps` set to a minimum above the replayed value (for example `value >= 1` against the replayed 0.03 GB/s); `ValidationFailed` and `ThresholdViolation` |
| `nccl-all-reduce-no-data` | `h100` | Stub prints no rows; `NoDataCollected` and no verdict |
| `gb200-computedomain-pass` | `gb200` | ComputeDomain `Ready` before workers `Running`; Certification `Succeeded`; no claims, templates or ComputeDomains left |
| `gb200-delete-mid-run` | `gb200` | ComputeDomain outlives every non-terminal worker pod; NVCRE logs the drain deferral; nothing left afterwards |
| `dra-only-detection` | DRA driver with `resources.gpus.enabled=true`, no device plugin or GFD | Architecture resolved from real ResourceSlices (ADR-082); `cluster info` counts DRA GPUs |

### Documentation

A `docs/how-to-guides/` page, "Run NVCRE locally without GPUs", registered in `docs/index.yml`. It covers `make mokka-up`, the profiles, running a Certification with the stub image, and a short "what this does not prove" section: CUDA, NCCL, bandwidth, goodput and DCGM levels 2–4.

### CI

A `uat-mokka` job in `.github/workflows/e2e-uat.yml` running one profile per matrix entry on the existing self-hosted `linux-amd64-cpu32` runners, triggered by the nightly `schedule` and by `workflow_dispatch`. It never runs on `pull_request` events, for the same reason `codeql.yml` routes those to `ubuntu-latest`: fork PRs build unvetted code. Later, per-PR runs come from the `pull-request/N` push trigger, which copy-pr-bot creates only for vetted PRs. The Mokka image is published for `linux/amd64` and `linux/arm64`; the stub and IMEX-shim images are built in the job.

### Testing plan

The lane is its own test. Before it lands, each case runs green locally on one profile at a time. The failure-path cases must fail when the stub variant is swapped for the passing one, so they are proven to detect what they claim.

## Rationale

**Two full Certifications ran end to end on a laptop with no GPUs.** On 2026-10-14, on Docker Desktop (arm64, 7.7 GiB), with the recipes above:

- **H100 MPI, on the device plugin:** `communication/nccl-all-reduce` reported PASSED in 41 s. Each worker was allocated `nvidia.com/gpu: 8` by the real device plugin, detection came from labels the real GFD wrote, the catalog's sshd setup and SSH-session readiness probe (ADR-093) ran for real, `mpirun` started 16 ranks across both workers over ssh and `orted`, and BandwidthMeasurement parsed 24 result rows and the `Socket` transport through the kubelet log API.
- **GB200 with a real ComputeDomain:** `communication/nccl-all-reduce` reported PASSED in 48 s. The ComputeDomain passed server-side dry run against the real driver's CRD. The real controller created the channel and daemon templates, the real IMEX daemons formed the domain, claim preparation was retried for about three seconds ("error getting ComputeDomainClique") until it was `Ready`, and the workers waited in `ContainerCreating` until then.

The same runs surfaced what the other lanes cannot:

- **A detection gap.** On the first H100 attempt, `mpirun` hung before its first line of output. NVCRE reported `WorkflowRunning` throughout and would have done so until `timeoutPerJob`, an hour later. The cause was the Kind descriptor limit, but the undetected silent launcher is NVCRE's.
- **The #235 fix, observed on a real kubelet.** Deleting a GB200 Certification mid-run kept the ComputeDomain until both workers, which took about 30 s to terminate, and their claims were gone. NVCRE logged "Workload pods still terminating; deferring cleanup until they are gone". KWOK deletes pods instantly, and envtest has no kubelet.
- **Real setup contracts.** GFD does not set `nvidia.com/gpu.present`, which every NVCRE target selector depends on. On a fresh cluster without the GPU Operator, NVCRE finds no nodes. That is worth stating in the setup documentation, and only a real GFD shows it.

**Mokka is what makes those runs possible.** NVIDIA's device plugin, GFD, DRA driver, ComputeDomain daemons and DCGM all load NVML. Without Mokka, putting GPU-requesting NVCRE pods on Kind means patching `nvidia.com/gpu` onto node status and hand-writing `gpu.product`, which is the fixture approach again: no CDI, no `nvidia-smi`, no device-plugin path, and no way to run the DRA driver at all.

**Separating the lanes keeps each one honest about what it proves.** The render lane is fast, covers every cloud, and asserts pod specs. The runtime lane is slower, covers only on-prem paths, and asserts behaviour. Putting runtime assertions in the render lane would require the fake executions that make its passes meaningless, and putting every CSP in the runtime lane is impossible because Mokka does not simulate their networks.

**The expected yield is real and bounded.** Of the 33 behaviour-changing PRs merged or open from 2026-09-23 to 2026-10-14:

- **Caught directly:** one bug, #419.
- **Validated before shipping:** four changes: #379, #380 for its GPU half, #451 at DCGM level 1 if Mokka supports it, and #420 for its positive path.
- **Partly caught by any real-kubelet run:** four more:
  - #467: a Job stuck in `ContainerCreating` without a bound.
  - #483: a launcher without its pull secret.
  - #464: an on-prem NVL72 loopback runtime the Trainer CRD rejects.
  - #395: the KAI in-pod wait.
- **Out of reach:** the majority, because they are wrong configuration for the real workload or network, found by running it on hardware: #487, #434, #458, #437, #445, #443, and #466, which only fails on CRI-O.

So this lane is not a substitute for hardware. It catches the GPU-stack contract and lifecycle bugs, which are the costliest to find on hardware.

## Consequences

### Positive

- Contributors can run Certifications, WorkloadRuns and the GB200 ComputeDomain flow on a laptop in about ten minutes of setup, mostly image pulls.
- Detection, allocation and DRA behaviour are tested against what NVIDIA's components write, and the lane follows them as they change.
- The UAT suite gets failure-path coverage: workload failure, threshold violation, missing data, deletion mid-run.
- The ComputeDomain lifecycle, including the #235 barrier and the template-creation race, gets regression tests off hardware.

### Negative

- A second lane to maintain. Its setup has more moving parts than KWOK: a DaemonSet that stages simulated surfaces, the device plugin, GFD, NFD, and on GB200 the DRA driver and a locally built IMEX image.
- Mokka is at 0.x and moving quickly; the pin must be bumped deliberately. Two defects in its examples were hit during the local proof: the stale GFD manifest, and a ComputeDomain prerequisite (`altProcDevices`) that its chart documents as unreleased although DRA driver 0.5.0 ships it.
- One profile cluster at a time fits in an 8 GB Docker Desktop. The two profile clusters used about 3 GiB each.
- Every runtime-lane platform is `onprem`. AWS, GCP, Azure, OCI and Nscale paths remain render-lane only.
- Bandwidth and goodput numbers in this lane are replayed, so they test parsing and verdict logic, never performance.
- Kind-specific behaviour can masquerade as NVCRE behaviour, as the descriptor limit did. Findings from this lane need a second look before they become issues.

## Alternatives Considered

### Patch GPU capacity onto plain Kind nodes

Pods would schedule and run, and the stub image would work. Detection would read labels we wrote, allocation would bypass the device plugin, `nvidia-smi` would be absent, and the DRA driver could not run, so the ComputeDomain lifecycle, the main reason for the lane, would be out of reach. **Rejected.**

### Run:ai fake-gpu-operator

It replaces NVIDIA's components with its own stand-ins rather than running them, so the inputs NVCRE reads would still not come from the producers NVCRE meets in production. It does not run NVIDIA's DRA driver or ComputeDomain daemons. **Rejected.**

### Serve logs from KWOK

KWOK's `Logs` and `ClusterLogs` resources can serve a file as a fake pod's log. That would exercise log parsing against the kubelet API, but the pod still never runs, so probes, MPI bootstrap, allocation and drain stay untested. envtest already covers parsing. **Rejected.**

### Run the real NGC images on Mokka

The real `nvcr.io/nvidia/pytorch` image schedules, pulls and starts on Mokka, and its sshd and MPI bootstrap are exercised. The first CUDA or NCCL call fails, so every Certification ends `Failed` after pulling a multi-gigabyte image per node. The stub keeps the real bootstrap and makes the result deterministic. **Rejected as the default**; a real-image smoke case may be added later.

### GPU runners in CI

They measure what Mokka cannot. They are scarce and expensive, and they cannot reproduce faults or lifecycle races on demand. **Complementary, not an alternative.** `test/cluster/tests.sh` remains the hardware path.

### Extend the KWOK lane instead of adding one

KWOK nodes run no kubelet, so Mokka's DaemonSet, the device plugin and the DRA driver cannot run on them; Mokka's own scale harness says as much. **Rejected.**

## Notes

**Local proof environment:** macOS on arm64; Docker Desktop with 14 CPUs and 7.7 GiB; Kind 0.33 with Kubernetes 1.37 and containerd 2.3.4; Helm 4.3. Both profiles used one control-plane node and two workers.

**The success path does not run the drain barrier, and that is intended.** On success, the `default:` arm of `completeTerminalGroup` in `pkg/controller/workflow_controller.go` calls `cleanupScopedDependencies` without the barrier. The barrier guards only the failure, timeout, retry and deletion paths: compare the `if jobFailed` branch of the same function, which calls `shouldWaitForPodDrain` before anything is cleaned up. These are cited by symbol, not line, because the file moves with most merges. In the GB200 run the ComputeDomain was deleted in the same second JobSet killed the MPI workers. That is safe for the MPI entries because the ranks have exited and the workers run only `sshd`. The argument does not carry over to the torch entries, which already use a ComputeDomain: both training entries include `_lib/deps/gb200-compute-domain-and-dra-torch.yaml` for `gpuArchitecture: in: [gb200, gb300]` (`nemotron5-8b.yaml:298`, `nemotron5-56b.yaml:270`) and set `mlPolicy.torch` (`:86`). There the node pods are the ranks and run no `sshd`, so the success path has to be checked separately; it is in the findings below.

**GFD's clique label reflects the profile, not the topology overlay.** GFD is not injected by Mokka's NRI plugin, so it reads the profile default and labelled both GB200 workers `nvidia.com/gpu.clique=00000000-0000-0000-0000-000000000001.0` rather than the overlay's UUID. That is consistent across components and harmless for one-clique tests. Multi-clique topology tests need GFD to see the overlay, or the clique labels set from the overlay.

**Findings to file separately:**

- A launcher that hangs before producing output is not detected until `timeoutPerJob`.
- The success-path ComputeDomain cleanup for the torch entries (`nemotron5-8b`, `nemotron5-56b` on GB200/GB300). If the group can reach `Succeeded` while any node pod is still running a rank, the ComputeDomain is deleted under it. The runtime lane's `gb200` profile can test this once a training stub exists.
- Mokka profiles give every node the same GPU UUIDs. NVCRE does not key on UUIDs today; anything that does would be misled. Mokka's control-plane mode may give per-node identity (unverified).
- Mokka's `h100` profile carries `nvidia.com/gpu.clique`. Whether real HGX H100 nodes do should be checked before H100 topology tests rely on it.

**Mokka's networking roadmap** covers node-level InfiniBand and RoCE NIC surfaces (k8s-test-infra #478), per-NIC state and fault injection (#923), and Network Operator support with multi-rack topology (#595). With #478, the `rdmaDeviceAccess: hostPath` mode from ADR-088 becomes testable here. Nothing on the roadmap covers EFA or GKE networking.

**Open questions:** whether Mokka supports device-plugin time-slicing (MIG layouts are documented); whether DCGM level 1 diagnostics run against Mokka's DCGM; and the CI wall-clock time per profile, which has only been measured locally.

## References

- [Mokka (NVIDIA/k8s-test-infra)](https://github.com/NVIDIA/k8s-test-infra) and its [simulation depth by area](https://nvidia.github.io/k8s-test-infra/#simulation-depth-by-area)
- ADR-014: envtest integration tests with golden files
- ADR-049: Kind + KWOK end-to-end UAT tests
- ADR-077: certification workload image override
- ADR-082: GPU architecture fallback from DRA ResourceSlices
- ADR-085: Nscale NKS support on the DRA stack
- ADR-088: OCI GB200 RoCE overrides and `rdmaDeviceAccess`
- ADR-093: MPI worker `SYS_CHROOT` and session readiness probe
- ADR-094: HGX B300 architecture defaults and on-prem NIC override
- Fixes cited: #235 (drain barrier), #419 (DRA GPU count), #487, #454
- `pkg/controller/pod_drain.go`, `pkg/controller/workflow_deps.go`, `pkg/controller/gpu_resourceslice_detect.go`, `test/uat/tilt/dra-stub/main.go`, `test/uat/tilt/kwok-stage-pod-complete.yaml`
