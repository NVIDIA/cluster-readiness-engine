#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Sustained per-GPU GEMM burn-in (ADR-091).
#
# cutlass_profiler is single-device and has no soak mode: it takes --device=<n>
# and runs for a duration or an iteration count, once. This drives it into a
# burn-in by launching one process per GPU CONCURRENTLY and looping each for the
# configured duration.
#
# Concurrency is a requirement of the design, not a speed optimisation. A
# harness that walks the devices one at a time reports eight per-GPU numbers
# that each look fine, because it never puts the chassis under the power and
# thermal load that makes a marginal GPU fail. Sequential per-device
# benchmarking is a different test from a burn-in and would not find what this
# entry exists to find (ADR-091, "Entry shape").
#
# Each sweep emits one NVCRE_ROW per GPU, which is how the rows reach the
# controller (ADR-090). Every GPU shares the sample ordinal for a given sweep,
# which is what makes "GPU 3 fell behind at sample 900 while the other seven
# held" answerable from the rows.
#
# THE PRECISION IS NOT ARBITRARY. DCGM's per-SKU targets are derived from an
# FP32-pipe measurement: field 1007, which dcgmproftester reports and the
# target table is calibrated from, is DCGM_FI_PROF_PIPE_FP32_ACTIVE, and every
# target in that table lands at 44-62% of its SKU's FP32 non-tensor peak. The
# default dtype here is therefore f32, so the number this harness measures is
# the same quantity DCGM's target describes.
#
# Running f16 instead would be a silent disaster rather than a visible one: an
# H100 sustains roughly 700 TFLOPS of FP16 tensor throughput against a 29.25
# TFLOPS target, so the floor would be about 4% of the achieved value and the
# gate would pass every GPU including a badly degraded one, while still
# reporting itself as armed.
#
# THE VERDICT IS DCGM'S, UNCHANGED.
#
# DCGM's Targeted Stress plugin fails a GPU when
#
#   maxVal < discountMultiplier * target_perf_min_ratio * target_stress
#
# (TargetedStress_wrapper.cpp). All three parts are carried over:
#
#   maxVal                 the MAXIMUM achieved throughput over the run
#   target_perf_min_ratio  NVCRE_GEMM_MIN_RATIO_PERCENT, DCGM default 0.95
#   target_stress          NVCRE_GEMM_TARGET_GFLOPS, DCGM's per-SKU table
#
# discountMultiplier is 1 here, which is the faithful value rather than an
# omission. DCGM's factor discounts the share of its measured interval spent
# copying over PCIe rather than doing GEMM work, because its own maxVal
# includes that copy time. cutlass_profiler reports throughput for the GEMM
# kernel alone, so there is no copy time in this maxVal to discount, and
# applying a discount anyway would lower the bar below DCGM's.
#
# A non-zero exit fails the Job and the existing failed-node path records the
# node. The rows name the GPU; the exit code fails the node. ADR-091 decision
# 4 explains why this is not a threshold registry key.
#
# The per-sample rows are reported regardless of the verdict, which is the half
# of this entry DCGM does not provide: dcgm-level4 passes --json and NVCRE
# discards its per-GPU values. A GPU that peaks above the floor and degrades
# through the soak therefore PASSES, exactly as it does under DCGM, and its
# decay is visible in the rows and in the per-GPU summary. See the amendment on
# ADR-091 for what that costs.

set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration, all from the entry's template so nothing is baked into the
# image that an operator might need to change.
# ---------------------------------------------------------------------------

: "${NVCRE_GEMM_DURATION_SECONDS:=1800}"   # total soak length
: "${NVCRE_GEMM_SAMPLE_SECONDS:=30}"       # length of one sweep, so one sample
: "${NVCRE_GEMM_TARGET_GFLOPS:=0}"          # DCGM target_stress, per GPU
: "${NVCRE_GEMM_MIN_RATIO_PERCENT:=0}"      # DCGM target_perf_min_ratio, as percent
: "${NVCRE_GEMM_FLOOR_TFLOPS:=0}"           # absolute override of the product
: "${NVCRE_GEMM_OPERATION:=gemm}"          # cutlass_profiler --operation
: "${NVCRE_GEMM_M:=8192}"
: "${NVCRE_GEMM_N:=8192}"
: "${NVCRE_GEMM_K:=8192}"
: "${NVCRE_GEMM_DTYPE:=f32}"               # profiler --A/--B element type
: "${CUTLASS_PROFILER:=cutlass_profiler}"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

# ---------------------------------------------------------------------------
# The floor, in GFLOPS.
#
# GFLOPS throughout the comparison because that is the unit DCGM's target is
# stated in, so the arithmetic is DCGM's with no conversion step to get wrong.
# Rows and the summary report TFLOPS, which is what a human reads.
# ---------------------------------------------------------------------------

