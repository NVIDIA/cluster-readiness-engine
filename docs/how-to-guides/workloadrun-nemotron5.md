---
title: Run Nemotron 5 Training with WorkloadRun
description: End-to-end guide to running a Nemotron 5 (56B) Megatron-LM training workload with WorkloadRun and generating a report.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This guide walks through running a Nemotron 5 (56B) training workload on a GPU cluster using Cluster Readiness Engine (CRE) and `ncrectl`, from manifest to report. It uses the `exec` framework to launch a Megatron-LM training script with mock data, so no dataset or checkpoint download is required.

For an introduction to WorkloadRun and when to use it instead of a full Certification, see the [WorkloadRun Quick Start](../getting-started/workloadrun-quick-start.md). For generic WorkloadRun options (targeting nodes, measurements, framework types), see [Run a WorkloadRun](./run-workloadrun.md).

## Prerequisites

- A Kubernetes cluster with GPU nodes (`nvidia.com/gpu.present=true`) and `kubectl` access
- `ncrectl` installed and the CRE controller running on the cluster — see [Installation](../getting-started/install.md)
- An NGC API key for pulling the `nvcr.io/nvidia/pytorch` workload image

If a cluster administrator has already installed the controller, you only need the NGC API key for the workload image — skip straight to creating the manifest below.

## Create the WorkloadRun manifest

Save the following as `nemotron5.yaml`. It runs Nemotron 5 (56B) using Megatron-LM with the `exec` framework:

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nemotron5-56b
  namespace: default
