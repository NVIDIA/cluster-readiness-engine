---
title: Run DeepSeek-V3 Training with WorkloadRun
description: End-to-end guide to running DeepSeek-V3 BF16 training with Megatron-Bridge and WorkloadRun, including a developer-overridable branch checkout.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This guide walks through running DeepSeek-V3 BF16 training on a GPU cluster with NVIDIA Cluster Readiness Engine (NVCRE), using Megatron-Bridge and WorkloadRun. Unlike the [Nemotron 5 guide](./workloadrun-nemotron5.md), which uses raw Megatron-LM, this example uses Megatron-Bridge's recipe system *and* demonstrates the developer-friendly pattern of overriding the container's `scripts/performance/` with a cloned branch checkout — so developers can iterate on their own `run_script.py` / `utils/overrides.py` without rebuilding the image.

For an introduction to WorkloadRun and when to use it instead of a full Certification, see the [WorkloadRun Quick Start](../getting-started/workloadrun-quick-start.md). For generic WorkloadRun options (targeting nodes, measurements, framework types), see [Run a WorkloadRun](./run-workloadrun.md).

## Prerequisites

- A Kubernetes cluster with GPU nodes (`nvidia.com/gpu.present=true`) and `kubectl` access
- `nvcrectl` installed and the NVCRE controller running on the cluster — see [Installation](../getting-started/install.md)
- An NGC API key for pulling the NeMo container image from `nvcr.io`
- The DeepSeek-V3 recipe fetches the model configuration from Hugging Face Hub at startup, so worker pods need outbound access to `huggingface.co` (or a pre-populated Hugging Face cache mounted into the pods — see the env var comments below)

No dataset or checkpoint download is required: the recipe trains on synthetic data, and this guide runs a proxy-size model.

## Megatron-LM vs Megatron-Bridge

| | Megatron-LM ([Nemotron 5 guide](./workloadrun-nemotron5.md)) | Megatron-Bridge (this guide) |
|---|---|---|
| **Config style** | All args spelled out in `train.sh` | Recipe name + Hydra overrides |
| **Container** | `pytorch:25.08-py3` + clone Megatron-LM | `nemo:26.02.00` (Megatron-Bridge pre-installed at `/opt/Megatron-Bridge`) |
| **Entry point** | `torchrun pretrain_gpt.py --num-layers 79 ...` | `torchrun run_script.py -m deepseek -s v3 ...` |
| **Init container** | Clone Megatron-LM from GitHub | Clone Megatron-Bridge from GitHub (`llmb-r0.2.0` branch) |
| **What gets overridden** | The whole `megatron-lm` package | Only `scripts/performance/` — bridge package + megatron-core come from the container |
| **Log profile** | `megatron-training` | `megatron-bridge` |

## Create the WorkloadRun manifest

Save the following as `deepseek-v3-bf16.yaml`:

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: deepseek-v3-bf16
  namespace: default
