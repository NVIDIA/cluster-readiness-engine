#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

for tool in kind kubectl helm go; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "Setup UAT requires ${tool} in PATH"
    exit 1
  fi
done
helm_version="$(helm version --template '{{.Version}}')"
if [[ "${helm_version}" != v3.* ]]; then
  echo "Setup UAT requires Helm 3 because its collision-free external fixture uses an executable post-renderer; found ${helm_version}"
  exit 1
fi

cluster="${KIND_CLUSTER_SETUP_UAT:-nvcre-setup-uat-$$}"
workdir="$(mktemp -d)"
kubeconfig="${workdir}/kubeconfig"
owned=0
export HELM_PLUGINS="${workdir}/helm-plugins"
mkdir -p "${HELM_PLUGINS}"

cleanup() {
  status=$?
  if [[ ${status} -ne 0 ]]; then
    mkdir -p "${workdir}/diagnostics"
    kind export logs "${workdir}/diagnostics/kind" --name "${cluster}" || true
    kubectl --kubeconfig "${kubeconfig}" get all --all-namespaces -o wide >"${workdir}/diagnostics/resources.txt" 2>&1 || true
    kubectl --kubeconfig "${kubeconfig}" get events --all-namespaces --sort-by=.lastTimestamp >"${workdir}/diagnostics/events.txt" 2>&1 || true
    echo "Setup UAT diagnostics: ${workdir}/diagnostics"
    if [[ -n "${GITHUB_ENV:-}" ]]; then
      echo "SETUP_UAT_DIAGNOSTICS=${workdir}/diagnostics" >>"${GITHUB_ENV}"
    fi
  fi
  if [[ ${owned} -eq 1 ]]; then
    kind delete cluster --name "${cluster}" || true
  fi
  exit "${status}"
}
if kind get clusters | grep -Fxq "${cluster}"; then
  echo "Refusing to reuse existing Kind cluster ${cluster}"
  exit 1
fi

owned=1
trap cleanup EXIT
kind create cluster --name "${cluster}" --kubeconfig "${kubeconfig}"
context="kind-${cluster}"
go build -ldflags "-s -w" -o bin/nvcrectl ./cmd/nvcrectl/
KUBECONFIG="${kubeconfig}" KUBE_CONTEXT="${context}" NVCRECTL="${PWD}/bin/nvcrectl" \
  go test -tags=uat ./test/uat/setup/ -v -timeout 30m -count=1
