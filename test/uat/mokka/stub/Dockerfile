# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# NCCL test stub for the Mokka environment (ADR-096). Selected per
# Certification with spec.image (ADR-077).
#
# OpenMPI, sshd and orted are real, so NVCRE's MPI bootstrap and the kubelet
# log path run as they would on hardware. Only the measuring binary is
# replaced: all_reduce_perf_mpi replays a recorded nccl-tests log on rank 0.
#
# STUB_MODE picks the behaviour baked into the image, because a Certification
# can set the image but not the workload's environment:
#   pass   replay the log and exit 0
#   fail   print a partial log and exit 1
#   empty  replay everything except the result rows and exit 0
#   slow   wait STUB_SLOW_SECONDS before replaying, for deletion tests
#
# logs/allreduce.log is pkg/nccl/testdata/real_a100_allreduce_2node_pdt.log
# with the Kubernetes timestamps, the kubectl container banner and the ssh
# known-hosts warnings removed; the real ssh prints its own warnings here.
#
# Pinned, like the rest of the lane: the base by digest (ubuntu:24.04, the
# multi-arch index), and OpenMPI to the 4.1 release whose fork behaviour the
# mpirun wrapper works around. A failing nightly then always has a repository
# change to point at. Bump both together.
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ARG STUB_MODE=pass
ARG STUB_SLOW_SECONDS=300

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openmpi-bin=4.1.6-7ubuntu2 libopenmpi3t64=4.1.6-7ubuntu2 \
      openssh-server openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /usr/local/mpi/bin /opt/stub

COPY logs/allreduce.log /opt/stub/allreduce.log
COPY all_reduce_perf_mpi /usr/local/bin/all_reduce_perf_mpi
COPY mpirun /usr/local/mpi/bin/mpirun
RUN chmod 0755 /usr/local/bin/all_reduce_perf_mpi /usr/local/mpi/bin/mpirun \
 && printf 'STUB_MODE=%s\nSTUB_SLOW_SECONDS=%s\n' "${STUB_MODE}" "${STUB_SLOW_SECONDS}" > /opt/stub/mode