spec:
  # The NeMo 26.02.00 image has Megatron-Bridge + Megatron-Core +
  # DeepEP + matching transformers/flashinfer/etc. all pre-installed at
  # /opt/Megatron-Bridge. Nothing needs pip install at runtime.
  image: nvcr.io/nvidia/nemo:26.02.00
  framework:
    exec:
      command: ["/bin/bash", "/config/train.sh"]
  numNodes: 16
  config:
    inline:
      train.sh: |
        #!/bin/bash
        set -e

        BRIDGE_PATH=/mnt/workspace/megatron-bridge
        # Prepend the cloned scripts/performance/ to PYTHONPATH so a
        # developer's branch overrides win for runner code, while the
        # bridge package + megatron-core continue to come from the
        # container's /opt/Megatron-Bridge.
        export PYTHONPATH=${BRIDGE_PATH}/scripts/performance:$PYTHONPATH

        exec torchrun \
          --nnodes $PET_NNODES \
          --nproc-per-node $PET_NPROC_PER_NODE \
          ${BRIDGE_PATH}/scripts/performance/run_script.py \
          -a dummy -p dummy \
          -m deepseek -s v3 \
          -c bf16 \
          -g gb300 \
          -ng $((PET_NNODES * PET_NPROC_PER_NODE)) \
          -gn $PET_NPROC_PER_NODE \
          -ms 50 \
          -mb 1 -pp 1 -ep 64 \
          --no-detach \
          model.hidden_size=1024 \
          model.kv_channels=64 \
          model.pipeline_model_parallel_layout=null \
          model.virtual_pipeline_model_parallel_size=null \
          logger.log_throughput=true \
          train.manual_gc=true \
          train.manual_gc_interval=100
  initContainers:
    - name: megatron-bridge-clone
      image: nvcr.io/nvidia/nemo:26.02.00
      command: ["/bin/bash", "-c"]
      args:
        - |
          set -ex
          git clone --depth 1 -b llmb-r0.2.0 \
            https://github.com/NVIDIA-NeMo/Megatron-Bridge.git \
            /mnt/workspace/megatron-bridge
      volumeMounts:
        - name: workspace
          mountPath: /mnt/workspace
  volumes:
    - name: workspace
      emptyDir:
        medium: Memory
  volumeMounts:
    - name: workspace
      mountPath: /mnt/workspace
  env:
    # NCCL / torch.distributed tuning recommended for this model.
    - name: TORCH_NCCL_AVOID_RECORD_STREAMS
      value: "1"
    - name: TORCH_NCCL_HIGH_PRIORITY
      value: "1"
    - name: NCCL_NVLS_ENABLE
      value: "0"
    - name: NCCL_NET_GDR_LEVEL
      value: PHB
    - name: NCCL_NET_GDR_C2C
      value: "1"
    # MNNVL / NVLink topology hints
    - name: NVLINK_DOMAIN_SIZE
      value: "72"
    - name: USE_MNNVL
      value: "1"
    - name: NUM_OF_HYBRID_EP_RANKS_PER_NVLINK_DOMAIN
      value: "64"
    # CUDA + transformer-engine tuning
    - name: CUDA_DEVICE_MAX_CONNECTIONS
      value: "32"
    - name: NVTE_FWD_LAYERNORM_SM_MARGIN
      value: "20"
    - name: NVTE_BWD_LAYERNORM_SM_MARGIN
      value: "20"
    # HuggingFace: the DeepSeek-V3 recipe needs to fetch the model
    # config to introspect architecture. Leave these at "0" so HF Hub
    # downloads succeed (mount an HF cache instead if your cluster is
    # air-gapped).
    - name: TRANSFORMERS_OFFLINE
      value: "0"
    - name: HF_HUB_OFFLINE
      value: "0"
    - name: TOKENIZERS_PARALLELISM
      value: "False"
    # UCX env vars to avoid memory hook conflicts in the container
    - name: UCX_MEM_MMAP_HOOK_MODE
      value: none
    - name: UCX_MEM_CUDA_HOOK_MODE
      value: none
    - name: UCX_MEM_MALLOC_HOOKS
      value: "n"
    - name: UCX_ERROR_SIGNALS
      value: ""
  resources:
    limits:
      nvidia.com/gpu: "4"
      memory: 800Gi
      cpu: "128"
    requests:
      nvidia.com/gpu: "4"
      memory: 500Gi
      cpu: "64"
  goodputMeasurement:
    logProfileRef: megatron-bridge
    sampleInterval: 10s
```

### How the branch override works

The pattern in this YAML is the developer-iteration pattern for Megatron-Bridge:

1. **Container ships everything heavy.** `nvcr.io/nvidia/nemo:26.02.00` already has `megatron-bridge`, `megatron-core` (with FSDP), `transformers`, `flashinfer`, `DeepEP`, and matching CUDA/NCCL — all version-pinned to work together. No `pip install` at runtime.
2. **Init container clones a branch** of `https://github.com/NVIDIA-NeMo/Megatron-Bridge` into a shared `emptyDir` volume. Only `scripts/performance/` matters here — it's where `run_script.py`, `utils/overrides.py`, and the recipe configs live.
3. **`train.sh` prepends the cloned `scripts/performance/` to `PYTHONPATH`** and invokes the cloned `run_script.py` by absolute path. Result:
   - `from utils.overrides import …` resolves to the developer's branch (override wins).
   - `from megatron.bridge import …` and `from megatron.core import …` resolve to the container's pre-installed packages (stable, version-matched).
