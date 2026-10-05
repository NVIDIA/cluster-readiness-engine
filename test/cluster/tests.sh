#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Basic tests for a real GPU cluster with NVCRE installed, after NVSentinel's
# tests/uat/tests.sh. Each test creates one Certification or WorkloadRun with
# kubectl, waits for it to succeed, and prints its status:
#
#   test_nccl           Certification nccl-cert: every NCCL category, the two
#                       loopbacks included
#   test_certification  Certification nemo-cert: NeMo training (nemotron5-8b).
#                       Fails for now: training can't tolerate every CSP's taints
#   test_workloadrun    WorkloadRun my-workload, docs/how-to-guides/run-workloadrun.md
#
# The Certifications are the one in docs/getting-started/quick-start.md with
# other categories from docs/concepts/catalog.md, and the WorkloadRun is copied
# from its page as-is. Keep them in step when the docs change.
#
# It needs kubectl and jq, and assumes NVCRE is installed and ready. It works in
# the current kubeconfig context and namespace. The examples run on every
# schedulable GPU node, so use a cluster you can take over. Each test deletes
# what it created.
#
# Environment:
#   UAT_TIMEOUT          seconds each test may take (default 1200, 20 minutes)
#   UAT_CLEANUP_TIMEOUT  seconds to wait for a test's resource to be deleted
#                        (default 600)
#   ARTIFACTS            directory for the JUnit report (optional)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Reports each test on its own, as JUnit XML in $ARTIFACTS. Copied as-is from
# NVSentinel's tests/uat/junit.sh.
# shellcheck source-path=SCRIPTDIR source=junit.sh
source "${SCRIPT_DIR}/junit.sh"

UAT_TIMEOUT=${UAT_TIMEOUT:-1200}
UAT_CLEANUP_TIMEOUT=${UAT_CLEANUP_TIMEOUT:-600}

# Seconds between polls.
POLL_INTERVAL=10

# The resource the running test created, for cleanup_uat to delete.
UAT_CLEANUP_RESOURCE=""

