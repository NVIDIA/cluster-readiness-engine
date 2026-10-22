---
title: nvcrectl rbac
description: Print the RoleBinding that grants the controller write access in a run namespace.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl rbac print

Writes the RoleBinding that grants the controller write access in one run namespace, for applying with `kubectl` or committing next to a Certification in GitOps.

```bash
nvcrectl rbac print --namespace <run-namespace> [flags]
```

The binding is needed only when the chart is installed with `rbac.scope: namespaced` ([ADR-092](../designs/092-per-namespace-controller-rbac.md)). Without it, a Workflow in that namespace waits with reason `NamespacePermissionsMissing` and resumes on its own once the binding exists, or fails if `manager.namespaceAccessTimeout` (default 10 minutes) runs out first. `nvcrectl certification run` and `nvcrectl workloadrun run` create the binding themselves in namespaces they create; this command is for runs that do not go through `nvcrectl`, and for namespaces that already exist. See [RBAC requirements](../operations/deployment.md#rbac-requirements).

Nothing is read from the cluster. The manifest is built by the same code `nvcrectl` uses when it creates the binding, so the role and binding names cannot drift between the two.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` / `-n` | — | Run namespace to grant (required) |
| `--controller-namespace` | `nvcre` | Namespace the controller is installed in |
| `--service-account` | `nvcre-manager` | Controller ServiceAccount name |

The defaults match `nvcrectl setup init`, which installs Helm release `nvcre` in namespace `nvcre`. A release with another name or namespace has another ServiceAccount: copy it from the subject of `kubectl get clusterrolebinding nvcre-manager-rolebinding -o yaml`, or from the message of a waiting Workflow, which names it.

### Examples

```bash
# Grant the controller access in team-a
nvcrectl rbac print --namespace team-a | kubectl apply -f -

# A release named "readiness" installed in namespace "platform"
nvcrectl rbac print -n team-a --controller-namespace platform --service-account readiness-nvcre-manager
```

Output:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: nvcre-manager-run
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: nvcre-run-role
subjects:
- kind: ServiceAccount
  name: nvcre-manager
  namespace: nvcre
```

Delete the binding only after the Certification or WorkloadRun that needed it is fully deleted: the controller's finalizers write in the namespace until then.
