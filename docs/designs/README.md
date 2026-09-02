# Architecture Decision Records

Each record states one decision: the context that forced it, what was decided, and what
the decision costs. They are written before the code, not after it, and they are the
fastest way to learn why this repository is shaped the way it is.

Read the relevant record before you change the behaviour it describes. `CLAUDE.md` and
`AGENTS.md` both point here for that reason.

## Conventions

- The file name carries the number, and each record's heading matches it.
- ADR-000 is the full architecture record. ADR-001 is an abridged version of the same
  decision, kept for readers who want the short form.
- New records follow the structure in `CLAUDE.md`: Context, Decision, Implementation,
  Rationale, Consequences, Alternatives Considered, Notes, References.

## Index

| ADR | Title |
|---|---|
| 000 | [NVCRE Architecture for GPU Cluster Certification](000-adr.md) |
| 001 | [Architecture — NVCRE for GPU Cluster Certification](001-adr-abridged.md) |
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
| 027 | [Kustomize-like Override UX](027-kustomize-override-ux.md) |
| 031 | [Platform-Aware NCCL Communication Benchmark Configuration](031-platform-aware-nccl-config.md) |
| 032 | [Orchestration Overrides](032-orchestration-overrides.md) |
| 034 | [Eliminate LifecycleSpec — Infer Dependency Scope and Ordering from References](034-inferred-dependency-lifecycle.md) |
| 035 | [Optional Legacy Kubeflow Training Operator Support](035-optional-legacy-kubeflow.md) |
| 038 | [Shell Installer Script for nvcrectl](038-installer-script.md) |
| 039 | [CLI Self-Upgrade Command](039-cli-self-upgrade.md) |
| 041 | [kubeadm-Style Init/Reset with Phases](041-kubeadm-style-init-reset.md) |
| 042 | [CLI Command for Running Certifications](042-certification-run.md) |
| 043 | [Per-Category nodesPerJob with Auto-Selection and Early Overlay Resolution](043-per-category-nodes-per-job.md) |
| 044 | [Full Certification Lifecycle in nvcrectl](044-nvcrectl-certification-lifecycle.md) |
| 045 | [Embedded Config and Go Client Apply in nvcrectl](045-nvcrectl-embedded-config.md) |
| 046 | [Shared Template Library for Catalog Entries](046-shared-template-library.md) |
| 047 | [Standardize NCCL Communication Entries on AWS EFA Configuration](047-standardize-nccl-aws.md) |
| 048 | [Embedded Trainer Manifests](048-embedded-trainer-manifests.md) |
| 049 | [Kind + KWOK End-to-End UAT Tests with e2e-framework](049-kind-kwok-uat-tests.md) |
| 050 | [Unified nvcrectl certification run Pipeline](050-nvcrectl-unified-run-pipeline.md) |
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
| 061 | [Remove Remediation Controller — Failed Node Attribution via Certification CR](061-nvcre-nvsentinel-remediation-decoupling.md) |
| 062 | [Succeeded Node Attribution via a Compressed ConfigMap](062-node-detail-propagation.md) |
| 063 | [Auto-Inject Tolerations from `target.taintSelectors`](063-taint-selector-tolerations.md) |
| 064 | [Helm Chart Distribution](064-helm-chart-distribution.md) |
| 065 | [nvcrectl Helm Install](065-nvcrectl-helm-install.md) |
| 066 | [Remove the `kubeJob` Workload Type](066-remove-kubejob-workload-type.md) |
| 067 | [`kubectl nvcrectl` Plugin Support and Full kubectl Flag Parity](067-kubectl-plugin-support.md) |
| 068 | [Offloading Inline Node Lists from the Workflow CR via Compressed ConfigMaps](068-group-nodes-compressed-configmap.md) |
| 069 | [cmd/ layout — kubernetes/kubernetes convention](069-cmd-layout.md) |
| 070 | [WorkloadRun MPI Transport-Layer Overrides for AWS GB300 (RoCE)](070-workloadrun-gb300-mpi-transport.md) |
| 071 | [Threshold Violation Reason Propagation into Report Surfaces](071-threshold-reason-propagation.md) |
| 072 | [Freeze GoodputMeasurement Status at Job Terminal State](072-goodput-terminal-freeze.md) |
| 073 | [Convergent `setup init` Retry After a Partial Kubeflow Trainer Install](073-setup-retry-convergence.md) |
| 074 | [Supply Chain Artifact and Verification Contract](074-supply-chain-attestation.md) |
