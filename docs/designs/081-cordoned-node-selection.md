# ADR-081: Support for Cordoned Node Selection

**Status:** Proposed

**Date:** 2026-09-15

## Context

Currently, there is no mechanism which supports running a Certification or Workflow which
targets a cordoned GPU node. Specifically, the workflow-controller will ignore any cordoned
node in discoverTargetNodes even if the node satisfies all other selection requirements from
the specified nodeSelector, matchExpressions, nodeNames, and taintSelectors. If this restriction
was not in place, cordoned nodes could already be targeted with a taintSelectors entry against the
node.kubernetes.io/unschedulable taint, since cordoning a node adds this taint directly. However,
if the given Workflow does not have a custom nodeHealthMonitor CEL expression, the
workflow-controller will apply a default expression which results in a HardwareFailure for any
cordoned node. Note that there is not a mechanism to provide a custom nodeHealthMonitor expression
at the Certification layer.

To prevent competing workloads from running on nodes undergoing a Certification, a cluster operator
may prefer using a cordon over a direct taint for the following 2 reasons:

1. DaemonSets automatically tolerate the unschedulable taint: running a Certification requires
a node is ready to run the given communication or training workload triggered by NVCRE.
In most cases, this requires that core system add-ons (such as kube-proxy or the CNI plugin)
and GPU Operator operands, both of which typically run as DaemonSets, are fully deployed.
Applying an explicit taint and leveraging an NVCRE taintSelector requires that all of these
operands either tolerate all taints or explicitly tolerate the given taint. Without this
toleration, there is a risk that the given DaemonSet cannot re-schedule to the node if it
is recreated. Using a node cordon removes this requirement because all DaemonSets
automatically tolerate the unschedulable taint.
2. NVCRE clients may use a cordon as a scheduling gate: a client that uses NVCRE to validate
new nodes or nodes returning from remediation may choose to use a cordon to keep nodes isolated
during new node bootstrapping or during remediation workflows. To prevent these clients from
needing to swap an existing cordon with a taint to keep nodes isolated during the validation
phase, it would be beneficial to support direct targeting of cordoned nodes. Note that
NVSentinel primarily uses node cordons as an isolation mechanism, so a direct integration
path between NVCRE and NVSentinel requires support for selecting cordoned nodes.

## Decision

Allow an operator to target a cordoned node by adding the node.kubernetes.io/unschedulable
taint to a Certification's taintSelectors, without any changes to the Certification or
Workflow API.

1. Node discovery: discoverTargetNodes will not exclude cordoned nodes that matched a
taintSelectors entry for the node.kubernetes.io/unschedulable taint. If this taint is not
present in taintSelectors, the existing behavior will apply which filters out cordoned
nodes from the set of selected nodes.
2. Pod tolerations: no changes are required for how tolerations are applied to workload pod
templates. Any taint specified in taintSelectors is automatically tolerated by the workload
pods via adapter.SetTolerations.
3. Health monitor: if taintSelectors specifies the node.kubernetes.io/unschedulable
taint, the cordon health check will be dropped for the run. This means clearing
nodeHealthMonitor on the created Job even when the catalog jobTemplate sets it, not
only skipping the nil default, since every catalog entry sets the
node.spec.unschedulable == true expression explicitly. If this taint is not present in
taintSelectors, the default monitor CEL expression will continue to be applied.

Example Certification which targets a cordoned node:
```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-node-1-certification
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB200
    nodeNames:
      - gpu-node-1
    taintSelectors:
      - key: node.kubernetes.io/unschedulable
        effect: NoSchedule
  nodesPerJob: 1
  enableMNNVL: true
  categories:
    - domain: communication
      variant: nccl-loopback
```

## Consequences

1. taintSelectors requires every entry to match, so adding node.kubernetes.io/unschedulable
excludes every uncordoned node too. A single Certification cannot target both cordoned and
uncordoned nodes at once.
2. If taintSelectors specifies node.kubernetes.io/unschedulable, nodeHealthMonitor is cleared
entirely, regardless of whether it came from the catalog, the Workflow, or the controller's
default. No node in the run can be marked HardwareFailureDetected.
3. Setting a taintSelectors entry for node.kubernetes.io/unschedulable drops the blanket taint
toleration workload pods get by default. This is not unique to node.kubernetes.io/unschedulable.
Selection only requires the listed taints to be present, so a cordoned node carrying an
additional, unrelated taint can be selected and then fail to schedule.
4. A node cordoned but not yet carrying the node.kubernetes.io/unschedulable taint would be
excluded from the Certification. It is the client's responsibility to ensure every taint
listed in taintSelectors is already applied on the node before requesting a Certification.

## Alternatives Considered

1. Add a new allowCordonedNodes field: rather than detect that cordoned nodes should be
allowed for selection through the existing taintSelectors field, an alternative implementation
would be to add a new allowCordonedNodes boolean to the Certification API. This field
would need to be referenced to implement the 3 behaviors described above for node discovery,
pod tolerations, and health monitor defaults. With the recommended implementation, no API
changes are required and operators only need to reason about which taints are applied
to their target nodes.
2. Allow overriding the health monitor at the Certification layer: regardless of whether we
use the taintSelectors or allowCordonedNodes approach, we could expose the ability to provide
a custom health monitor CEL expression directly on the Certification API, rather than only
skipping the default. We will punt on this since operators don't need it just to target a
cordoned node.