spec:
  image: nvcr.io/nvidia/pytorch:25.08-py3
  framework:
    exec:
      command: ["/bin/bash", "/config/train.sh"]
  numNodes: 16
  config:
    inline:
      train.sh: |
        #!/bin/bash
        set -e

        WORKSPACE_DIR=${WORKSPACE_DIR:-/mnt/workspace}
        MEGATRON_PATH=${MEGATRON_PATH:-${WORKSPACE_DIR}/megatron-lm}
        CHECKPOINT_DIR=${CHECKPOINT_DIR:-${WORKSPACE_DIR}/checkpoints}
        TENSORBOARD_DIR=${TENSORBOARD_DIR:-${WORKSPACE_DIR}/tensorboard}
        mkdir -p ${CHECKPOINT_DIR} ${TENSORBOARD_DIR}

        if ! ls ${MEGATRON_PATH}/megatron/core/datasets/helpers_cpp*.so 1>/dev/null 2>&1; then
          echo "Building Megatron helpers_cpp..."
          cd ${MEGATRON_PATH}
          pip install -e . --no-deps --no-build-isolation 2>&1 | tail -5
          cd ${WORKSPACE_DIR}
        fi

        CHECKPOINT_ARGS=""
        DATA_CACHE_ARGS=""
        if [ "$ENABLE_CHECKPOINT" = "true" ]; then
          CHECKPOINT_ARGS="--save ${CHECKPOINT_DIR}"
          if [ -d "${CHECKPOINT_DIR}" ] && [ "$(ls -A ${CHECKPOINT_DIR} 2>/dev/null)" ]; then
            CHECKPOINT_ARGS="${CHECKPOINT_ARGS} --load ${CHECKPOINT_DIR}"
          fi
          DATA_CACHE_ARGS="--data-cache-path ${WORKSPACE_DIR}/data_cache"
          mkdir -p ${WORKSPACE_DIR}/data_cache
        fi

        TOTAL_GPUS=$((PET_NNODES * PET_NPROC_PER_NODE))
        TP=${TENSOR_PARALLELISM:-4}
        PP=${PIPELINE_PARALLELISM:-1}
        MBS=${MICRO_BATCH_SIZE:-1}
        GBS=${GLOBAL_BATCH_SIZE:-$TOTAL_GPUS}

        echo "TOTAL_GPUS=${TOTAL_GPUS} GBS=${GBS} MBS=${MBS} TP=${TP} PP=${PP}"

        exec torchrun \
          --nnodes $PET_NNODES \
          --nproc-per-node $PET_NPROC_PER_NODE \
          ${MEGATRON_PATH}/pretrain_gpt.py \
          --attention-backend flash \
          --distributed-timeout-minutes 230 \
          --use-mcore-models \
          --no-mmap-bin-files \
          --sequence-parallel \
          --untie-embeddings-and-output-weights \
          --disable-bias-linear \
          --init-method-std 0.014 \
          --position-embedding-type rope \
          --rotary-base 1000000 \
          --rotary-percent 1.0 \
          --squared-relu \
          --group-query-attention \
          --kv-channels 128 \
          --normalization RMSNorm \
          --attention-dropout 0.0 \
          --hidden-dropout 0.0 \
          --exit-duration-in-mins 30 \
          --train-iters 50 \
          --lr-decay-iters 1830030 \
          --lr 6e-4 \
          --min-lr 6e-6 \
          --weight-decay 0.1 \
          --clip-grad 1.0 \
          --lr-decay-style cosine \
          --lr-warmup-iters 5 \
          --eval-iters 1 \
          --eval-interval 50 \
          --log-interval 10 \
          --tokenizer-type NullTokenizer \
          --vocab-size 131072 \
          --mock-data \
          --num-workers 1 \
          --no-create-attention-mask-in-dataloader \
          --log-progress \
          --timing-log-option minmax \
          --log-params-norm \
          --log-num-zeros-in-grad \
          --log-throughput \
          --bf16 \
          --adam-beta1 0.9 \
          --adam-beta2 0.95 \
          --use-distributed-optimizer \
          --overlap-grad-reduce \
          --overlap-param-gather \
          --manual-gc \
          --log-straggler \
          --disable-straggler-on-startup \
          --straggler-minmax-count 16 \
          --check-weight-hash-across-dp-replicas-interval 20000 \
          --ckpt-fully-parallel-save \
          --ckpt-fully-parallel-load \
          --async-save \
          --ckpt-assume-constant-structure \
          --ckpt-format torch_dist \
          --num-layers 79 \
          --hidden-size 8192 \
          --ffn-hidden-size 32768 \
          --num-attention-heads 64 \
          --seq-length 8192 \
          --max-position-embeddings 8192 \
          --num-query-groups 8 \
          --tensor-model-parallel-size $TP \
          --pipeline-model-parallel-size $PP \
          --micro-batch-size $MBS \
          --global-batch-size $GBS \
          $CHECKPOINT_ARGS \
          $DATA_CACHE_ARGS \
          --save-interval ${SAVE_INTERVAL:-250} \
          --save-retain-interval ${SAVE_RETAIN_INTERVAL:-1000} \
          --tensorboard-dir ${TENSORBOARD_DIR}
  initContainers:
    - name: megatron-clone
      image: nvcr.io/nvidia/pytorch:25.08-py3
      command: ["/bin/bash", "-c"]
      args:
        - |
          set -ex
          if [ ! -d "/mnt/workspace/megatron-lm/.git" ]; then
            git clone --depth 1 -b core_v0.15.2 \
              https://github.com/NVIDIA/Megatron-LM.git /mnt/workspace/megatron-lm
          fi
          echo "Megatron-LM cloned successfully"
      volumeMounts:
        - mountPath: /mnt/workspace
          name: workspace
  volumes:
    - name: workspace
      emptyDir:
        medium: Memory
  volumeMounts:
    - mountPath: /mnt/workspace
      name: workspace
  env:
    - name: NVTE_FWD_LAYERNORM_SM_MARGIN
      value: "16"
    - name: NVTE_BWD_LAYERNORM_SM_MARGIN
      value: "16"
    - name: NVTE_FUSED_ATTN
      value: "0"
    - name: TORCHINDUCTOR_WORKER_START
      value: fork
    - name: CUDA_DEVICE_MAX_CONNECTIONS
      value: "1"
    - name: TORCH_NCCL_AVOID_RECORD_STREAMS
      value: "1"
    - name: TORCH_NCCL_HIGH_PRIORITY
      value: "1"
    - name: UCX_MEM_MMAP_HOOK_MODE
      value: none
    - name: UCX_MEM_CUDA_HOOK_MODE
      value: none
    - name: UCX_MEM_MALLOC_HOOKS
      value: "n"
    - name: UCX_ERROR_SIGNALS
      value: ""
    - name: EXIT_DURATION_MINS
      value: "30"
    - name: ENABLE_CHECKPOINT
      value: "false"
    - name: TRAIN_ITERS
      value: "50"
    - name: SAVE_INTERVAL
      value: "250"
    - name: SAVE_RETAIN_INTERVAL
      value: "1000"
    - name: PYTHONPATH
      value: /mnt/workspace/megatron-lm
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
    logProfileRef: megatron-training
    sampleInterval: 10s
