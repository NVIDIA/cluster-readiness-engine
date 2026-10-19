// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	// MultusNetworksAnnotation is the pod annotation Multus reads to attach
	// secondary networks, in order.
	MultusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

	keyAnnotations = "annotations"
	keyResources   = "resources"
	keyLimits      = "limits"
	keyRequests    = "requests"

	// workerContainerName is the workload container of a node replicatedJob,
	// the same container the catalog's NIC fragments patch.
	workerContainerName = "node"

	maxNetworkNameLen = 253
)

var (
	// networkPattern and resourceNamePattern mirror the kubebuilder Pattern
	// markers on NetworkAttachment. They are duplicated here because offline
	// nvcrectl render never reaches an API server.
	networkPattern      = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	resourceNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[a-zA-Z0-9]([-A-Za-z0-9_.]{0,61}[a-zA-Z0-9])?$`)
)

// ValidateNetworkAttachments checks a networkAttachments list against the
// same rules the CRD enforces per entry and on the list length. The
// cross-field exclusions live with the owning spec, since they read sibling
// fields.
func ValidateNetworkAttachments(atts []nvcrev1alpha1.NetworkAttachment) error {
	if len(atts) > nvcrev1alpha1.MaxNetworkAttachments {
		return fmt.Errorf("networkAttachments has %d entries; at most %d are allowed",
			len(atts), nvcrev1alpha1.MaxNetworkAttachments)
	}
	for i, a := range atts {
		if !networkPattern.MatchString(a.Network) {
			return fmt.Errorf("networkAttachments[%d].network %q must be <name> or <namespace>/<name>"+
				" with a DNS-1123 namespace and name", i, a.Network)
		}
		name := a.Network[strings.LastIndex(a.Network, "/")+1:]
		if len(name) > maxNetworkNameLen {
			return fmt.Errorf("networkAttachments[%d].network name must be at most %d characters", i, maxNetworkNameLen)
		}
		if len(a.ResourceName) > maxNetworkNameLen || !resourceNamePattern.MatchString(a.ResourceName) {
			return fmt.Errorf("networkAttachments[%d].resourceName %q must be a fully qualified extended"+
				" resource name (<domain>/<name>)", i, a.ResourceName)
		}
		if strings.Contains(a.ResourceName, "kubernetes.io/") || strings.HasPrefix(a.ResourceName, "k8s.io/") ||
			strings.Contains(a.ResourceName, ".k8s.io/") {
			return fmt.Errorf("networkAttachments[%d].resourceName %q must not use the reserved"+
				" kubernetes.io or k8s.io domains", i, a.ResourceName)
		}
	}
	return nil
}

// NetworkAttachmentsAnnotationValue renders the Multus networks annotation
// value: the networks in list order, comma separated.
func NetworkAttachmentsAnnotationValue(atts []nvcrev1alpha1.NetworkAttachment) string {
	names := make([]string, 0, len(atts))
	for _, a := range atts {
		names = append(names, a.Network)
	}
	return strings.Join(names, ",")
}

// networkAttachmentResources counts the attachments per resourceName. Each
// attachment is one interface and needs one device, so two attachments backed
// by the same resource request two.
func networkAttachmentResources(atts []nvcrev1alpha1.NetworkAttachment) map[string]int64 {
	out := make(map[string]int64, len(atts))
	for _, a := range atts {
		out[a.ResourceName]++
	}
	return out
}

// ApplyNetworkAttachmentsToDependencies wires the attachments into every
// TrainingRuntime dependency: each replicatedJob other than the MPI launcher
// gets the Multus networks annotation on its pod template, replacing any
// value the catalog rendered, and its worker container (the one named
// "node", else the first) gets limits and requests for each resourceName.
// It is a no-op for an empty list, and a dependency it does not change comes
// back with the exact bytes it went in with.
func ApplyNetworkAttachmentsToDependencies(
	deps []nvcrev1alpha1.DependencySpec, atts []nvcrev1alpha1.NetworkAttachment,
) error {
	if len(atts) == 0 {
		return nil
	}
	annotation := NetworkAttachmentsAnnotationValue(atts)
	resources := networkAttachmentResources(atts)

	for i := range deps {
		if len(deps[i].Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(deps[i].Raw, &obj); err != nil {
			return fmt.Errorf("unmarshal dependency %d: %w", i, err)
		}
		if kind, _ := obj[keyKind].(string); kind != kindTrainingRuntime {
			continue
		}
		replicatedJobs, ok := nestedSlice(obj, keySpec, keyTemplate, keySpec, keyReplicatedJobs)
		if !ok {
			continue
		}
		changed := false
		for _, rj := range replicatedJobs {
			job, isMap := rj.(map[string]any)
			if !isMap {
				continue
			}
			// The MPI launcher runs mpirun, not ranks, so it gets no attachment.
			if name, _ := job[keyName].(string); name == launcherJobName {
				continue
			}
			if applyNetworkAttachmentsToJob(job, annotation, resources) {
				changed = true
			}
		}
		if !changed {
			continue
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshal dependency %d: %w", i, err)
		}
		deps[i].Raw = raw
	}
	return nil
}

// applyNetworkAttachmentsToJob patches one replicatedJob's pod template.
// replicatedJobs[].template.spec.template is JobTemplateSpec -> JobSpec ->
// PodTemplateSpec.
func applyNetworkAttachmentsToJob(job map[string]any, annotation string, resources map[string]int64) bool {
	podTemplate, ok := nestedMap(job, keyTemplate, keySpec, keyTemplate)
	if !ok {
		return false
	}
	containers, found := nestedSlice(podTemplate, keySpec, keyContainers)
	if !found || len(containers) == 0 {
		return false
	}
	worker := workerContainer(containers)
	if worker == nil {
		return false
	}

	metadata := ensureMap(podTemplate, keyMetadata)
	annotations := ensureMap(metadata, keyAnnotations)
	annotations[MultusNetworksAnnotation] = annotation

	res := ensureMap(worker, keyResources)
	limits := ensureMap(res, keyLimits)
	requests := ensureMap(res, keyRequests)
	for name, qty := range resources {
		v := strconv.FormatInt(qty, 10)
		limits[name] = v
		requests[name] = v
	}
	return true
}

// workerContainer returns the container named "node", or the first container
// when none carries that name.
func workerContainer(containers []any) map[string]any {
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m[keyName].(string); name == workerContainerName {
			return m
		}
	}
	m, _ := containers[0].(map[string]any)
	return m
}