4. **No image rebuild required.** A developer commits to their branch on GitHub, points the init container's `git clone -b ...` at it, and re-submits the WorkloadRun. Iteration takes seconds (small clone) instead of minutes (image build/push).

Branch choice matters: pick a branch whose script + bridge API expectations match what the container ships. `llmb-r0.2.0` works against `nemo:26.02.00`. A branch that depends on a newer bridge API will fail at import time — pick a different branch or use a newer NeMo image.

### Key configuration

- **`numNodes: 16`** — the number of nodes **per job group**, not a total. The orchestrator partitions **all** eligible GPU nodes into groups of this size and creates one job per group: `numNodes: 16` on a 16-node cluster creates a single 16-node job, while `numNodes: 4` on the same cluster would create four 4-node jobs that run in parallel. Set it to your full cluster size for a single full-scale run.
- **`-m deepseek -s v3`** — selects the DeepSeek-V3 architecture.
- **`-c bf16`** — BF16 precision (FP8 paths in this branch require a newer bridge than `nemo:26.02.00` ships).
- **`-g gb300`** — GPU-specific tuning hints. Also accepts `gb200`, `b200`, or `h100` — match your cluster.
- **`-a dummy -p dummy --no-detach`** — `run_script.py` in this branch was written as a SLURM job submitter; `--no-detach` makes it run in-process under `torchrun`. The `-a`/`-p` flags are required by argparse but unused when not actually submitting via SLURM.
- **`-ng / -gn`** — `-ng` is `numNodes × gpusPerNode`; `-gn` matches `gpusPerNode`. The `PET_NNODES` and `PET_NPROC_PER_NODE` environment variables are injected into worker pods automatically.
- **`-pp 1 -ep 64`** — pipeline parallelism off, 64-way expert parallelism. Works for the proxy-size model below; for the full model use `-pp 4 -ep 64 -vp 4` at 256+ GPUs. The product `ep × tp × pp` must divide the world size; with `-pp 1` and no tensor parallelism, `-ep` can equal the world size.
- **`model.hidden_size=1024 model.kv_channels=64`** — proxy model dimensions so the workload fits on a 64-GPU cluster. The full DeepSeek-V3 (671B) needs 256+ GPUs; drop these overrides when you have enough GPUs.
- **`model.pipeline_model_parallel_layout=null model.virtual_pipeline_model_parallel_size=null`** — clears the recipe's layout/VPP defaults so they don't conflict with the `-pp 1` we're forcing.
- **`logger.log_throughput=true`** — emits TFLOPS per iteration into the training log; the built-in `megatron-bridge` LogProfile parses these for goodput.
- **`train.manual_gc=true train.manual_gc_interval=100`** — explicit garbage-collection pacing recommended for this recipe.
- **`resources`** — optional. When omitted, NVCRE auto-sets only `nvidia.com/gpu: <gpusPerNode>` (both limits and requests); it does not guess memory or CPU. Set the block explicitly, as here, if you need CPU pinning or memory sizing.

NVCRE auto-handles NCCL environment variables, ComputeDomain and DRA setup, EFA/RoCE networking, and topology-aware orchestration based on the detected platform and GPU architecture.

## Submit the WorkloadRun

```bash
export NGC_API_KEY=<your-ngc-api-key>

nvcrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  deepseek-v3-bf16.yaml
```

The `--workload-registry*` flags create an `nvcr.io` image pull secret in the target namespace and inject it into the WorkloadRun automatically.

## Monitor progress

Watch pods come up:

```bash
kubectl get pods -w
```

Lightweight status check:

```bash
nvcrectl workloadrun status deepseek-v3-bf16
```

Check goodput measurement in real time:

```bash
kubectl get goodputmeasurement -w
```

The training-loop log lines (in `kubectl logs <pod> -c node -f`) will look like:

