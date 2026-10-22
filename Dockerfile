# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Build the manager binary.
# Base images are pinned by digest so a rebuild uses the same bits every time.
# The tag is kept for readability; the digest is what resolves. Dependabot
# raises the digest on its weekly docker run.
FROM public.ecr.aws/docker/library/golang:1.27.3-trixie@sha256:1896654cfc5c2b5ad74c07f29cb36f5d2d1cbed42e3054070fbfe444e521e83c AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -ldflags "-s -w -X main.version=${VERSION}" -o manager ./cmd/manager/

FROM nvcr.io/nvidia/distroless/static:v4.1.4@sha256:d9bff7801f2963231e01e567ab73d70f443b80c62306cec77d9f973a544db776
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
