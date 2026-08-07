# ADR-031: Platform-Aware NCCL Communication Benchmark Configuration

## Context

NCCL communication benchmarks (all-reduce, all-gather, alltoall) require platform-specific transport configuration to achieve optimal inter-node bandwidth. Each cloud provider uses a different interconnect fabric — AWS uses EFA, GCP uses FastRak/TCPXO, Azure uses InfiniBand, and OCI uses RoCE — each requiring distinct NCCL environment variables, MPI binary paths, library paths, and in some cases PCIe topology descriptor files.

The current NCCL catalog entries (`communication/nccl-all-reduce`, `communication/nccl-all-gather`, `communication/nccl-alltoall`) hard-code on-premises NVLink-local defaults (`NCCL_IB_DISABLE=1`, `NCCL_NET_PLUGIN=none`), which disable all fabric communication. These entries work for single-node NVLink validation but cannot exercise the inter-node network fabric that distributed training depends on.

The override mechanism (ADR-012, ADR-027) already supports conditional patching via `when.platform` with auto-detection from node `providerID`, but no NCCL catalog entry uses it. The training catalog entries demonstrate the override pattern with `when.gpuArchitecture`.

## Decision

Extend the three NCCL communication catalog entries with platform-conditional overrides that inject the correct NCCL transport configuration per cloud provider. The base template retains on-premises defaults as the safe fallback. Each platform override replaces the MPI launcher arguments with platform-specific NCCL environment variables, and where required, adds topology descriptor files via ConfigMap dependencies.

## Implementation

### Environment variable propagation via MPI `-x` flags

NCCL entries use MPI with a launcher/worker architecture: the launcher pod runs `mpirun` which SSH's to worker node pods. NCCL environment variables must reach the remote worker processes, not just the launcher container. OpenMPI's `-x VAR=VALUE` flag explicitly forwards environment variables to all MPI ranks across nodes. Container `env` on the launcher pod does NOT propagate to remote workers.

Therefore, platform-specific NCCL variables go into `trainer.args` as `-x` flags, while launcher-only variables (library paths, MPI prefix) go into `trainer.env`.

### Atomic list replacement for args and command

The `Trainer.Args` and `Trainer.Command` fields have `+listType=atomic` in the TrainJob API. Strategic merge patch replaces atomic lists entirely rather than appending. Each platform override provides the complete `trainer.args` array including both the stable MPI flags and platform-specific `-x` environment variables. This eliminates positional fragility — each override is a self-contained, readable argument list.

### Topology descriptor files via ConfigMap dependencies

Some platforms require `NCCL_TOPO_FILE` pointing to an XML file that describes the PCIe/NVLink/NIC hierarchy of the instance type. Since the workload container (e.g., `nvcr.io/nvidia/pytorch:26.01-py3`) does not include these files, each platform override that requires a topology file adds:

1. A ConfigMap dependency containing the XML content inline, with `when` matching the platform
2. A `podTemplateOverrides` entry adding a volume mount from the ConfigMap to `/etc/nccl/topo.xml` on node containers
3. The `-x NCCL_TOPO_FILE=/etc/nccl/topo.xml` flag in the MPI args

ConfigMaps follow the existing dependency lifecycle (`cleanup: auto`) and are auto-cleaned up when the Workflow is deleted.

### Platform override summary