```

Key fields:

- **`numNodes: 16`** — the number of nodes **per job group**, not a total. The orchestrator partitions **all** eligible GPU nodes into groups of this size and creates one job per group: `numNodes: 16` on a 16-node cluster creates a single 16-node job, while `numNodes: 4` on the same cluster would create four 4-node jobs that certify the nodes in parallel. Set it to your full cluster size for a single full-scale run.
- **`resources`** — optional. When omitted, CRE auto-sets only `nvidia.com/gpu: <gpusPerNode>` (both limits and requests); it does not guess memory or CPU. Set the block explicitly, as here, if you need CPU pinning or memory sizing.
- **`TP` in `train.sh`** — tensor parallelism: 4 for GB200/GB300, 8 for H100.
- **No `target` needed** — CRE auto-discovers all GPU nodes. See [Run a WorkloadRun](./run-workloadrun.md) to target specific nodes.
- **`goodputMeasurement`** — parses training logs with the built-in `megatron-training` LogProfile to compute goodput, TFLOPs/GPU, and step time for the report.

CRE auto-handles NCCL environment variables, ComputeDomain and DRA setup, EFA/RoCE networking, and topology-aware orchestration based on the detected platform and GPU architecture.

## Submit the WorkloadRun

```bash
export NGC_API_KEY=<your-ngc-api-key>

ncrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  nemotron5.yaml
```

The `--workload-registry*` flags create an `nvcr.io` image pull secret in the target namespace and inject it into the WorkloadRun automatically.

Output:

```
Discovered 16 GPU nodes with product: NVIDIA-GB300
WorkloadRun nemotron5-56b created in namespace default.

To check status:
  kubectl get workloadrun nemotron5-56b -n default
```

## Monitor progress

Watch pods come up:

```bash
kubectl get pods -w
```

Check WorkloadRun status (lightweight):

```bash
ncrectl workloadrun status nemotron5-56b
```

For the full resource spec:

```bash
kubectl get workloadrun nemotron5-56b -o yaml
```

## Generate a report

After the workload completes (or fails), generate a report:

```bash
ncrectl workloadrun report nemotron5-56b
```

Output:

```
╔════════════════════════════════════════════════════════════════╗
║                      WorkloadRun Report                        ║
╚════════════════════════════════════════════════════════════════╝

  Name:      nemotron5-56b
  Platform:  aws
  GPU:       gb300
  Nodes:     16

┌────────────────────────────────────────────────────────────────┐
│  workloadrun/nemotron5-56b                                     │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Runtime:   4m 12s                                             │
│  Scale:     full-scale                                         │
│  Nodes/Job: 16                                                 │
│  Jobs:      1                                                  │
│                                                                │
│  ┌ clique-0 (16 nodes) ──────────────────────────────────────┐ │
│  │  Avg Runtime Goodput:  0.50 (50%)                         │ │
│  │  Avg TFLOPs/GPU:  852.7                                   │ │
│  │  Avg Step Time:  1.75s                                    │ │
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

If a node fails, CRE records it in the WorkloadRun status with a reason (`HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed`); it never taints, cordons, or otherwise modifies the node.

To save the report as JSON:

```bash
ncrectl workloadrun report nemotron5-56b --results-file report.json
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
ncrectl workloadrun cancel nemotron5-56b -n default
```

To uninstall CRE entirely:

```bash
ncrectl setup reset
```

## Alternative: one-shot run with wait and report

Combine run, wait, and report in a single command:

```bash
ncrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait --results-file report.json \
  nemotron5.yaml
```

This blocks until the workload completes and prints the report automatically.

## Next steps

- [How-to: Run a WorkloadRun](./run-workloadrun.md) — targeting nodes, measurements, framework options
- [CLI Reference: workloadrun](../cli-reference/workloadrun.md)
- [API Reference: WorkloadRun](../api-reference/workloadrun.md)
- Nemotron 5 is also available as certification catalog entries (`training/nemotron5-8b`, `training/nemotron5-56b`) — see [Certify a Cluster](./certify-a-cluster.md)
