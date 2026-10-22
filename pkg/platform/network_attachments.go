// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

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
// Those add to a quantity the catalog already rendered for the same resource
// (for example nvidia.com/mlnxnics on Azure or TogetherAI), so the list adds
// interfaces rather than shrinking the platform request. On OCI the default
// mlnxnics wiring is not rendered when a list is set, so there the list's
// count is the whole request. It is a no-op for an empty list, and a dependency it does not change comes
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
		dep := &deps[i]
		if len(dep.Raw) == 0 {
			continue
		}
		obj := map[string]any{}
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
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
			jobChanged, err := applyNetworkAttachmentsToJob(job, annotation, resources)
			if err != nil {
				return fmt.Errorf("dependency %d: %w", i, err)
			}
			if jobChanged {
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
		dep.Raw = raw
	}
	return nil
}

// applyNetworkAttachmentsToJob patches one replicatedJob's pod template.
// replicatedJobs[].template.spec.template is JobTemplateSpec -> JobSpec ->
// PodTemplateSpec.
func applyNetworkAttachmentsToJob(job map[string]any, annotation string, resources map[string]int64) (bool, error) {
	podTemplate, ok := nestedMap(job, keyTemplate, keySpec, keyTemplate)
	if !ok {
		return false, nil
	}
	containers, found := nestedSlice(podTemplate, keySpec, keyContainers)
	if !found || len(containers) == 0 {
		return false, nil
	}
	worker := workerContainer(containers)
	if worker == nil {
		return false, nil
	}

	res := ensureMap(worker, keyResources)
	limits := ensureMap(res, keyLimits)
	requests := ensureMap(res, keyRequests)
	// Compute every sum before writing any limit or request. An error can still
	// leave this decoded job partly changed (the resources maps above may have
	// been created), but the caller then returns without re-marshalling, so the
	// dependency's Raw bytes are left as they were.
	sums := make(map[string]string, len(resources))
	for name, qty := range resources {
		// An extended resource's request must equal its limit, and an omitted
		// request defaults to the limit, so one base serves both: the limit
		// when set, else the request.
		base, present := limits[name]
		if !present {
			base, present = requests[name]
		}
		sum, err := addResourceQuantity(base, present, qty)
		if err != nil {
			return false, fmt.Errorf("resource %s: %w", name, err)
		}
		sums[name] = sum
	}

	metadata := ensureMap(podTemplate, keyMetadata)
	annotations := ensureMap(metadata, keyAnnotations)
	annotations[MultusNetworksAnnotation] = annotation
	for name, sum := range sums {
		limits[name] = sum
		requests[name] = sum
	}
	return true, nil
}

// addResourceQuantity adds qty devices to a quantity already rendered in a
// limits or requests map, which JSON decoding leaves as a string ("8") or a
// number (8). With nothing rendered the result is qty itself.
func addResourceQuantity(existing any, present bool, qty int64) (string, error) {
	if !present || existing == nil {
		return strconv.FormatInt(qty, 10), nil
	}
	var q resource.Quantity
	switch v := existing.(type) {
	case string:
		parsed, err := resource.ParseQuantity(v)
		if err != nil {
			return "", fmt.Errorf("existing quantity %q: %w", v, err)
		}
		q = parsed
	case float64:
		if v != float64(int64(v)) {
			return "", fmt.Errorf("existing quantity %v is not a whole number", v)
		}
		q = *resource.NewQuantity(int64(v), resource.DecimalSI)
	case json.Number:
		parsed, err := resource.ParseQuantity(v.String())
		if err != nil {
			return "", fmt.Errorf("existing quantity %q: %w", v, err)
		}
		q = parsed
	default:
		return "", fmt.Errorf("existing quantity has unsupported type %T", existing)
	}
	q.Add(*resource.NewQuantity(qty, resource.DecimalSI))
	return q.String(), nil
}

