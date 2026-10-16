#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Brings up a Kind cluster with Mokka-simulated GPUs and NVCRE installed from
# the working tree (ADR-096). Shared by `make mokka-up`, `make tilt-mokka` and
# the UAT runtime lane, so local development and CI run the same recipe.
#
# Usage: setup.sh <phase>...
#
#   cluster  create the Kind cluster (reused if it already exists)
#   infra    nvml-mock, NFD, device plugin, GFD, Trainer; DRA driver on gb200/gb300
#   stub     build and load the nvcre-nccl-stub:{pass,fail,empty,slow} images
#   nvcre    build and load the manager image, install the local chart, build nvcrectl
#   info     run `nvcrectl cluster info` against the cluster
#   up       cluster infra stub nvcre info
#   down     delete the Kind cluster
#
# Environment:
#   PROFILE        h100 (default), b200, gb200 or gb300
#   MOKKA_CLUSTER  Kind cluster name (default nvcre-mokka)
#   IMG            manager image (default ghcr.io/nvidia/cluster-readiness-engine/manager:mokka-test)
#   MOKKA_SRC      a k8s-test-infra checkout to build the IMEX shim from; by
#                  default MOKKA_REF is cloned into ${XDG_CACHE_HOME:-~/.cache}/nvcre-mokka/,
#                  outside the repository so neither controller-gen's ./... nor
#                  the manager's docker build context picks it up
#   KIND_NODE_IMAGE  optional Kind node image
#
# Every kubectl and helm call names the cluster's context explicitly, so the
# script never acts on whatever context happens to be current. Every phase can
# be re-run on an existing cluster: third-party charts already at the pinned
# version are skipped, and NVCRE is upgraded from the working tree.
# -E so the ERR trap below also fires inside functions.
set -Eeuo pipefail

# ── Pins ──
# The DRA driver stays on the version Mokka's own e2e suite pins and runs in
# CI (tests/e2e/VERSION-MATRIX.md), because that is what the IMEX shim is
# tested against. Move it when Mokka does.
NVML_MOCK_CHART="oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock"
NVML_MOCK_VERSION="0.5.0"
MOKKA_REF="v0.5.0"
NFD_REPO="https://kubernetes-sigs.github.io/node-feature-discovery/charts"
NFD_VERSION="0.19.0"
DRA_REPO="https://helm.ngc.nvidia.com/nvidia"
DRA_VERSION="0.5.0"
PROM_VERSION="v0.82.2"
TRAINER_VERSION="2.2.1"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

profile="${PROFILE:-h100}"
cluster="${MOKKA_CLUSTER:-nvcre-mokka}"
img="${IMG:-ghcr.io/nvidia/cluster-readiness-engine/manager:mokka-test}"
ctx="kind-${cluster}"
gpu_selector="mokka.nvidia.com/type=sgpu"
cd_image="nvcre-mokka-cd-daemon:${MOKKA_REF}-dra${DRA_VERSION}"
stub_modes=(pass fail empty slow)

k() { kubectl --context "${ctx}" "$@"; }
h() { helm --kube-context "${ctx}" "$@"; }
log() { printf '\n==> %s\n' "$*"; }
die() {
  echo "error: $*" >&2
  exit 1
}

uses_dra() { [[ "${profile}" == gb200 || "${profile}" == gb300 ]]; }

preflight() {
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || die "${tool} is required in PATH"
  done
}

cluster_exists() { kind get clusters 2>/dev/null | grep -Fxq "${cluster}"; }

require_cluster() {
  cluster_exists || die "Kind cluster ${cluster} does not exist; run: setup.sh cluster"
}

# deployed reports whether any revision of a release was successfully deployed
# from the given chart version. Third-party charts are installed once and then
# left alone: some, such as Kubeflow Trainer, have their webhook-cert Secrets
# rewritten by their own controllers, so a later `helm upgrade` (or rollback)
# fails on server-side-apply conflicts and leaves a failed revision on top of a
# working one. A release that never deployed is still installed.
deployed() {
  local release="$1" namespace="$2" chart="$3"
  h history "${release}" -n "${namespace}" -o json 2>/dev/null | tr '}' '\n' |
    grep "\"chart\":\"${chart}\"" | grep -Eq '"status":"(deployed|superseded)"'
}

# install_once installs a third-party chart unless it is already deployed at the
# pinned version. Arguments after the first four go to helm upgrade --install.
install_once() {
  local release="$1" namespace="$2" chart="$3" ref="$4"
  shift 4
  if deployed "${release}" "${namespace}" "${chart}"; then
    echo "${release}: ${chart} already deployed"
    return
  fi
  h upgrade --install "${release}" "${ref}" --namespace "${namespace}" --create-namespace "$@"
}

gpu_nodes() { k get nodes -l "${gpu_selector}" -o jsonpath='{.items[*].metadata.name}'; }

