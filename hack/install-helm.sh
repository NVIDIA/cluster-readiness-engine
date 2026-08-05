#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Install Helm when it is not already on PATH (e.g. before CI image rebuild).
set -euo pipefail

HELM_VERSION="${HELM_VERSION:-3.17.3}"

if command -v helm >/dev/null 2>&1; then
  helm version --short
  exit 0
fi

ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${ARCH}.tar.gz" \
  | tar -xz -C "$tmp" "linux-${ARCH}/helm"
install -m 0755 "$tmp/linux-${ARCH}/helm" /usr/local/bin/helm
helm version --short
