// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	// LabelKeyManagedBy marks the generated workload as NVCRE-managed and
	// ReservedLabelPrefix covers the keys that associate it with its Job.
	// The Job controller owns both; a user workload label may not set them.
	LabelKeyManagedBy   = "app.kubernetes.io/managed-by"
	ReservedLabelPrefix = "nvcre.nvidia.com/"

	// MaxLabels bounds a single workloadMetadata.labels map. It mirrors the
	// maxProperties in the CRD schema so a map composed from several levels,
	// which no single admission rule ever sees whole, is held to the same
	// limit.
	MaxLabels = 32
)

// ReservedLabelKey reports whether key is one the controller owns on the
// generated workload object.
func ReservedLabelKey(key string) bool {
	return key == LabelKeyManagedBy || strings.HasPrefix(key, ReservedLabelPrefix)
}

// ValidateLabels mirrors the CRD schema's admission rules for a
// workloadMetadata.labels map. Admission already covers every value that
// arrives through the API, but two cases need this: offline `nvcrectl render`
// never reaches an API server, and a map composed from global, per-category
// and derived queue labels is not the map any single object was admitted
// with. fieldPath names the offending field in the error.
//
// Keys are visited in sorted order so a map with several problems always
// reports the same one.
func ValidateLabels(labels map[string]string, fieldPath string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%s: %d labels exceeds the maximum of %d",
			fieldPath, len(labels), MaxLabels)
	}
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		if ReservedLabelKey(key) {
			return fmt.Errorf(
				"%s[%q]: label key is reserved for the controller: %q and any key under %q identify workload ownership",
				fieldPath, key, LabelKeyManagedBy, ReservedLabelPrefix)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("%s[%q]: invalid label key: %s",
				fieldPath, key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(labels[key]); len(errs) > 0 {
			return fmt.Errorf("%s[%q]: invalid label value %q: %s",
				fieldPath, key, labels[key], strings.Join(errs, "; "))
		}
	}
	return nil
}

// LabelsOf returns md's labels as a plain string map. The result is always a
// fresh copy, so a caller composing labels cannot write back into the API
// object it read them from. Absent metadata and an empty map both yield nil,
// which keeps an unconfigured path rendering exactly as it did before.
func LabelsOf(md *nvcrev1alpha1.WorkloadMetadata) map[string]string {
	if md == nil || len(md.Labels) == 0 {
		return nil
	}
	labels := make(map[string]string, len(md.Labels))
	for key, value := range md.Labels {
		labels[key] = string(value)
	}
	return labels
}

// MetadataFrom wraps a label map as workload metadata, returning nil when
// there is nothing to set so a path that resolved no labels stays absent
// rather than gaining an empty object. The map is copied.
func MetadataFrom(labels map[string]string) *nvcrev1alpha1.WorkloadMetadata {
	if len(labels) == 0 {
		return nil
	}
	typed := make(map[string]nvcrev1alpha1.WorkloadLabelValue, len(labels))
	for key, value := range labels {
		typed[key] = nvcrev1alpha1.WorkloadLabelValue(value)
	}
	return &nvcrev1alpha1.WorkloadMetadata{Labels: typed}
}

// MergeLabels returns base overlaid with overlay, per key. Neither argument is
// mutated: catalog templates and API objects are shared, so composing through
// a shared map reference would leak the composition back into its source. An
// empty overlay adds nothing, and an empty string is a real label value, not a
// deletion — there is no deletion syntax. The result is nil when empty.
func MergeLabels(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	merged := make(map[string]string, len(base)+len(overlay))
	maps.Copy(merged, base)
	maps.Copy(merged, overlay)
	return merged
}

// InsertConsistentLabel returns labels with key=value present, following the
// insert/same/conflict rule NVCRE uses wherever it derives a label the user
// may also have written: a missing key is inserted, an identical value is
// accepted so the merge is idempotent, and a different value is a conflict
// reporting the key, both values, and fieldPath. labels is never mutated.
func InsertConsistentLabel(
	labels map[string]string, key, value, fieldPath string,
) (map[string]string, error) {
	if existing, present := labels[key]; present && existing != value {
		return nil, fmt.Errorf(
			"%s[%q]: conflicting label value %q, expected %q",
			fieldPath, key, existing, value)
	}
	merged := make(map[string]string, len(labels)+1)
	maps.Copy(merged, labels)
	merged[key] = value
	return merged, nil
}

// BuildObject constructs the framework workload object for spec and applies
// md's labels to it. It is the one construction seam shared by Job
// reconciliation and dry-run rendering, so the object the controller submits
// and the object the dry run validates cannot drift apart.
//
// Adapter.Build stays responsible for the framework-specific typed object.
// Object labels are handled here instead of behind the Adapter interface
// because every workload kind exposes them identically through client.Object,
// so no adapter needs label-specific code. Adapter-produced labels with
// unrelated keys are preserved. For a requested key the adapter already set,
// an identical value is accepted and a different value is an error: neither
// side silently wins.
//
// Only user-requested metadata is applied. The controller-owned
// identification labels and the owner reference remain the Job controller's to
// overlay afterwards.
func BuildObject(
	adapter Adapter,
	name, namespace string,
	spec *nvcrev1alpha1.WorkloadSpec,
	md *nvcrev1alpha1.WorkloadMetadata,
) (client.Object, error) {
	requested := LabelsOf(md)
	if err := ValidateLabels(requested, "spec.workloadMetadata.labels"); err != nil {
		return nil, err
	}

	obj, err := adapter.Build(name, namespace, spec)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		return obj, nil
	}

	built := obj.GetLabels()
	merged := make(map[string]string, len(built)+len(requested))
	maps.Copy(merged, built)
	for _, key := range slices.Sorted(maps.Keys(requested)) {
		if existing, present := built[key]; present && existing != requested[key] {
			return nil, fmt.Errorf(
				"spec.workloadMetadata.labels[%q]: conflicts with the label %s already set by the workload builder: requested %q, built %q",
				key, adapter.GVK().Kind, requested[key], existing)
		}
		merged[key] = requested[key]
	}
	obj.SetLabels(merged)
	return obj, nil
}