FLOOR_GFLOPS=0
FLOOR_SOURCE="none"
if (( NVCRE_GEMM_FLOOR_TFLOPS > 0 )); then
  # An operator's absolute floor replaces the product outright rather than
  # scaling it: someone who states a number in TFLOPS means that number.
  FLOOR_GFLOPS=$(( NVCRE_GEMM_FLOOR_TFLOPS * 1000 ))
  FLOOR_SOURCE="gemmFloorTFLOPS=${NVCRE_GEMM_FLOOR_TFLOPS}"
elif (( NVCRE_GEMM_TARGET_GFLOPS > 0 && NVCRE_GEMM_MIN_RATIO_PERCENT > 0 )); then
  FLOOR_GFLOPS=$(( NVCRE_GEMM_TARGET_GFLOPS * NVCRE_GEMM_MIN_RATIO_PERCENT / 100 ))
  FLOOR_SOURCE="${NVCRE_GEMM_MIN_RATIO_PERCENT}% of ${NVCRE_GEMM_TARGET_GFLOPS} GFLOPS"
fi

# ---------------------------------------------------------------------------
# Device inventory. The row's componentID is the GPU UUID, because an index is
# only meaningful inside one node and a UUID survives a reboot that renumbers
# the bus. The index is carried too: ADR-090 defines it as the device index
# within the node, which is what makes a row addressable to a slot.
# ---------------------------------------------------------------------------