// workerContainer returns the container named "node", or the first container
// when none carries that name. Both the runtime path and the override path pick
// the worker this way, so an override raises the same container the runtime
// wrote to. It returns nil for an empty list.
func workerContainer(containers []any) map[string]any {
	if len(containers) == 0 {
		return nil
	}
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

// ApplyNetworkAttachmentsToOverrides carries the attachments into the
// TrainingRuntime patches of WorkloadRun platform overrides. Those overrides
// are applied after the runtime is built (by the Workflow controller, or by
// nvcrectl render once it knows the platform), and an override merges a
// scalar quantity by replacing it. Without this, an override that requests a
// resource an attachment also names, such as nvidia.com/mlnxnics on Azure,
// would replace the summed request on the runtime with its own count, and the
// pod would ask for fewer devices than it has interfaces.
//
// So on the worker container (the one named "node", else the first, as on
// the runtime path) of every replicatedJob other than the MPI launcher, each
// attachment resource the patch already requests is raised by the
// attachments' count, in both limits and requests. A patch that
// does not request the resource is left alone, so it cannot shrink the request
// the runtime carries, and a networks annotation a patch renders is replaced
// with the list's. The user's own spec.overrides are not passed here and stay
// the last word, as on every other WorkloadRun field. It is a no-op for an
// empty list.
func ApplyNetworkAttachmentsToOverrides(
	overrides []WorkloadRunOverride, atts []nvcrev1alpha1.NetworkAttachment,
) error {
	if len(atts) == 0 {
		return nil
	}
	annotation := NetworkAttachmentsAnnotationValue(atts)
	resources := networkAttachmentResources(atts)

	for oi := range overrides {
		for di := range overrides[oi].Dependencies {
			dep := &overrides[oi].Dependencies[di]
			if len(dep.Raw) == 0 {
				continue
			}
			obj := map[string]any{}
			if err := json.Unmarshal(dep.Raw, &obj); err != nil {
				return fmt.Errorf("unmarshal override %d dependency %d: %w", oi, di, err)
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
				if name, _ := job[keyName].(string); name == launcherJobName {
					continue
				}
				jobChanged, err := applyNetworkAttachmentsToPatchJob(job, annotation, resources)
				if err != nil {
					return fmt.Errorf("override %d dependency %d: %w", oi, di, err)
				}
				if jobChanged {
					changed = true
				}
			}
			if !changed {
				continue
			}
			raw, err := json.Marshal(obj)
			if err != nil {
				return fmt.Errorf("marshal override %d dependency %d: %w", oi, di, err)
			}
			dep.Raw = raw
		}
	}
	return nil
}

// applyNetworkAttachmentsToPatchJob raises, on one replicatedJob of an
// override patch, every attachment resource the worker container (named
// "node", else the first) already requests, and replaces a networks annotation the patch renders.
func applyNetworkAttachmentsToPatchJob(job map[string]any, annotation string, resources map[string]int64) (bool, error) {
	podTemplate, ok := nestedMap(job, keyTemplate, keySpec, keyTemplate)
	if !ok {
		return false, nil
	}
	changed := false
	if annotations, found := nestedMap(podTemplate, keyMetadata, keyAnnotations); found {
		if _, set := annotations[MultusNetworksAnnotation]; set {
			annotations[MultusNetworksAnnotation] = annotation
			changed = true
		}
	}

	containers, _ := nestedSlice(podTemplate, keySpec, keyContainers)
	worker := workerContainer(containers)
	if worker == nil {
		return changed, nil
	}
	res, found := worker[keyResources].(map[string]any)
	if !found {
		return changed, nil
	}
	limits, _ := res[keyLimits].(map[string]any)
	requests, _ := res[keyRequests].(map[string]any)
	// Compute every sum before writing any limit or request. An error can still
	// leave this decoded job partly changed (the annotation above may already be
	// replaced), but the caller then returns without re-marshalling, so the
	// override's Raw bytes are left as they were.
	sums := make(map[string]string, len(resources))
	for name, qty := range resources {
		base, present := limits[name]
		if !present {
			base, present = requests[name]
		}
		if !present {
			continue
		}
		sum, err := addResourceQuantity(base, present, qty)
		if err != nil {
			return false, fmt.Errorf("resource %s: %w", name, err)
		}
		sums[name] = sum
	}
	if len(sums) == 0 {
		return changed, nil
	}
	limits = ensureMap(res, keyLimits)
	requests = ensureMap(res, keyRequests)
	for name, sum := range sums {
		limits[name] = sum
		requests[name] = sum
	}
	return true, nil
}
