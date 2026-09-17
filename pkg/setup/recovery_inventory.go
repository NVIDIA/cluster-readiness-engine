// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type recoveryObjectEvidence struct {
	uid             types.UID
	ownerReferences []metav1.OwnerReference
	classification  string
}

type recoveryEvidence struct {
	namespaceUID types.UID
	objects      map[string]recoveryObjectEvidence
	resources    map[string]bool
}

func collectRecoveryEvidence(
	sp setupPhaseParams, prior *recoveryEvidence, trainerAPIsRemoved bool,
) (recoveryEvidence, []string) {
	evidence := recoveryEvidence{objects: map[string]recoveryObjectEvidence{}, resources: map[string]bool{}}
	namespace := &unstructured.Unstructured{}
	namespace.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: kindNamespace})
	if err := sp.c.Get(sp.ctx, client.ObjectKey{Name: trainerNamespace}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return evidence, nil
		}
		return evidence, []string{fmt.Sprintf("cannot get namespace %s: %v", trainerNamespace, err)}
	}
	evidence.namespaceUID = namespace.GetUID()

	resources, err := discoverProtectedResources(sp)
	if err != nil {
		return evidence, []string{err.Error()}
	}
	var objects []unstructured.Unstructured
	for _, resource := range resources {
		if trainerAPIsRemoved && resource.gvk.Group == trainerAPIGroup {
			continue
		}
		evidence.resources[resource.resource] = true
		items, listErr := listAllUnstructured(sp.ctx, sp.c, resource.gvk, trainerNamespace)
		if listErr != nil {
			return evidence, []string{fmt.Sprintf("cannot list %s in namespace %s: %v",
				resource.resource, trainerNamespace, listErr)}
		}
		objects = append(objects, items...)
	}

	allowed := map[types.UID]unstructured.Unstructured{}
	pending := make([]unstructured.Unstructured, 0)
	var blockers []string
	for _, object := range objects {
		if allowedObject, classification := directlyAllowedRecoveryObject(sp, object); allowedObject {
			if object.GetUID() == "" {
				blockers = append(blockers, fmt.Sprintf("protected %s %s/%s has no UID", object.GetKind(), object.GetNamespace(), object.GetName()))
				continue
			}
			allowed[object.GetUID()] = object
			evidence.objects[recoveryIdentity(object)] = objectEvidence(object, classification)
			continue
		}
		if matchesPriorEvidence(prior, object) {
			allowed[object.GetUID()] = object
			evidence.objects[recoveryIdentity(object)] = objectEvidence(object, prior.objects[recoveryIdentity(object)].classification)
			continue
		}
		pending = append(pending, object)
	}

	// Resolve verified descendants strictly by controller-owner UID. Repeating
	// permits Deployment -> ReplicaSet -> Pod chains without trusting labels.
	for changed := true; changed; {
		changed = false
		remaining := pending[:0]
		for _, object := range pending {
			owner, ok := controllerOwner(object)
			ownerObject, ownerAllowed := allowed[owner.UID]
			if ok && ownerAllowed && ownerReferenceMatches(owner, ownerObject, object.GetNamespace()) {
				allowed[object.GetUID()] = object
				evidence.objects[recoveryIdentity(object)] = objectEvidence(object, "uid-linked-descendant")
				changed = true
				continue
			}
			remaining = append(remaining, object)
		}
		pending = remaining
	}
	remaining := pending[:0]
	for _, object := range pending {
		if ok, classification := allowedControllerLease(object, allowed); ok {
			if object.GetUID() == "" {
				remaining = append(remaining, object)
				continue
			}
			allowed[object.GetUID()] = object
			evidence.objects[recoveryIdentity(object)] = objectEvidence(object, classification)
			continue
		}
		remaining = append(remaining, object)
	}
	pending = remaining
	for _, object := range pending {
		blockers = append(blockers, fmt.Sprintf("protected %s %s/%s is not a verified Trainer release resource or UID-linked descendant",
			object.GetKind(), object.GetNamespace(), object.GetName()))
	}
	sort.Strings(blockers)
	return evidence, blockers
}

type protectedAPIResource struct {
	resource string
	gvk      schema.GroupVersionKind
}