mapfile -t GPU_UUIDS < <(nvidia-smi --query-gpu=uuid --format=csv,noheader 2>/dev/null)
if [[ ${#GPU_UUIDS[@]} -eq 0 ]]; then
  log "FATAL: no GPUs visible to nvidia-smi; refusing to report a pass for a burn-in that never ran"
  exit 2
fi
GPU_COUNT=${#GPU_UUIDS[@]}
log "Found ${GPU_COUNT} GPU(s); soaking for ${NVCRE_GEMM_DURATION_SECONDS}s in ${NVCRE_GEMM_SAMPLE_SECONDS}s sweeps"
if (( FLOOR_GFLOPS > 0 )); then
  log "Floor: ${FLOOR_GFLOPS} GFLOPS per GPU, compared against each GPU's best sample (${FLOOR_SOURCE})"
else
  log "Floor: none configured; reporting per-GPU rows without a verdict"
fi

# Scratch space for per-device results.
#
# Checked rather than assumed. An unset or unwritable WORK made every path
# below absolute ("/dev0.csv"), so every sweep failed, every GPU reported
# outcome=error, and the run exited 1 saying no GPU sustained the floor. A
# tooling failure presented as a hardware failure on every GPU in the node is
# the worst answer this script can give: it fails a healthy node and sends an
# operator looking for a GPU fault that does not exist.
#
# Exit 2, not 1. Exit 1 means the test ran and a GPU was below the floor;
# exit 2 means the test could not run, which is the same distinction the
# missing-GPU check above makes.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/nvcre-gemm.XXXXXX" 2>/dev/null)" || WORK=""
if [[ -z "${WORK}" || ! -d "${WORK}" || ! -w "${WORK}" ]]; then
  log "FATAL: could not create a writable scratch directory under ${TMPDIR:-/tmp}; cannot run the burn-in"
  exit 2
fi
trap 'rm -rf "${WORK}"' EXIT

# ---------------------------------------------------------------------------
# One sweep on one device. Writes the achieved GFLOP/s to a per-device file so
# the parent can read every device's result after the sweep without parsing
# interleaved stdout from eight concurrent processes.
# ---------------------------------------------------------------------------
run_device() {
  local dev="$1" out="$2"
  # --profiling-iterations=0 with --profiling-duration makes the profiler run
  # for a wall-clock window instead of a fixed iteration count, which is what
  # keeps every device under load for the whole sweep regardless of how fast
  # any one of them is.
  "${CUTLASS_PROFILER}" \
    --operation="${NVCRE_GEMM_OPERATION}" \
    --device="${dev}" \
    --m="${NVCRE_GEMM_M}" --n="${NVCRE_GEMM_N}" --k="${NVCRE_GEMM_K}" \
    --A="${NVCRE_GEMM_DTYPE}":column --B="${NVCRE_GEMM_DTYPE}":column \
    --profiling-iterations=0 \
    --profiling-duration="$((NVCRE_GEMM_SAMPLE_SECONDS * 1000))" \
    --verification-enabled=false \
    --output="${out}.csv" \
    >"${out}.log" 2>&1
  local rc=$?

  # The profiler's CSV carries a GFLOPs column. Take the best row: within one
  # sweep the profiler may try several kernels, and the achievable throughput
  # for the device is the best of them, not the average over tuning attempts.
  #
  # awk rather than a python heredoc. The heredoc fed the program on stdin,
  # which works in the foreground and silently yields nothing once this
  # function is backgrounded, because an asynchronous command's stdin is
  # redirected from /dev/null when job control is off. Every sweep then
  # reported outcome=error with a value of zero, so a healthy GPU looked dead.
  # awk takes its program as an argument and the image needs no python.
  local gflops=""
  if [[ -s "${out}.csv" ]]; then
    gflops="$(awk -F, '
      NR == 1 {
        for (i = 1; i <= NF; i++) {
          gsub(/^[ \t"]+|[ \t"\r]+$/, "", $i)
          if ($i == "GFLOPs" || $i == "GFLOPs/s" || $i == "gflops") col = i
        }
        next
      }
      col && ($col + 0) > best { best = $col + 0 }
      END { printf "%.3f", best + 0 }
    ' "${out}.csv")"
  fi
  printf '%s %s\n' "${rc}" "${gflops:-0}" >"${out}.result"
}

# ---------------------------------------------------------------------------
# The soak. Sweeps run back to back until the duration is spent; within a
# sweep every device runs at once.
# ---------------------------------------------------------------------------

declare -a MAX_GFLOPS MIN_GFLOPS SUM_GFLOPS SAMPLES FAILED_SWEEPS
for ((d = 0; d < GPU_COUNT; d++)); do
  MAX_GFLOPS[d]=""
  MIN_GFLOPS[d]=""
  SUM_GFLOPS[d]=0
  SAMPLES[d]=0
  FAILED_SWEEPS[d]=0
done

START="${SECONDS}"
sample=0
while (( SECONDS - START < NVCRE_GEMM_DURATION_SECONDS )); do
  sweep_start="${SECONDS}"
  pids=()
  for ((d = 0; d < GPU_COUNT; d++)); do
    run_device "${d}" "${WORK}/dev${d}" &
    pids+=("$!")
  done
  # Wait for the whole sweep. A device that finishes early is idle until the
  # others are done, which keeps the sample ordinal meaningful: every row at
  # ordinal N was measured in the same window under the same load.
  for pid in "${pids[@]}"; do wait "${pid}" || true; done

  # Hold the sweep cadence even when the profiler returns early.
  #
  # The profiler is asked to run for the sweep length, so normally this sleeps
  # for nothing. It matters when the profiler exits immediately, which is what
  # a missing binary, an unsupported architecture or a driver fault looks
  # like: the loop would then spin at full speed, emitting a row per GPU per
  # iteration for the whole soak. A 30 minute run measured 255 rows from a
  # 2 second test that way. Rows are the input to a bounded row budget
  # (ADR-090), so a spinning harness does not just fill the log, it truncates
  # the result set it exists to produce. The cadence bounds the row count at
  # duration/sample per GPU by construction, whatever the profiler does.
  sweep_elapsed=$(( SECONDS - sweep_start ))
  if (( sweep_elapsed < NVCRE_GEMM_SAMPLE_SECONDS )); then
    sleep "$(( NVCRE_GEMM_SAMPLE_SECONDS - sweep_elapsed ))"
  fi

  ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  for ((d = 0; d < GPU_COUNT; d++)); do
    rc=1; gflops=0
    if [[ -r "${WORK}/dev${d}.result" ]]; then
      read -r rc gflops <"${WORK}/dev${d}.result"
    fi
    tflops="$(awk -v g="${gflops}" 'BEGIN { printf "%.2f", g / 1000.0 }')"

    outcome=pass
    if [[ "${rc}" != "0" ]] || awk -v g="${gflops}" 'BEGIN { exit !(g <= 0) }'; then
      outcome=error
      FAILED_SWEEPS[d]=$(( FAILED_SWEEPS[d] + 1 ))
    elif (( FLOOR_GFLOPS > 0 )) && \
         awk -v g="${gflops}" -v f="${FLOOR_GFLOPS}" 'BEGIN { exit !(g < f) }'; then
      # This sample is under the floor. "below" rather than "fail": the verdict
      # is DCGM's and is taken over the whole run from the maximum, so one low
      # sample does not fail the GPU. The row still records it, which is how a
      # decay stays visible in evidence that the verdict does not act on.
      outcome=below
    fi

    # The row. Read by the controller's structured-rows producer: the marker,
    # then one JSON object. Node is added by the controller, which knows the
    # Kubernetes node name this pod landed on; a harness does not.
    printf 'NVCRE_ROW {"componentID":"%s","index":%d,"name":"achievedTFLOPS","value":%s,"unit":"TFLOPS","sample":%d,"timestamp":"%s","outcome":"%s"}\n' \
      "${GPU_UUIDS[d]}" "${d}" "${tflops}" "${sample}" "${ts}" "${outcome}"

    if [[ "${outcome}" != "error" ]]; then
      SUM_GFLOPS[d]="$(awk -v s="${SUM_GFLOPS[d]}" -v g="${gflops}" 'BEGIN { printf "%.3f", s + g }')"
      SAMPLES[d]=$(( SAMPLES[d] + 1 ))
      if [[ -z "${MIN_GFLOPS[d]}" ]] || \
         awk -v g="${gflops}" -v m="${MIN_GFLOPS[d]}" 'BEGIN { exit !(g < m) }'; then
        MIN_GFLOPS[d]="${gflops}"
      fi
      # The verdict basis: DCGM compares the maximum achieved value.
      if [[ -z "${MAX_GFLOPS[d]}" ]] || \
         awk -v g="${gflops}" -v m="${MAX_GFLOPS[d]}" 'BEGIN { exit !(g > m) }'; then
        MAX_GFLOPS[d]="${gflops}"
      fi
    fi
  done
  sample=$(( sample + 1 ))
done

log "Soak complete: ${sample} sample(s) per GPU over $(( SECONDS - START ))s"

# ---------------------------------------------------------------------------
# The verdict, on the MAXIMUM achieved result, which is DCGM's.
#
# The minimum is reported beside it and judged on by nothing. That asymmetry is
# deliberate and is the honest statement of this entry's limit: a GPU whose
# maximum clears the floor passes even if its minimum does not, so a thermal
# decay through the soak is recorded in the rows and in the line below without
# failing the node. ADR-091's amendment records what that gives up.
#
# The summary goes at the END of stdout deliberately. Failure-log capture keeps
# only the last 800 lines and 32 KiB, and that bound was already widened once
# because dcgmi hid its verdict behind a closing block. A verdict printed first
# would be the first thing truncated.
# ---------------------------------------------------------------------------

failed=0
printf '\n'
log "Per-GPU results (verdict on the maximum, as DCGM compares it):"
for ((d = 0; d < GPU_COUNT; d++)); do
  if [[ ${SAMPLES[d]} -eq 0 ]]; then
    log "  GPU ${d} ${GPU_UUIDS[d]}: NO RESULT (${FAILED_SWEEPS[d]} failed sweep(s))"
    failed=1
    continue
  fi
  max_t="$(awk -v g="${MAX_GFLOPS[d]}" 'BEGIN { printf "%.2f", g / 1000.0 }')"
  min_t="$(awk -v g="${MIN_GFLOPS[d]}" 'BEGIN { printf "%.2f", g / 1000.0 }')"
  avg_t="$(awk -v s="${SUM_GFLOPS[d]}" -v n="${SAMPLES[d]}" 'BEGIN { printf "%.2f", s / n / 1000.0 }')"
  verdict=OK
  if (( FLOOR_GFLOPS > 0 )) && \
     awk -v m="${MAX_GFLOPS[d]}" -v f="${FLOOR_GFLOPS}" 'BEGIN { exit !(m < f) }'; then
    verdict="BELOW FLOOR"
    failed=1
  fi
  log "  GPU ${d} ${GPU_UUIDS[d]}: max ${max_t} TFLOPS (the verdict), min ${min_t} TFLOPS," \
      "mean ${avg_t} TFLOPS over ${SAMPLES[d]} sample(s): ${verdict}"
  # Said out loud, because the verdict does not say it. A GPU whose maximum
  # clears the floor and whose minimum does not is the case DCGM's comparison
  # passes, and it is the case an operator most wants to know about.
  if (( FLOOR_GFLOPS > 0 )) && [[ "${verdict}" == "OK" ]] && \
     awk -v m="${MIN_GFLOPS[d]}" -v f="${FLOOR_GFLOPS}" 'BEGIN { exit !(m < f) }'; then
    log "    note: this GPU fell to ${min_t} TFLOPS at some point, under the" \
        "${FLOOR_GFLOPS} GFLOPS floor. DCGM's comparison is on the maximum, so it passes."
  fi
done

if [[ ${failed} -ne 0 ]]; then
  printf '\n'
  if (( FLOOR_GFLOPS > 0 )); then
    log "FAILED: at least one GPU never reached ${FLOOR_GFLOPS} GFLOPS (${FLOOR_SOURCE}). Named above; per-GPU rows carry every sample."
  else
    log "FAILED: at least one GPU produced no result at all. Named above."
  fi
  exit 1
fi
printf '\n'
if (( FLOOR_GFLOPS > 0 )); then
  log "PASSED: every GPU reached ${FLOOR_GFLOPS} GFLOPS (${FLOOR_SOURCE}) on at least one of ${sample} sample(s), which is DCGM's comparison."
else
  log "PASSED: ${sample} sample(s) collected from every GPU; no floor configured, so no performance verdict was made."
fi
