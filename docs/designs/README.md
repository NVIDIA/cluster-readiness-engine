# Architecture Decision Records

Each record states one decision: the context that forced it, what was decided, and what
the decision costs. They are written before the code, not after it, and they are the
fastest way to learn why this repository is shaped the way it is.

Read the relevant record before you change the behaviour it describes. `CLAUDE.md` and
`AGENTS.md` both point here for that reason.

## Conventions

- The number below is the one the record gives itself in its own heading.
- The rows are in file-name order, which is the order the directory lists them in.
- New records follow the structure in `CLAUDE.md`: Context, Decision, Implementation,
  Rationale, Consequences, Alternatives Considered, Notes, References.

### Numbers that differ from the file name

Three records give themselves a number that their file name does not match. Search by
either and use this table to find the other.

| Record calls itself | File |
|---|---|
| ADR-0022 | [`000-adr.md`](000-adr.md) |
| ADR-026 | [`027-kustomize-override-ux.md`](027-kustomize-override-ux.md) |
| ADR-027 | [`031-platform-aware-nccl-config.md`](031-platform-aware-nccl-config.md) |

`031-platform-aware-nccl-config.md` calls itself ADR-027, and `027-kustomize-override-ux.md`
calls itself ADR-026, so the name ADR-027 reaches a different record depending on whether
you follow the heading or the file name. `032-orchestration-overrides.md` cites the override
record as ADR-027, meaning the file name; `031-platform-aware-nccl-config.md` cites it as
ADR-026, meaning the heading. Both point at `027-kustomize-override-ux.md`.

### Unused numbers

No record claims 000, 006, 011, 020, 028, 029, 030, 031, 033, 036, 037 or 040. Note that
`README.md` and `AGENTS.md` describe the set as "ADR-000 to ADR-069" and name
`000-adr.md` as the architecture record, which reads by file name rather than by heading.

## Index