# On a failed phase, show enough to tell which component is stuck.
on_error() {
  local status=$?
  if cluster_exists; then
    echo "--- pods not Running or Completed in ${cluster} ---" >&2
    k get pods -A --no-headers 2>/dev/null | grep -vE 'Running|Completed' >&2 || true
  fi
  exit "${status}"
}
trap on_error ERR

phase_cluster() {
  preflight docker kind kubectl
  if cluster_exists; then
    echo "Kind cluster ${cluster} already exists; reusing it."
  else
    log "Creating Kind cluster ${cluster}"
    kind create cluster --name "${cluster}" --config "${script_dir}/kind/cluster.yaml" \
      ${KIND_NODE_IMAGE:+--image "${KIND_NODE_IMAGE}"}
    echo "Note: kind switched the current kubectl context to ${ctx}."
  fi
  k wait --for=condition=Ready nodes --all --timeout=180s
  [[ -n "$(gpu_nodes)" ]] || die "cluster ${cluster} has no ${gpu_selector} workers; it was not created by this script"
}

# The profile is recorded so a second `up` with another profile fails loudly
# instead of leaving one profile's labels and drivers under another's, and so
# the runtime lane can skip cases that do not apply.
record_profile() {
  local existing
  existing="$(k -n kube-system get configmap nvcre-mokka -o jsonpath='{.data.profile}' 2>/dev/null || true)"
  if [[ -n "${existing}" && "${existing}" != "${profile}" ]]; then
    die "cluster ${cluster} was set up for profile ${existing}; run 'make mokka-down' first, or set MOKKA_CLUSTER to a new name"
  fi
  k -n kube-system create configmap nvcre-mokka --from-literal=profile="${profile}" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
}

# GB200 and GB300 need every GPU worker in one NVLink clique. Node names depend
# on the cluster name, so the overlay is generated rather than checked in.
topology_overlay() {
  local out="$1" node
  {
    echo "topology:"
    echo "  enabled: true"
    echo "  domains:"
    echo "    - name: nvl72-a"
    echo "      uuid: \"00000000-0000-0000-0000-0000000000ab\""
    echo "      cliques:"
    echo "        - id: 0"
    echo "          nodes:"
    for node in $(gpu_nodes); do
      echo "            - ${node}"
    done
  } >"${out}"
}

wait_for_gpu_labels() {
  local node deadline=$((SECONDS + 300))
  for node in $(gpu_nodes); do
    until [[ -n "$(k get node "${node}" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')" ]]; do
      ((SECONDS < deadline)) || die "GFD labels never reached ${node}"
      sleep 5
    done
    echo "${node}: nvidia.com/gpu.product=$(k get node "${node}" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.product}')"
  done
}

mokka_src() {
  if [[ -n "${MOKKA_SRC:-}" ]]; then
    echo "${MOKKA_SRC}"
    return
  fi
  local dir="${XDG_CACHE_HOME:-${HOME}/.cache}/nvcre-mokka/${MOKKA_REF}"
  if [[ ! -d "${dir}/.git" ]]; then
    preflight git
    git clone --quiet --depth 1 --branch "${MOKKA_REF}" \
      https://github.com/NVIDIA/k8s-test-infra.git "${dir}" >&2
  fi
  echo "${dir}"
}

# Mokka's IMEX-shim overlay on the upstream DRA driver image. It repackages the
# proprietary nvidia-imex binary, so it is built locally and never published.
build_cd_image() {
  if docker image inspect "${cd_image}" >/dev/null 2>&1; then
    echo "Using existing ${cd_image}"
    return
  fi
  local src
  src="$(mokka_src)"
  log "Building ${cd_image} from ${src}"
  docker build --target daemon \
    --build-arg "COMPUTE_DOMAIN_DAEMON_IMAGE=nvcr.io/nvidia/dra-driver-nvidia-gpu:v${DRA_VERSION}" \
    --build-arg "GOLANG_VERSION=$(bash "${src}/hack/golang-version.sh")" \
    -t "${cd_image}" -f "${src}/deployments/nvml-mock/Dockerfile.compute-domain-daemon" "${src}"
}