| Platform | Interconnect | MPI binary | Key NCCL vars | Topo file |
|----------|-------------|------------|---------------|-----------|
| **on-prem** (base) | NVLink-local | `/usr/local/mpi/bin/mpirun` | `NCCL_IB_DISABLE=1`, `NCCL_NET_PLUGIN=none` | No |
| **aws** | EFA | `/opt/amazon-efa-ofi/openmpi/bin/mpirun` | `FI_EFA_USE_DEVICE_RDMA=1`, `NCCL_MNNVL_ENABLE=0` | p5.48xlarge topo XML |
| **gcp** | FastRak/TCPXO | (unchanged) | `NCCL_ALGO`, `NCCL_FASTRAK_*` (20+ vars), `NCCL_TUNER_PLUGIN` | No (uses tuner plugin) |
| **azure** | InfiniBand | (unchanged) | `NCCL_IB_PCI_RELAXED_ORDERING=1`, `NCCL_TOPO_FILE`, `NCCL_SOCKET_IFNAME` | NDv5 topo XML |
| **oci** | RoCE | (unchanged) | `NCCL_IB_TC=41`, `NCCL_IB_SL=0`, `NCCL_IB_QPS_PER_CONNECTION=4`, `NCCL_IB_GID_INDEX=3` | No |

### Azure A100

Azure + NVIDIA A100 SXM4 is supported for the nccl-all-reduce, nccl-all-gather, and nccl-alltoall catalog entries via the existing override mechanism.

1. **Override selection:** The generic Azure override is narrowed with `gpuArchitecture: notIn: [a100]`, so for Azure clusters with A100 GPUs only the Azure+A100 override applies; for other Azure GPUs (e.g. H100) the generic Azure override applies. Exactly one override matches per (platform, GPU) pair.
2. **NCCL_IB_HCA:** The Azure+A100 override sets `NCCL_IB_HCA=mlx5` to direct NCCL to use all Mellanox ConnectX-6 InfiniBand adapters. Without this, NCCL does not optimally select IB devices on A100 nodes.
3. **A100-specific topology:** The override uses `azure-a100-sxm4.xml` (via the `azure-ib-with-topo-a100-comm.yaml` dependency) which describes the correct PCIe Gen4 bus IDs for A100. The generic Azure topology file (`azure-h100.xml`) uses PCIe Gen5/H100 bus IDs and causes NCCL to misroute traffic across NUMA boundaries on A100 hardware.
4. **Additional IB tuning:** The A100 override includes `NCCL_COLLNET_ENABLE=1`, `NCCL_IGNORE_CPU_AFFINITY=1`, `NCCL_IB_SPLIT_DATA_ON_QPS=0`, and `NCCL_IB_AR_THRESHOLD=0` to maximize InfiniBand throughput on A100 fabric.
5. **Bandwidth thresholds:** Expected minimum bus bandwidth for A100 SXM4 on InfiniBand is not set by the catalog. Users add thresholds by setting `spec.categoryOptions[].thresholds` on the Certification when targeting Azure A100. The metric name for NCCL bus bandwidth is `busBandwidthGBps` (unit GB/s; from BandwidthMeasurement). CEL expressions use a single `value` variable (float64). Example: `busBandwidthGBps: "value >= 50"` enforces a minimum 50 GB/s bus bandwidth; operators should tune the value based on their Azure IB topology and expected performance. An example Certification snippet is in `config/samples/` (see Azure A100 NCCL sample).

### Files modified

- `pkg/catalog/entries/communication/nccl-all-reduce.yaml` — add `overrides:` section with 4 platform entries + topo ConfigMap dependencies for aws/azure
- `pkg/catalog/entries/communication/nccl-all-gather.yaml` — same structure, different perf binary name
- `pkg/catalog/entries/communication/nccl-alltoall.yaml` — same structure, different perf binary name
- `pkg/catalog/catalog_test.go` — add `overrideCount` to `lookupOutput()` for golden file validation
- Catalog golden files updated to include override count
- New integration test `workflow-nccl-platform-override` verifying override application with AWS nodes

### Example: AWS override structure

