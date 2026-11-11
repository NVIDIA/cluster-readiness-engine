# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The CUTLASS GEMM burn-in harness for compute/cutlass-gemm (ADR-091).
#
# Its own context directory, not the repository root. The root .dockerignore
# excludes everything and re-includes only Go sources, so a build rooted there
# cannot see this script.
#
# Two stages: compile cutlass_profiler, then carry only the binary and the
# runtime it needs into a CUDA runtime image. The builder is a CUDA devel image
# of several gigabytes and the profiler is one executable.

# ---------------------------------------------------------------------------
# Pins. A bump touches all four.
# ---------------------------------------------------------------------------
ARG CUDA_VERSION=12.8.1
ARG UBUNTU_VERSION=24.04
ARG CUTLASS_REF=v3.9.2

FROM nvcr.io/nvidia/cuda:${CUDA_VERSION}-devel-ubuntu${UBUNTU_VERSION} AS build
ARG CUTLASS_REF

# CUTLASS_LIBRARY_KERNELS is the single most important setting here.
#
# The default, `all`, generates tens of thousands of kernel instantiations and
# turns this into a multi-hour build that produces a binary of several
# gigabytes. This entry runs one operation: a large dense GEMM in a 16-bit
# type. Narrowing the filter to that keeps the build and the image to something
# CI can carry, at the cost of the profiler being unable to run anything else
# in this image, which is the correct trade for a single-purpose harness.
#
# Architectures, from pkg/catalog/entries/_lib/gpu-defaults.yaml:
#   80   A100
#   89   L40 / L40S
#   90a  H100 / H200          (a: arch-conditional features)
#   100a B200 / GB200
#   103a B300 / GB300
#   120a RTX PRO 6000 Blackwell
ARG CUTLASS_ARCHS="80;89;90a;100a;103a;120a"
ARG CUTLASS_KERNELS="cutlass_simt_sgemm_*"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates cmake git ninja-build python3 \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch "${CUTLASS_REF}" \
        https://github.com/NVIDIA/cutlass.git /src/cutlass

WORKDIR /src/build
RUN cmake /src/cutlass -G Ninja \
      -DCMAKE_BUILD_TYPE=Release \
      -DCUTLASS_NVCC_ARCHS="${CUTLASS_ARCHS}" \
      -DCUTLASS_LIBRARY_KERNELS="${CUTLASS_KERNELS}" \
      -DCUTLASS_ENABLE_TESTS=OFF \
      -DCUTLASS_UNITY_BUILD_ENABLED=ON \
    && ninja cutlass_profiler \
    && strip tools/profiler/cutlass_profiler

# ---------------------------------------------------------------------------
# Runtime.
# ---------------------------------------------------------------------------
FROM nvcr.io/nvidia/cuda:${CUDA_VERSION}-runtime-ubuntu${UBUNTU_VERSION}

# The harness needs a shell with mapfile (bash), awk, and nvidia-smi. awk is
# gawk or mawk from the base; nvidia-smi arrives from the container runtime's
# driver mount, not from the image.
RUN apt-get update \
    && apt-get install -y --no-install-recommends bash gawk \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /src/build/tools/profiler/cutlass_profiler /usr/local/bin/cutlass_profiler
COPY entrypoint.sh /usr/local/bin/nvcre-gemm-burnin
RUN chmod 0755 /usr/local/bin/nvcre-gemm-burnin /usr/local/bin/cutlass_profiler

# Not root. The harness reads nvidia-smi and runs a compute kernel; neither
# needs privilege, and unlike dcgm-level4 there is no hostengine to talk to.
RUN useradd --system --uid 65532 --no-create-home --shell /usr/sbin/nologin nvcre
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/nvcre-gemm-burnin"]