phase_infra() {
  preflight docker kind kubectl helm
  require_cluster
  local values="${script_dir}/values/nvml-mock/${profile}.yaml"
  [[ -f "${values}" ]] || die "unknown PROFILE ${profile}; expected one of: $(cd "${script_dir}/values/nvml-mock" && ls ./*.yaml | xargs -n1 basename | sed 's/\.yaml$//' | tr '\n' ' ')"
  record_profile

  log "Installing nvml-mock ${NVML_MOCK_VERSION} (profile ${profile})"
  local args=(-f "${values}")
  if uses_dra; then
    local overlay
    overlay="$(mktemp)"
    topology_overlay "${overlay}"
    args+=(-f "${overlay}")
  fi
  install_once nvml-mock mokka "nvml-mock-${NVML_MOCK_VERSION}" "${NVML_MOCK_CHART}" \
    --version "${NVML_MOCK_VERSION}" "${args[@]}" --wait --timeout 10m

  log "Installing NFD ${NFD_VERSION}"
  install_once nfd node-feature-discovery "node-feature-discovery-${NFD_VERSION}" node-feature-discovery \
    --repo "${NFD_REPO}" --version "${NFD_VERSION}" --wait --timeout 5m

  log "Deploying the device plugin and GFD"
  k apply -f "${script_dir}/manifests/device-plugin.yaml" -f "${script_dir}/manifests/gfd.yaml"
  k -n kube-system rollout status daemonset/nvidia-device-plugin-mock --timeout=5m
  k -n kube-system rollout status daemonset/nvidia-gfd-mock --timeout=5m
  wait_for_gpu_labels
  # GFD does not write nvidia.com/gpu.present; the GPU Operator does, and
  # NVCRE's target selectors rely on it.
  # shellcheck disable=SC2046
  k label nodes $(gpu_nodes) nvidia.com/gpu.present=true --overwrite

  if uses_dra; then
    build_cd_image
    kind load docker-image "${cd_image}" --name "${cluster}"
    log "Installing dra-driver-nvidia-gpu ${DRA_VERSION} (ComputeDomains only)"
    install_once nvidia-dra-driver nvidia "dra-driver-nvidia-gpu-${DRA_VERSION}" dra-driver-nvidia-gpu \
      --repo "${DRA_REPO}" --version "${DRA_VERSION}" -f "${script_dir}/values/dra-driver.yaml" \
      --set image.repository="${cd_image%:*}" --set image.tag="${cd_image##*:}" --set image.pullPolicy=IfNotPresent \
      --wait --timeout 10m
  fi

  log "Installing Prometheus Operator CRDs ${PROM_VERSION} and Kubeflow Trainer ${TRAINER_VERSION}"
  k apply --server-side -f "https://github.com/prometheus-operator/prometheus-operator/releases/download/${PROM_VERSION}/stripped-down-crds.yaml" >/dev/null
  install_once kubeflow-trainer kubeflow-system "kubeflow-trainer-${TRAINER_VERSION}" \
    oci://ghcr.io/kubeflow/charts/kubeflow-trainer --version "${TRAINER_VERSION}" \
    --set 'manager.tolerations[0].operator=Exists' --set 'jobset.controller.tolerations[0].operator=Exists' \
    --wait --timeout 8m
}

phase_stub() {
  preflight docker kind
  require_cluster
  local mode images=()
  for mode in "${stub_modes[@]}"; do
    log "Building nvcre-nccl-stub:${mode}"
    docker build --quiet --build-arg "STUB_MODE=${mode}" -t "nvcre-nccl-stub:${mode}" "${script_dir}/stub" >/dev/null
    images+=("nvcre-nccl-stub:${mode}")
  done
  kind load docker-image "${images[@]}" --name "${cluster}"
}

phase_nvcre() {
  preflight docker kind kubectl helm go make
  require_cluster
  log "Building ${img}"
  make -C "${repo_root}" docker-build IMG="${img}"
  kind load docker-image "${img}" --name "${cluster}"

  log "Installing NVCRE from the local chart"
  h upgrade --install nvcre "${repo_root}/helm/cluster-readiness-engine" \
    --namespace nvcre --create-namespace \
    --values "${repo_root}/test/uat/tilt/nvcre-values.yaml" \
    --set manager.image.repository="${img%:*}" --set manager.image.tag="${img##*:}" \
    --set manager.imagePullSecrets=null --wait --timeout 5m
  # The tag never changes between rebuilds, so Helm sees no change to roll out.
  k -n nvcre rollout restart deployment/nvcre-manager
  k -n nvcre rollout status deployment/nvcre-manager --timeout=3m

  make -C "${repo_root}" build-nvcrectl
}

phase_info() {
  require_cluster
  "${repo_root}/bin/nvcrectl" --context "${ctx}" cluster info
}

phase_down() {
  preflight kind
  if cluster_exists; then
    kind delete cluster --name "${cluster}"
  else
    echo "Kind cluster ${cluster} does not exist."
  fi
}

[[ $# -gt 0 ]] || die "usage: setup.sh <cluster|infra|stub|nvcre|info|up|down>..."
for phase in "$@"; do
  case "${phase}" in
    cluster) phase_cluster ;;
    infra) phase_infra ;;
    stub) phase_stub ;;
    nvcre) phase_nvcre ;;
    info) phase_info ;;
    # Sequential statements, not an && chain: errexit is suspended inside
    # any function that runs as part of an && list.
    up)
      phase_cluster
      phase_infra
      phase_stub
      phase_nvcre
      phase_info
      ;;
    down) phase_down ;;
    *) die "unknown phase ${phase}" ;;
  esac
done