func discoverProtectedResources(sp setupPhaseParams) ([]protectedAPIResource, error) {
	if sp.discoverNamespaced == nil {
		return nil, fmt.Errorf("cannot discover protected namespaced resources: discovery client is unavailable")
	}
	lists, err := sp.discoverNamespaced()
	if err != nil {
		return nil, fmt.Errorf("cannot complete discovery of protected namespaced resources: %w", err)
	}
	var result []protectedAPIResource
	seen := map[string]bool{}
	for _, list := range lists {
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			return nil, fmt.Errorf("parse discovered groupVersion %q: %w", list.GroupVersion, parseErr)
		}
		for _, resource := range list.APIResources {
			if !resource.Namespaced || strings.Contains(resource.Name, "/") ||
				!containsString(resource.Verbs, "list") || excludedRecoveryResource(gv.Group, resource.Name) {
				continue
			}
			key := gv.String() + "/" + resource.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, protectedAPIResource{
				resource: key,
				gvk:      schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: resource.Kind},
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].resource < result[j].resource })
	return result, nil
}

func excludedRecoveryResource(group, resource string) bool {
	return (group == "" && (resource == "events" || resource == "endpoints")) ||
		(group == "events.k8s.io" && resource == "events") ||
		(group == "discovery.k8s.io" && resource == "endpointslices")
}

func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

func directlyAllowedRecoveryObject(sp setupPhaseParams, object unstructured.Unstructured) (bool, string) {
	_, inManifest := sp.releaseManifestObjects[manifestIdentity(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())]
	if completeHelmOwner(&object).bundled() && inManifest {
		return true, "exact-release-manifest:" + stableClassification(object)
	}
	switch object.GetAPIVersion() + "/" + object.GetKind() {
	case "v1/ServiceAccount":
		allowed := object.GetName() == "default" && len(object.GetOwnerReferences()) == 0 &&
			len(object.GetLabels()) == 0 && len(object.GetAnnotations()) == 0
		secrets, _, _ := unstructured.NestedSlice(object.Object, "secrets")
		pullSecrets, _, _ := unstructured.NestedSlice(object.Object, "imagePullSecrets")
		automount, found, _ := unstructured.NestedBool(object.Object, "automountServiceAccountToken")
		allowed = allowed && len(secrets) == 0 && len(pullSecrets) == 0 && (!found || !automount)
		return allowed, "default-service-account:" + stableClassification(object)
	case "v1/ConfigMap":
		labels := object.GetLabels()
		annotations := object.GetAnnotations()
		allowedAnnotations := len(annotations) == 0 ||
			(len(annotations) == 1 && annotations["kubernetes.io/description"] != "")
		data, _, _ := unstructured.NestedStringMap(object.Object, "data")
		binary, _, _ := unstructured.NestedStringMap(object.Object, "binaryData")
		allowed := object.GetName() == "kube-root-ca.crt" && len(object.GetOwnerReferences()) == 0 &&
			len(labels) == 0 &&
			allowedAnnotations && len(data) == 1 && data["ca.crt"] != "" && len(binary) == 0
		return allowed, "kube-root-ca-configmap:" + stableClassification(object)
	case "v1/Secret":
		return isTrainerHelmStorage(object), "exact-trainer-helm-storage:" + stableClassification(object)
	default:
		return false, ""
	}
}

// isTrainerHelmStorage verifies the stored release identity, not just a name
// prefix that an unrelated Secret could share. Unreadable records stay protected.
func isTrainerHelmStorage(object unstructured.Unstructured) bool {
	labels := object.GetLabels()
	version, err := strconv.Atoi(labels["version"])
	if err != nil || version <= 0 || strconv.Itoa(version) != labels["version"] ||
		object.GetNamespace() != trainerNamespace ||
		object.GetName() != "sh.helm.release.v1."+trainerReleaseName+".v"+labels["version"] ||
		labels["owner"] != phaseHelm || labels["name"] != trainerReleaseName ||
		len(object.GetOwnerReferences()) != 0 {
		return false
	}
	data, _, err := unstructured.NestedStringMap(object.Object, "data")
	typeName, _, typeErr := unstructured.NestedString(object.Object, "type")
	if err != nil || typeErr != nil || typeName != "helm.sh/release.v1" || len(data) != 1 {
		return false
	}
	// The API encodes Secret bytes in base64; Helm itself stores base64 of
	// gzipped JSON (or plain JSON for legacy records) in those bytes.
	encoded, err := base64.StdEncoding.DecodeString(data["release"])
	if err != nil {
		return false
	}
	payload, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return false
	}
	if bytes.HasPrefix(payload, []byte{0x1f, 0x8b, 0x08}) {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return false
		}
		defer func() { _ = reader.Close() }()
		// Bound decompression of untrusted input. Oversized records block
		// automatic recovery; they are never treated as disposable storage.
		const maxReleaseBytes = 16 << 20
		payload, err = io.ReadAll(io.LimitReader(reader, maxReleaseBytes+1))
		if err != nil || len(payload) > maxReleaseBytes {
			return false
		}
	}
	var release struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int    `json:"version"`
		Info      struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	return json.Unmarshal(payload, &release) == nil &&
		release.Name == trainerReleaseName && release.Namespace == trainerNamespace &&
		release.Version == version && release.Info.Status != "" && release.Info.Status == labels["status"]
}