```
[2026-06-11 15:19:14] iteration   2/  50 | consumed samples: 1024 |
  elapsed time per iteration (ms): 11184.3 |
  learning rate: 6.000000E-05 | global batch size: 512 |
  lm loss: 1.178900E+01 | ... | grad norm: 1.837 | ...
```

…with a `throughput per GPU (TFLOP/s/GPU): ...` field appended on each iteration thanks to `logger.log_throughput=true`.

## Generate a report

```bash
nvcrectl workloadrun report deepseek-v3-bf16
```

Example output (16 nodes × 4 GPUs, BF16 proxy model):

```
╔════════════════════════════════════════════════════════════════╗
║                       WorkloadRun Report                       ║
╚════════════════════════════════════════════════════════════════╝

  Name:      deepseek-v3-bf16
  Platform:  aws
  GPU:       gb300
  Nodes:     16

┌────────────────────────────────────────────────────────────────┐
│  workloadrun/deepseek-v3-bf16                                   │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Runtime:   7m 52s                                             │
│  Scale:     full-scale                                         │
│  Nodes/Job: 16                                                 │
│  Jobs:      1                                                  │
│                                                                │
│  ┌ clique-0 (16 nodes) ──────────────────────────────────────┐ │
│  │  Avg Runtime Goodput:  1.00 (100%)                        │ │
│  │  Avg TFLOPs/GPU:  556.5                                   │ │
│  │  Avg Step Time:  4.78s                                    │ │
│  └───────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   1/1 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

If a node fails, NVCRE records it in the WorkloadRun status with a reason (`HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed`); it never taints, cordons, or otherwise modifies the node.

To save as JSON:

```bash
nvcrectl workloadrun report deepseek-v3-bf16 --results-file report.json
```

## Gang scheduling

If the cluster runs a gang-aware scheduler such as KAI Scheduler, add `spec.gangScheduler` so all pods in a job group are held until the entire gang can be placed:

```yaml
spec:
  gangScheduler:
    schedulerName: kai-scheduler   # required; injected as schedulerName into every workload pod
    queue: high-priority           # optional; defaults to "default-queue"
```

The `queue` value is applied as the `kai.scheduler/queue` label on the pod template metadata. It must be a valid Kubernetes label value (at most 63 characters). See [Run a WorkloadRun](./run-workloadrun.md) for details.

## Clean up

Cancel the WorkloadRun (cascades to its Workflow, Jobs, and pods):

```bash
nvcrectl workloadrun cancel deepseek-v3-bf16 -n default
```

## Changing precision

The `-c` flag in `train.sh` controls numerical precision. This example uses `-c bf16`. The `llmb-r0.2.0` `run_script.py` also accepts `bf16`, `fp8_cs`, `fp8_mx`, `fp8_sc`, and `nvfp4`, but some FP8 variants require recipe code that's only on newer (incompatible) branches against `nemo:26.02.00` — stick with `bf16` unless you've verified the branch supports your chosen FP8 mode. If you switch, also update `metadata.name` to keep the resource name aligned with what it's actually doing.

## Available models

The `llmb-r0.2.0` `run_script.py` accepts model family + size via `-m / -s`:

```bash
# List available recipes from inside any running pod
kubectl exec -it <pod> -c node -- bash -c '
  python /opt/Megatron-Bridge/scripts/performance/run_script.py --help
' 2>&1 | grep -A 1 -E "(-m|-s|--task)" | head -20
```

Compatible models include DeepSeek-V3, GPT-OSS, LLaMA 3.1, and Qwen3. All use the same WorkloadRun pattern — swap `-m deepseek -s v3` for the desired family/size, adjust parallelism (`-pp`, `-ep`, `-vp`) for your GPU count, and (optionally) drop the `model.hidden_size=... model.kv_channels=...` proxy overrides if you have enough GPUs for the full model.

## Next steps

- [How-to: Run a WorkloadRun](./run-workloadrun.md) — targeting nodes, measurements, framework options
- [How-to: Run Nemotron 5 Training](./workloadrun-nemotron5.md) — the raw Megatron-LM equivalent of this guide
- [CLI Reference: workloadrun](../cli-reference/workloadrun.md)
- [API Reference: WorkloadRun](../api-reference/workloadrun.md)
