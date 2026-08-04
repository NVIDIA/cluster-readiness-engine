#!/bin/bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -e

# All paths via env vars
WORKSPACE_DIR=${WORKSPACE_DIR:-/mnt/workspace}
MEGATRON_PATH=${MEGATRON_PATH:-${WORKSPACE_DIR}/megatron-lm}
CHECKPOINT_DIR=${CHECKPOINT_DIR:-${WORKSPACE_DIR}/checkpoints}
TENSORBOARD_DIR=${TENSORBOARD_DIR:-${WORKSPACE_DIR}/tensorboard}
mkdir -p ${CHECKPOINT_DIR} ${TENSORBOARD_DIR}

# Build C++ helpers if not already compiled (needed for mock data)
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
  # Only load from checkpoint if one exists (skip on first run).
  if [ -d "${CHECKPOINT_DIR}" ] && [ "$(ls -A ${CHECKPOINT_DIR} 2>/dev/null)" ]; then
    CHECKPOINT_ARGS="${CHECKPOINT_ARGS} --load ${CHECKPOINT_DIR}"
  fi
  DATA_CACHE_ARGS="--data-cache-path ${WORKSPACE_DIR}/data_cache"
  mkdir -p ${WORKSPACE_DIR}/data_cache
fi

# Dynamic computation — values from Go template at render time.
{{- $totalGPUs := mul (int .NodesPerJob) (int .GpusPerNode) -}}
{{- $mbs := 1 -}}
{{- $gbs := mul $totalGPUs $mbs -}}
TOTAL_GPUS={{ $totalGPUs }}
TP=${TENSOR_PARALLELISM:-{{ .TP }}}
PP=${PIPELINE_PARALLELISM:-{{ .PP }}}
MBS=${MICRO_BATCH_SIZE:-{{ $mbs }}}
GBS=${GLOBAL_BATCH_SIZE:-{{ $gbs }}}

echo "TOTAL_GPUS=${TOTAL_GPUS} GBS=${GBS} MBS=${MBS} TP=${TP} PP=${PP}"

exec torchrun \
  --nnodes $PET_NNODES \
  --nproc-per-node $PET_NPROC_PER_NODE \
  ${MEGATRON_PATH}/pretrain_gpt.py \
  --attention-backend flash \
  --distributed-timeout-minutes 60 \
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
  --exit-duration-in-mins {{ .ExitDurationMins }} \
  --train-iters {{ .MaxSteps }} \
  --lr-decay-iters 1830030 \
  --lr 6e-4 \
  --min-lr 6e-6 \
  --weight-decay 0.1 \
  --clip-grad 1.0 \
  --lr-decay-style cosine \
  --lr-warmup-iters 5 \
  --eval-iters 1 \
  --eval-interval {{ .MaxSteps }} \
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
  --num-layers 32 \
  --hidden-size 4096 \
  --ffn-hidden-size 21504 \
  --num-attention-heads 32 \
  --seq-length ${SEQ_LENGTH:-8192} \
  --max-position-embeddings ${SEQ_LENGTH:-8192} \
  --num-query-groups 8 \
  --tensor-model-parallel-size $TP \
  --pipeline-model-parallel-size $PP \
  --micro-batch-size $MBS \
  --global-batch-size $GBS \
  ${RECOMPUTE_ARGS} \
  $CHECKPOINT_ARGS \
  $DATA_CACHE_ARGS \
  --save-interval ${SAVE_INTERVAL:-250} \
  --save-retain-interval ${SAVE_RETAIN_INTERVAL:-1000} \
  --tensorboard-dir ${TENSORBOARD_DIR}