| ADR | Title |
|---|---|
| 0022 ⁽ᵈ⁾ | [CRE Architecture for GPU Cluster Certification](000-adr.md) |
| 001 | [Architecture — CRE for GPU Cluster Certification](001-adr-abridged.md) |
| 002 | [Architecture — Layered CRD Hierarchy](002-layered-crd-hierarchy.md) |
| 003 | [Architecture — Strongly-Typed Workload Adapter Pattern](003-workload-adapter-pattern.md) |
| 004 | [Feature — CEL-Based Node Health Monitoring](004-cel-node-health-monitoring.md) |
| 005 | [Feature — LogProfile-Driven Goodput Measurement](005-logprofile-goodput-measurement.md) |
| 007 | [Feature — Topology-Aware Multi-Group Orchestration](007-topology-aware-orchestration.md) |
| 008 | [Feature — Checkpoint-Based Restart and State Recovery](008-checkpoint-restart.md) |
| 009 | [Feature — Adaptive Stall Detection](009-adaptive-stall-detection.md) |
| 010 | [Architecture — Certification Catalog with init() Registration](010-certification-catalog.md) |
| 012 | [Feature — Platform and GPU Architecture Overrides](012-platform-gpu-overrides.md) |
| 013 | [Architecture — Prometheus Metrics and Observability](013-prometheus-observability.md) |
| 014 | [Testing — envtest Integration Tests with Golden Files](014-integration-test-strategy.md) |
| 015 | [Feature — Auto-Created GoodputMeasurement from Job Spec](015-auto-created-goodput-measurement.md) |
| 016 | [NCCL All-Reduce Certification Catalog Entry](016-nccl-all-reduce-catalog.md) |
| 017 | [NCCL Bandwidth Measurement](017-nccl-bandwidth-measurement.md) |
| 018 | [NCCL Test Suite Catalog Entries](018-nccl-test-suite-catalog.md) |
| 019 | [Sequential Workflow Execution in Certification Controller](019-sequential-workflow-execution.md) |
| 021 | [Performance Threshold Enforcement (Pass/Fail Criteria)](021-performance-threshold-enforcement.md) |
| 023 | [Catalog Configurability — Remove Hardcoded Values, Add Certification-Level Config](023-catalog-configurability.md) |
| 024 | [YAML-Embedded Catalog — Replace Go Struct Literals with Embedded YAML Files](024-yaml-embedded-catalog.md) |
| 025 | [YAML Template Catalog — Replace Post-Parse Injection with Go Templates + Sprig](025-yaml-template-catalog.md) |
| 026 ⁽ᵈ⁾ | [Kustomize-like Override UX](027-kustomize-override-ux.md) |
| 027 ⁽ᵈ⁾ | [Platform-Aware NCCL Communication Benchmark Configuration](031-platform-aware-nccl-config.md) |
| 032 | [Orchestration Overrides](032-orchestration-overrides.md) |
| 034 | [Eliminate LifecycleSpec — Infer Dependency Scope and Ordering from References](034-inferred-dependency-lifecycle.md) |
| 035 | [Optional Legacy Kubeflow Training Operator Support](035-optional-legacy-kubeflow.md) |
| 038 | [Shell Installer Script for ncrectl](038-installer-script.md) |
| 039 | [CLI Self-Upgrade Command](039-cli-self-upgrade.md) |
| 041 | [kubeadm-Style Init/Reset with Phases](041-kubeadm-style-init-reset.md) |
| 042 | [CLI Command for Running Certifications](042-certification-run.md) |
| 043 | [Per-Category nodesPerJob with Auto-Selection and Early Overlay Resolution](043-per-category-nodes-per-job.md) |
| 044 | [Full Certification Lifecycle in ncrectl](044-xcalctl-certification-lifecycle.md) |
| 045 | [Embedded Config and Go Client Apply in ncrectl](045-xcalctl-embedded-config.md) |
| 046 | [Shared Template Library for Catalog Entries](046-shared-template-library.md) |
| 047 | [Standardize NCCL Communication Entries on AWS EFA Configuration](047-standardize-nccl-aws.md) |
| 048 | [Embedded Trainer Manifests](048-embedded-trainer-manifests.md) |
| 049 | [Kind + KWOK End-to-End UAT Tests with e2e-framework](049-kind-kwok-uat-tests.md) |
| 050 | [Unified ncrectl certification run Pipeline](050-xcalctl-unified-run-pipeline.md) |
| 051 | [Tolerate All Taints and Avoid GPU Nodes for Controllers](051-tolerate-all-taints.md) |
| 052 | [Forced CLI Upgrade Check for Release Builds](052-forced-cli-upgrade.md) |
| 053 | [Ordered Dependency Deletion via Reverse Topological Sort](053-ordered-dependency-deletion.md) |
| 054 | [Multi-Scale NCCL Cluster Validation](054-multi-scale-nccl-validation.md) |
| 055 | [Adaptive Fault Isolation for NCCL Diagnostics](055-adaptive-fault-isolation.md) |
| 056 | [Cross-Boundary Probing for Infrastructure Fault Detection](056-cross-boundary-probing.md) |
| 057 | [DCGM Level-3 Diagnostics — A100 Configuration](057-dcgm-level3-a100.md) |
| 058 | [Mistral GB300 SKU Support (InfiniBand)](058-mistral-gb300-ib-support.md) |
| 059 | [WorkloadRun — Simplified Workload Execution API](059-workloadrun-simplified-api.md) |
| 060 | [Azure H100 Multi-Node NCCL Support](060-azure-h100-nccl-support.md) |
| 061 | [Remove Remediation Controller — Failed Node Attribution via Certification CR](061-excalibur-nvsentinel-remediation-decoupling.md) |
| 062 | [Succeeded Node Attribution via a Compressed ConfigMap](062-node-detail-propagation.md) |
| 063 | [Auto-Inject Tolerations from `target.taintSelectors`](063-taint-selector-tolerations.md) |
| 064 | [Helm Chart Distribution](064-helm-chart-distribution.md) |
| 065 | [ncrectl Helm Install](065-xcalctl-helm-install.md) |
| 066 | [Remove the `kubeJob` Workload Type](066-remove-kubejob-workload-type.md) |
| 067 | [`kubectl ncrectl` Plugin Support and Full kubectl Flag Parity](067-kubectl-plugin-support.md) |
| 068 | [Offloading Inline Node Lists from the Workflow CR via Compressed ConfigMaps](068-group-nodes-compressed-configmap.md) |
| 069 | [cmd/ layout — kubernetes/kubernetes convention](069-cmd-layout.md) |

⁽ᵈ⁾ the file name carries a different number; see above.
