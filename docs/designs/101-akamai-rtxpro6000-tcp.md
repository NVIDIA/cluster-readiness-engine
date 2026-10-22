# ADR-101: Akamai LKE RTX PRO 6000 TCP Overrides

> **Status:** Accepted

## Context

Akamai LKE nodes identify themselves with `linode://` provider IDs. They need a
separate platform identity so they do not inherit unrelated on-premises rules.
The inspected RTX PRO 6000 VMs expose one GPU, standard `nvidia.com/gpu`
resources, an `eth0` workload interface, and no steady-state GPU taint. Larger
VM shapes have eight GPUs; their PCIe peer-to-peer paths have not been qualified.

A two-node all-reduce run with one GPU per node completed over sockets with no
correctness errors, testing 8 bytes through 1 GiB with 20 iterations and one
cycle. At 1 GiB it measured 0.91 GB/s out-of-place bus bandwidth. This does not
qualify eight-GPU VMs, training, other collectives, or a performance threshold.
At that bandwidth, the generic 16 GiB / 100 iteration / 10 cycle workload cannot
reasonably finish within the one-hour launcher timeout.

## Decision

1. Identify `linode://` nodes as `akamai` and support that platform in offline
   Certification and WorkloadRun rendering.
2. Reuse a shared RTX PRO 6000 TCP environment for the three NCCL collectives,
   Nemotron 5 8B, and MPI/Torch WorkloadRuns. Select `eth0`, disable InfiniBand
   and external network plugins, and request only standard GPU resources.
3. For multi-GPU Akamai renders, set `NCCL_P2P_DISABLE=1` until the VM's intra-node
   paths are qualified. Do not set `NCCL_P2P_LEVEL=PHB`: PHB permits access
   through the host bridge and does not disable an unqualified PCIe path.
   Leave both variables unset for one-GPU renders, preserving the tested profile.
4. Keep generic catalog benchmark defaults unchanged. Require explicit
   `maxBytes: 1G`, `numIterations: 20`, and `numCycles: 1` in documented Akamai
   NCCL examples, including all-reduce, all-gather and alltoall. These are the
   tested all-reduce bounds, not measured baselines for every collective.
5. Keep the architecture default at eight GPUs. Smaller VMs must explicitly set
   `gpusPerNode` to their allocatable GPU count; do not infer VM shape from the
   platform name or silently lower the request.

## Implementation

Add platform registration, provider-ID detection, and a synthetic offline node
identity. Compose the Akamai NCCL fragment into catalog and WorkloadRun
overrides. Cover one- and eight-GPU renders, MPI exports and user overrides,
and preserve GCP's independent P2P settings. Document the explicit benchmark
bounds beside the platform/architecture table and the GCP RTX PRO 6000 notes.
Use public constraints and documentation in code comments rather than internal
tracker IDs. Add `akamai` to the CLI reference lists.

## Rationale

TCP is the verified transport on the available LKE GPU VMs. No RDMA resource or
GPU taint should be invented for an environment that exposes neither.
Explicitly disabling P2P is conservative for unqualified multi-GPU VMs; it can
be revisited after measured qualification. A P2P distance setting is not a P2P
disable switch. Explicit bounded benchmark options address the TCP runtime
limit without adding platform detection to catalog option defaulting in this
onboarding change.

## Consequences

The default catalog benchmark remains unsuitable for this TCP deployment
unless users set the documented options. A future platform-aware defaulting
change must preserve explicit user values across live and offline paths.
Multi-GPU performance may be lower while P2P is disabled. The two-node,
one-GPU-per-node result remains the hardware validation scope. RoCE, additional
collectives, training, and multi-GPU VMs require separate hardware validation.

## Alternatives Considered

- **PHB for every multi-GPU render:** rejected because it permits an unqualified
  PCIe P2P path rather than avoiding it.
- **Leave multi-GPU P2P entirely to NCCL:** less conservative than disabling it
  while the VM paths remain unqualified.
- **Reduce catalog defaults globally:** rejected because other platforms have
  different bandwidth and established stress workloads.
- **Add Akamai-only benchmark defaults now:** deferred to avoid introducing
  defaulting behavior across live controllers and offline render paths in this
  change. The documentation must explicitly state all three required bounds.
- **Treat LKE as on-premises:** loses platform identity and invites accidental
  matches against unrelated hardware profiles.

## Notes

The completed all-reduce run used two single-GPU nodes. Disabling P2P only in
multi-GPU renders changes an unqualified profile, not that measured result.
No performance thresholds will be inferred from one bounded functional run.
The separate Workflow-render CLI platform gap is tracked independently.

## References

- [Akamai platform onboarding PR](https://github.com/NVIDIA/cluster-readiness-engine/pull/537)
- [NCCL P2P disable](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html#nccl-p2p-disable)
- [NCCL P2P distance control](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html#nccl-p2p-level)
- [Platform and GPU overrides](012-platform-gpu-overrides.md)
- [Platform-aware NCCL configuration](031-platform-aware-nccl-config.md)
