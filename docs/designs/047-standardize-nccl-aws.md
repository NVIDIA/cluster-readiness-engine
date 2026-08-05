# ADR-047: Standardize NCCL Communication Entries on AWS EFA Configuration

## Context

The three NCCL communication catalog entries (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`) had divergent AWS EFA override configurations. The `nccl-all-reduce` entry was the tested/working version, while `nccl-all-gather` and `nccl-alltoall` used an older configuration with:

- The base `nvcr.io/nvidia/pytorch` image instead of the purpose-built `public.ecr.aws/hpc-cloud/nccl-tests` image (which bundles precompiled NCCL test binaries and EFA libraries)
- The `amazon-efa-ofi` OpenMPI path (`/opt/amazon-efa-ofi/openmpi/bin/mpirun`) instead of Amazon's standard path (`/opt/amazon/openmpi/bin/mpirun`)
- An unnecessary AWS topology ConfigMap with PCIe bus ID mappings
- MPI-variant binary paths (`/usr/local/bin/*_perf_mpi`) instead of the standalone binaries in the nccl-tests image (`/opt/nccl-tests/build/*_perf`)
- Different NCCL environment variables (missing `FI_PROVIDER=efa`, `NCCL_SOCKET_IFNAME`, and the LD_LIBRARY_PATH passthrough)

This inconsistency meant that only `nccl-all-reduce` worked correctly on AWS EFA clusters.

## Decision

Standardize `nccl-all-gather` and `nccl-alltoall` AWS overrides to match the tested `nccl-all-reduce` configuration. Extract the shared AWS EFA runtime dependency (image + EFA resource requests) into a new `_lib` template.

## Implementation

### Shared template

New file `entries/_lib/deps/aws-efa-runtime-patch-comm.yaml`: TrainingRuntime strategic merge patch that sets the `public.ecr.aws/hpc-cloud/nccl-tests` image and requests 32 EFA devices. Parameterized by `{{ .EntryName }}` for the runtime name.

### Entry changes

For `nccl-all-gather` and `nccl-alltoall`:
1. Replace inline AWS runtime dependency with `{{ lib "deps/aws-efa-runtime-patch-comm.yaml" . | indent 2 }}`
2. Remove the AWS topology ConfigMap dependency
3. Change `command` to `/opt/amazon/openmpi/bin/mpirun`
4. Update `args` to match all-reduce: add `FI_PROVIDER=efa`, `NCCL_SOCKET_IFNAME=eth0`, `LD_LIBRARY_PATH` passthrough; remove `NCCL_TOPO_FILE`
5. Change binary path from `/usr/local/bin/*_perf_mpi` to `/opt/nccl-tests/build/*_perf`
6. Replace env vars with `PATH: "$PATH:/opt/amazon/efa/bin:/usr/bin"`
7. Add `image` override to trainer spec

For `nccl-all-reduce`: replace inline runtime dependency with shared `_lib` template (no behavioral change).

## Rationale

- **Single tested configuration**: the `nccl-tests` image is purpose-built for NCCL benchmarking on EFA and has been validated in production. Using the base pytorch image required separate EFA library installation and different binary paths.
- **Topology ConfigMap unnecessary**: the `nccl-tests` image includes topology auto-detection, making the static PCIe topology XML redundant.
- **Shared template**: the runtime dependency (image + EFA resources) is now in `_lib`, consistent with ADR-046's approach to eliminating duplication.

## Consequences

### Positive
- All three collective NCCL tests (`all_reduce`, `all_gather`, `alltoall`) use an identical, tested AWS configuration
- Updating the NCCL tests image version requires changing one `_lib` file

### Negative
- `nccl-all-gather` and `nccl-alltoall` now produce different WorkflowSpecs on AWS than before (intentional behavioral change)

## References

- ADR-046: Shared Template Library for Catalog Entries
- ADR-016: NCCL All-Reduce Certification Catalog Entry
- ADR-018: NCCL Test Suite Catalog
