# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# CUTLASS GEMM stub for the Mokka environment (ADR-096), selected per
# Certification with spec.image (ADR-077).
#
# The harness under test is the REAL one: images/cutlass-gemm/entrypoint.sh is
# copied in unchanged, so the Mokka lane exercises the production sweep loop,
# the production row emission and the production DCGM-arithmetic verdict. Only
# the two measuring binaries are replaced, which is the same discipline the
# NCCL stub applies to all_reduce_perf_mpi.
#
# That matters more here than it would for a pure fake: the verdict is this
# repository's own arithmetic rather than a vendor tool's, so the thing most
# worth exercising without GPUs is exactly the code a reimplementation would
# not cover.
#
# Its own directory, not the NCCL stub's. That stub pins OpenMPI and an Ubuntu
# digest together with the chart and driver versions in setup.sh, and a GEMM
# stub needs no MPI at all; keeping it separate leaves it off that treadmill.
#
# STUB_MODE picks the behaviour baked into the image, because a Certification
# can set the image but not the workload's environment:
#   pass      every GPU holds target; exit 0
#   slow      one GPU never reaches the floor on any sweep, so its maximum is
#             under it and DCGM's comparison fails; exit 1
#   degrade   one GPU hits target on the first sweep then falls below the
#             floor. Its maximum still clears, so this PASSES (exit 0), which
#             is what DCGM does. The mode exists to exercise the reporting:
#             the rows carry the decay and the summary names the GPU, which is
#             the half of this entry DCGM does not provide.
#   dead      one GPU returns no result at all; exit 1
#
# Base pinned by digest, the same ubuntu:24.04 multi-arch index the NCCL stub
# uses, so a failing nightly has a repository change to point at.
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ARG STUB_MODE=pass
# The throughput a healthy GPU reports, in GFLOPS. 30000 clears the floors for
# h100 (27787) and l40s (28500) and sits under b200's (32150), so the default
# profile reports a pass on the Mokka h100 profile.
ARG STUB_GFLOPS=30000
# What the degraded GPU drops to after its first sweep. 20000 is below every
# armed floor in the entry's table.
ARG STUB_DEGRADED_GFLOPS=20000
# Which device index misbehaves under the degrade and dead modes.
ARG STUB_BAD_DEVICE=2

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bash gawk coreutils \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/stub

COPY nvidia-smi /usr/local/bin/nvidia-smi
COPY cutlass_profiler /usr/local/bin/cutlass_profiler
# The production harness, copied verbatim.
COPY entrypoint.sh /usr/local/bin/nvcre-gemm-burnin

RUN chmod 0755 /usr/local/bin/nvidia-smi /usr/local/bin/cutlass_profiler \
      /usr/local/bin/nvcre-gemm-burnin \
 && printf 'STUB_MODE=%s\nSTUB_GFLOPS=%s\nSTUB_DEGRADED_GFLOPS=%s\nSTUB_BAD_DEVICE=%s\n' \
      "${STUB_MODE}" "${STUB_GFLOPS}" "${STUB_DEGRADED_GFLOPS}" "${STUB_BAD_DEVICE}" \
      > /opt/stub/mode

ENTRYPOINT ["/usr/local/bin/nvcre-gemm-burnin"]
