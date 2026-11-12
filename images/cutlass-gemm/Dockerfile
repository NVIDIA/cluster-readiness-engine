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
# Pins. Neither of the first two is free to move down.
#
# CUDA 13.0.2, because B300 needs CUDA 13.0 or newer and native sm_103 code
# (docs/operations/faq.md, and the AWS P6-B300 requirements it cites). 13.0.2
# specifically because that is the CUDA version this catalog's own AWS B300
# NCCL image already pins, so the harness and the collective entries agree on
# a toolkit for the same hardware.
#
# CUTLASS v4.2.0, because that is the first release carrying sm103. An earlier
# pin here was v3.9.2, which knows sm100 and sm120 but not sm103: the build
# would have produced an image with no kernels for B300 or GB300, the two
# architectures whose floors this entry enforces, and the gap would have shown
# up as a workload failure on real hardware rather than as a build error.
# v4.2.0 covers every architecture in CUTLASS_ARCHS below; a bump may raise it
# but must not lower it past 4.2.0.
# ---------------------------------------------------------------------------
ARG CUDA_VERSION=13.0.2
ARG UBUNTU_VERSION=24.04
ARG CUTLASS_REF=v4.2.0

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
    && ninja cutlass_profiler

# Stage the binary together with the shared libraries it links against.
#
# cutlass_profiler is NOT self-contained: CUTLASS builds libcutlass.so and
# links the profiler against it, so copying the binary alone produced an image
# whose only executable failed at startup with "libcutlass.so: cannot open
# shared object file". Every GPU would have reported no result and the run
# would have failed the node for a missing library.
#
# Collected by searching the build tree rather than naming a path, because the
# path is a CUTLASS build-layout detail that a version bump may move, and a
# stale COPY of a file that no longer exists fails the build loudly while a
# stale directory can silently copy nothing. If a future CUTLASS links the
# profiler statically, the search simply finds nothing and the result still
# works.
RUN set -eux; \
    mkdir -p /out/bin /out/lib; \
    strip tools/profiler/cutlass_profiler; \
    cp tools/profiler/cutlass_profiler /out/bin/; \
    find . -name 'libcutlass*.so*' -exec cp -av {} /out/lib/ \;

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

COPY --from=build /out/bin/cutlass_profiler /usr/local/bin/cutlass_profiler
COPY --from=build /out/lib/ /usr/local/lib/cutlass/
COPY entrypoint.sh /usr/local/bin/nvcre-gemm-burnin
RUN chmod 0755 /usr/local/bin/nvcre-gemm-burnin /usr/local/bin/cutlass_profiler

# Where libcutlass.so lives. ldconfig rather than LD_LIBRARY_PATH: the variable
# is inherited by anything the harness runs and is the kind of thing a workload
# override can clobber, while the cache is a property of the image.
RUN echo /usr/local/lib/cutlass > /etc/ld.so.conf.d/cutlass.conf && ldconfig

# Fail the build rather than the burn-in if the profiler cannot start.
#
# Without this the image was publishable with its only executable broken, and
# the first sign would have been every GPU on a real node reporting no result,
# which reads as failed hardware rather than a packaging mistake.
#
# The test is "no unresolved shared library", not "--help exits 0". That is the
# precise failure this guards: a tool is entitled to a non-zero exit from
# --help, and a smoke test resting on that would fail builds of a perfectly
# good image. An unresolved library, by contrast, is never acceptable.
RUN set -eux; \
    ldd /usr/local/bin/cutlass_profiler > /tmp/ldd.txt; \
    cat /tmp/ldd.txt; \
    if grep -q 'not found' /tmp/ldd.txt; then \
      echo "cutlass_profiler has unresolved shared libraries; it cannot start"; \
      exit 1; \
    fi; \
    rm -f /tmp/ldd.txt

# Not root. The harness reads nvidia-smi and runs a compute kernel; neither
# needs privilege, and unlike dcgm-level4 there is no hostengine to talk to.
RUN useradd --system --uid 65532 --no-create-home --shell /usr/sbin/nologin nvcre
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/nvcre-gemm-burnin"]