func allowedControllerLease(
	object unstructured.Unstructured, allowed map[types.UID]unstructured.Unstructured,
) (bool, string) {
	if object.GetAPIVersion() != "coordination.k8s.io/v1" || object.GetKind() != "Lease" ||
		(object.GetName() != "trainer.kubeflow.org" && object.GetName() != "6d4f6a47.jobset.x-k8s.io") ||
		len(object.GetLabels()) != 0 || len(object.GetAnnotations()) != 0 ||
		len(object.GetOwnerReferences()) != 0 {
		return false, ""
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		return false, ""
	}
	for field := range spec {
		switch field {
		case "holderIdentity", "leaseDurationSeconds", "acquireTime", "renewTime", "leaseTransitions":
		default:
			return false, ""
		}
	}
	holder, _, _ := unstructured.NestedString(object.Object, "spec", "holderIdentity")
	duration, _, _ := unstructured.NestedInt64(object.Object, "spec", "leaseDurationSeconds")
	podName, _, ok := strings.Cut(holder, "_")
	if !ok || podName == "" || duration != 15 {
		return false, ""
	}
	for _, candidate := range allowed {
		if candidate.GetAPIVersion() == "v1" && candidate.GetKind() == "Pod" &&
			candidate.GetNamespace() == object.GetNamespace() && candidate.GetName() == podName {
			return true, "verified-controller-lease:" + stableLeaseClassification(object, podName)
		}
	}
	return false, ""
}

func stableLeaseClassification(object unstructured.Unstructured, podName string) string {
	copy := object.DeepCopy()
	_ = unstructured.SetNestedField(copy.Object, podName, "spec", "holderIdentity")
	unstructured.RemoveNestedField(copy.Object, "spec", "acquireTime")
	unstructured.RemoveNestedField(copy.Object, "spec", "renewTime")
	unstructured.RemoveNestedField(copy.Object, "spec", "leaseTransitions")
	return stableClassification(*copy)
}

func matchesPriorEvidence(prior *recoveryEvidence, object unstructured.Unstructured) bool {
	if prior == nil {
		return false
	}
	want, ok := prior.objects[recoveryIdentity(object)]
	if !ok || want.uid != object.GetUID() {
		return false
	}
	current := objectEvidence(object, want.classification)
	return reflect.DeepEqual(want.ownerReferences, current.ownerReferences) &&
		want.classification == currentClassification(prior, object)
}

func currentClassification(prior *recoveryEvidence, object unstructured.Unstructured) string {
	want := prior.objects[recoveryIdentity(object)].classification
	switch {
	case strings.HasPrefix(want, "exact-release-manifest:"):
		return "exact-release-manifest:" + stableClassification(object)
	case strings.HasPrefix(want, "verified-controller-lease:"):
		holder, _, _ := unstructured.NestedString(object.Object, "spec", "holderIdentity")
		podName, _, _ := strings.Cut(holder, "_")
		return "verified-controller-lease:" + stableLeaseClassification(object, podName)
	case strings.HasPrefix(want, "default-service-account:"):
		return "default-service-account:" + stableClassification(object)
	case strings.HasPrefix(want, "kube-root-ca-configmap:"):
		return "kube-root-ca-configmap:" + stableClassification(object)
	case strings.HasPrefix(want, "exact-trainer-helm-storage:"):
		return "exact-trainer-helm-storage:" + stableClassification(object)
	default:
		return want
	}
}