```yaml
overrides:
- when:
    platform:
      equals: aws
  dependencies:
  - apiVersion: v1
    kind: ConfigMap
    lifecycle:
      cleanup: auto
    metadata:
      name: nccl-all-reduce-aws-topo
    data:
      topo.xml: |
        <!-- p5.48xlarge PCIe topology -->
        <system version="1">
          ...
        </system>
  jobTemplate:
    spec:
      workload:
        trainJob:
          podTemplateOverrides:
          - spec:
              containers:
              - name: trainer
                volumeMounts:
                - mountPath: /etc/nccl
                  name: nccl-topo
              volumes:
              - name: nccl-topo
                configMap:
                  name: nccl-all-reduce-aws-topo
            targetJobs:
            - name: trainer
          trainer:
            command:
            - timeout
            - "1800"
            - /opt/amazon-efa-ofi/openmpi/bin/mpirun
            args:
            - -np
            - "32"
            - -N
            - "4"
            - --allow-run-as-root
            - --mca
            - plm_rsh_args
            - -o StrictHostKeyChecking=no
            - -x
            - NCCL_DEBUG=INFO
            - -x
            - FI_EFA_USE_DEVICE_RDMA=1
            - -x
            - NCCL_MNNVL_ENABLE=0
            - -x
            - NCCL_TOPO_FILE=/etc/nccl/topo.xml
            - /usr/local/bin/all_reduce_perf_mpi
            - -b
            - "8"
            - -e
            - 16G
            - -f
            - "2"
            - -N
            - "0"
            env:
            - name: LD_LIBRARY_PATH
              value: "/opt/amazon-efa-ofi/openmpi/lib:/opt/amazon-efa-ofi/efa/lib:/opt/amazon-efa-ofi/ofi/lib"
            - name: OPAL_PREFIX
              value: /opt/amazon-efa-ofi/openmpi
```

## Rationale

- **Base as on-prem default**: On-premises clusters require no override matching. Users deploying on cloud platforms get automatic configuration through platform auto-detection. No explicit configuration is needed.
- **Full args replacement over partial patching**: `trainer.args` is `+listType=atomic`, so strategic merge replaces the entire list. This is intentional — each platform's args list is self-contained and readable. Positional JSON Patch (`/spec/.../args/5`) would be fragile and break if any arg is added or removed from the base.
- **ConfigMap for topology files**: Avoids building custom container images per platform. ConfigMaps are managed as regular Workflow dependencies with auto-cleanup. The topo XML is small (~2KB) and well within ConfigMap limits.
- **Three nearly-identical entries**: The three NCCL entries share the same override structure, differing only in the perf binary name. A shared template mechanism could reduce duplication but would require catalog loader changes beyond the scope of this ADR. The repetition is acceptable for 3 entries and is easy to maintain.

## Consequences

**Positive:**
- NCCL benchmarks work out-of-the-box on AWS, GCP, Azure, OCI, and on-prem
- No user configuration required — platform auto-detected from node providerID
- Correct NCCL transport configuration ensures meaningful fabric bandwidth measurements
- `status.orchestration.appliedOverrides` provides observability into which platform config was applied

**Negative:**
- Each NCCL entry grows by ~200 lines (4 platform overrides × ~50 lines each)
- Topology files are statically embedded — instance types not covered require manual topology file injection via Workflow-level overrides
- GCP FastRak configuration has 20+ variables that may need updates as NCCL versions evolve

## Alternatives Considered

1. **Container `env` for NCCL vars**: Would simplify overrides but NCCL env vars would not reach MPI worker processes on remote nodes. MPI `-x` is the correct propagation mechanism for multi-node workloads.
2. **Single catalog entry with template conditionals**: Use Go template `{{ if eq .Platform "aws" }}` in the YAML. Rejected because the catalog template data (`TemplateData`) does not include platform information — platform is detected at Workflow reconciliation time, after catalog Build.
3. **Custom NCCL test container images per CSP**: Bake topo files and env vars into images. Rejected — increases image maintenance burden and couples the container image to the deployment platform.

## References

- ADR-012: Platform/GPU Architecture Overrides
- ADR-027: Kustomize-like Override UX
- ADR-016: NCCL All-Reduce Catalog Entry
- ADR-018: NCCL Test Suite Catalog