log() {
  echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

error() {
  log "ERROR: $*" >&2
  exit 1
}

# create RESOURCE creates the manifest on stdin, RESOURCE (TYPE/NAME), and
# gives the test UAT_TIMEOUT seconds from now. cleanup_uat deletes only what
# create made, never a resource that already existed.
create() {
  kubectl create -f -
  UAT_CLEANUP_RESOURCE=$1
  DEADLINE=$((SECONDS + UAT_TIMEOUT))
}

cleanup_uat() {
  if [[ -z "${UAT_CLEANUP_RESOURCE}" ]]; then
    return 0
  fi
  log "Deleting ${UAT_CLEANUP_RESOURCE}..."
  # Foreground deletion returns once everything the run made is gone too, so
  # the next test starts on idle GPUs. kubectl's own messages are left out:
  # junit.sh reports the last error in a failed test's output, and that must
  # be the test's.
  if ! kubectl delete "${UAT_CLEANUP_RESOURCE}" --ignore-not-found --cascade=foreground \
    --timeout="${UAT_CLEANUP_TIMEOUT}s" >/dev/null 2>&1; then
    log "WARN: could not delete ${UAT_CLEANUP_RESOURCE} within ${UAT_CLEANUP_TIMEOUT}s"
  fi
}

# wait_for_success RESOURCE waits for its Succeeded or Failed condition to be
# True; the controller keeps them and InProgress mutually exclusive. It prints
# the resource's status, and fails the test if the resource failed or the time
# ran out.
wait_for_success() {
  local resource=$1 status="{}" progress last=""
  log "Waiting for ${resource}..."
  while [[ ${SECONDS} -lt ${DEADLINE} ]]; do
    if ! status=$(kubectl get "${resource}" -o json | jq -c '.status // {}'); then
      log "WARN: could not read ${resource}, retrying"
      sleep "${POLL_INTERVAL}"
      continue
    fi

    if jq -e '(.conditions // []) | any(.[]; .type == "Succeeded" and .status == "True")' <<<"${status}" >/dev/null; then
      log "${resource} succeeded, status:"
      jq . <<<"${status}"
      return 0
    fi
    if jq -e '(.conditions // []) | any(.[]; .type == "Failed" and .status == "True")' <<<"${status}" >/dev/null; then
      log "${resource} status:"
      jq . <<<"${status}"
      error "${resource} failed: $(jq -r '.conditions[] | select(.type == "Failed") | "\(.reason): \(.message)"' <<<"${status}")"
    fi

    # Report each step the controller takes while it runs.
    progress=$(jq -r '(.conditions // [])[] | select(.type == "InProgress" and .status == "True")
      | "\(.reason): \(.message)"' <<<"${status}")
    if [[ -n "${progress}" && "${progress}" != "${last}" ]]; then
      log "  ${progress}"
      last=${progress}
    fi
    sleep "${POLL_INTERVAL}"
  done

  log "${resource} status:"
  jq . <<<"${status}"
  error "Timed out after ${UAT_TIMEOUT}s waiting for ${resource}"
}

test_nccl() {
  log "Test 1: NCCL, every communication category"

  # The Certification in docs/getting-started/quick-start.md, with every
  # communication category in docs/concepts/catalog.md: the three collectives
  # and the two loopbacks. Each runs half the default NCCL iterations and
  # cycles (-n 100 and -N 10).
  create certifications.nvcre.nvidia.com/nccl-cert <<'EOF'
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: nccl-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  numIterations: 2
  numCycles: 2
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: communication
      variant: nccl-all-gather
    - domain: communication
      variant: nccl-alltoall
    - domain: communication
      variant: nccl-loopback
    - domain: communication
      variant: nccl-loopback-nvswitch
EOF

  wait_for_success certifications.nvcre.nvidia.com/nccl-cert
}

# shellcheck disable=SC2317 # The run after the error stays for when it goes.
test_certification() {
  log "Test 2: Certification, NeMo training"

  # NVCRE cannot run this on every CSP yet. The MPI tests above get a blanket
  # toleration, but a training workload only tolerates the taints listed in
  # target.taintSelectors, which also limit it to nodes carrying all of them
  # (ADR-063). GPU node taints differ from one CSP to the next, so no single
  # Certification fits them all. The test fails until training workloads can
  # tolerate them too; then remove the error below.
  error "NeMo training cannot tolerate the GPU node taints of every CSP yet: it only gets the tolerations in target.taintSelectors (ADR-063)"

  # The Certification in docs/getting-started/quick-start.md, with only its NeMo
  # training category.
  create certifications.nvcre.nvidia.com/nemo-cert <<'EOF'
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: nemo-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  categories:
    - domain: training
      variant: nemotron5-8b
EOF

  wait_for_success certifications.nvcre.nvidia.com/nemo-cert
}

test_workloadrun() {
  log "Test 3: WorkloadRun, from docs/how-to-guides/run-workloadrun.md"

  # Copied as-is from docs/how-to-guides/run-workloadrun.md, "Basic example".
  create workloadruns.nvcre.nvidia.com/my-workload <<'EOF'
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: my-workload
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      mpirunPath: /usr/local/mpi/bin/mpirun
      binary: /usr/local/bin/all_reduce_perf_mpi
  numNodes: 2
EOF

  wait_for_success workloadruns.nvcre.nvidia.com/my-workload
}

main() {
  log "Starting NVCRE cluster tests on $(kubectl config current-context)..."

  # Each test runs on its own: a test that fails doesn't stop the ones after
  # it, and cleanup_uat cleans up after each test. junit_finish fails the
  # script if any test failed.
  JUNIT_SUITE=nvcre-cluster
  JUNIT_CLEANUP=cleanup_uat

  junit_test test_nccl
  junit_test test_certification
  junit_test test_workloadrun

  log "========================================="
  junit_finish
}

main "$@"