func stableClassification(object unstructured.Unstructured) string {
	copy := object.DeepCopy().Object
	unstructured.RemoveNestedField(copy, "metadata", "uid")
	unstructured.RemoveNestedField(copy, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(copy, "metadata", "generation")
	unstructured.RemoveNestedField(copy, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(copy, "metadata", "managedFields")
	// Deletion progress is expected after Helm uninstall and while dependent
	// objects drain. It must not invalidate otherwise identical provenance.
	unstructured.RemoveNestedField(copy, "metadata", "deletionTimestamp")
	unstructured.RemoveNestedField(copy, "metadata", "deletionGracePeriodSeconds")
	unstructured.RemoveNestedField(copy, "metadata", "finalizers")
	unstructured.RemoveNestedField(copy, "status")
	value, _ := json.Marshal(copy)
	return string(value)
}

func controllerOwner(object unstructured.Unstructured) (metav1.OwnerReference, bool) {
	for _, owner := range object.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller && owner.UID != "" {
			return owner, true
		}
	}
	return metav1.OwnerReference{}, false
}

func ownerReferenceMatches(owner metav1.OwnerReference, ownerObject unstructured.Unstructured, childNamespace string) bool {
	return owner.UID == ownerObject.GetUID() && owner.APIVersion == ownerObject.GetAPIVersion() &&
		owner.Kind == ownerObject.GetKind() && owner.Name == ownerObject.GetName() &&
		(ownerObject.GetNamespace() == "" || ownerObject.GetNamespace() == childNamespace)
}

func recoveryIdentity(object unstructured.Unstructured) string {
	return object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
}

func objectEvidence(object unstructured.Unstructured, classification string) recoveryObjectEvidence {
	result := recoveryObjectEvidence{uid: object.GetUID(), classification: classification}
	result.ownerReferences = object.GetOwnerReferences()
	return result
}

func compareRecoveryEvidence(previous, current recoveryEvidence, trainerAPIsRemoved bool) []string {
	var blockers []string
	if previous.namespaceUID != "" && current.namespaceUID != previous.namespaceUID {
		blockers = append(blockers, fmt.Sprintf("namespace %s UID changed from %s to %s",
			trainerNamespace, previous.namespaceUID, current.namespaceUID))
	}
	for resource := range previous.resources {
		if current.resources[resource] || (trainerAPIsRemoved && strings.HasPrefix(resource, trainerAPIGroup+"/")) {
			continue
		}
		blockers = append(blockers, "previously discovered protected API resource disappeared: "+resource)
	}
	for identity, currentObject := range current.objects {
		previousObject, ok := previous.objects[identity]
		if !ok {
			blockers = append(blockers, "new protected object: "+identity)
			continue
		}
		if previousObject.uid != currentObject.uid {
			blockers = append(blockers, "replacement UID for protected object: "+identity)
			continue
		}
		if !reflect.DeepEqual(previousObject.ownerReferences, currentObject.ownerReferences) {
			blockers = append(blockers, "changed ownership for protected object: "+identity)
		}
		if previousObject.classification != currentObject.classification {
			blockers = append(blockers, "changed classification evidence for protected object: "+identity)
		}
	}
	sort.Strings(blockers)
	return blockers
}

func waitForObjectDeletion(
	ctx context.Context, c client.Client, object client.Object, description string,
) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	key := client.ObjectKeyFromObject(object)
	for {
		probe := &unstructured.Unstructured{}
		probe.SetGroupVersionKind(object.GetObjectKind().GroupVersionKind())
		if err := c.Get(ctx, key, probe); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("verify deletion of %s: %w", description, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s deletion: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func deleteTrainerCRDWithRecheck(sp setupPhaseParams, name string) error {
	safe, blockers := trainerWorkloadGate(sp)
	if !safe {
		return fmt.Errorf("late recovery refusal before deleting CRD %s: %s", name, strings.Join(blockers, "; "))
	}
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: kindCustomResourceDefinition})
	crd.SetName(name)
	if err := sp.c.Delete(sp.ctx, crd); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete request for CRD %s failed; deletion outcome is uncertain: %w", name, err)
	}
	if err := waitForObjectDeletion(sp.ctx, sp.c, crd, "CRD "+name); err != nil {
		return fmt.Errorf("CRD %s deletion was requested but not confirmed: %w", name, err)
	}
	_, _ = fmt.Fprintf(sp.out, "[deps] Confirmed Trainer CRD %s is absent after cleanup.\n", name)
	return nil
}

func deleteNamespaceWithUID(sp setupPhaseParams, uid types.UID) error {
	ns := &unstructured.Unstructured{}
	ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	ns.SetName(trainerNamespace)
	if uid == "" {
		return nil
	}
	if err := sp.c.Delete(sp.ctx, ns, client.Preconditions{UID: &uid}); err != nil {
		return fmt.Errorf("delete namespace %s with inspected UID %s: %w", trainerNamespace, uid, err)
	}
	return waitForObjectDeletion(sp.ctx, sp.c, ns, "namespace "+trainerNamespace)
}
