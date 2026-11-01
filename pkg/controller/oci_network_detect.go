// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// OCI RDMA NetworkAttachmentDefinition detection (ADR-103, issue #542).
//
// The OCI RDMA fragment requests nvidia.com/mlnxnics and attaches one
// secondary network per NIC. The platform owns that network and names it, so
// it is found by the k8s.v1.cni.cncf.io/resourceName annotation Multus needs
// to hand each attachment its allocated VF, not assumed. Detection runs only
// when the resolved Workflow still carries catalog.OCISecondaryNetworkUnresolved
// after overlay application, and it never guesses: zero matches, or several
// whose configs differ, fail the Certification before any workload exists.

const (
	// annotationNADResourceNameWrongKey is the device attribute key of
	// Multus's DRA integration. It looks alike, but Multus does not read it
	// from a NAD, so a zero-match message points it out.
	annotationNADResourceNameWrongKey = "k8s.cni.cncf.io/resourceName"

	// nadMessageListLimit caps how many NADs a message names.
	nadMessageListLimit = 10
)

// nadGVK is the NetworkAttachmentDefinition kind, read as unstructured so
// NVCRE takes no dependency on the Multus API types.
var nadGVK = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}

// ociNetworkSelection is the outcome of one selection pass over the NADs.
type ociNetworkSelection struct {
	// Name is the chosen <namespace>/<name>. Empty when selection failed.
	Name string
	// Skipped are the equivalent candidates not chosen, sorted. When it is
	// non-empty the controller emits a Normal SecondaryNetworkDetection event.
	Skipped []string
	// Failure is the user-facing reason no network was chosen. Empty on success.
	Failure string
}

// secondaryNetworkError is a terminal detection failure: no NAD matched,
// several differ, or the NADs cannot be listed for a reason a retry does not
// change. The Certification fails with ReasonSecondaryNetworkDetectionFailed.
type secondaryNetworkError struct {
	Message string
	// Forbidden is true when the list was refused. The CLI dry-run lists with
	// the user's kubeconfig, so it warns on this one instead of failing.
	Forbidden bool
}

func (e *secondaryNetworkError) Error() string { return e.Message }

// nadCandidate is one NAD carrying the OCI resource annotation.
type nadCandidate struct {
	ref    string
	config map[string]any // spec.config without its top-level name; nil when it does not parse
}

// resolveOCISecondaryNetwork lists every NAD and selects the OCI RDMA network
// for a workload in namespace. It returns a *secondaryNetworkError for every
// terminal outcome, and any other error is transient and worth a retry.
func resolveOCISecondaryNetwork(ctx context.Context, reader client.Reader, namespace string) (ociNetworkSelection, error) {
	nads, err := listNetworkAttachmentDefinitions(ctx, reader)
	if err != nil {
		if msg, terminal := nadListErrorMessage(err); terminal {
			return ociNetworkSelection{}, &secondaryNetworkError{Message: msg, Forbidden: apierrors.IsForbidden(err)}
		}
		return ociNetworkSelection{}, fmt.Errorf("listing NetworkAttachmentDefinitions: %w", err)
	}
	sel := selectOCISecondaryNetwork(nads, namespace)
	if sel.Name == "" {
		return sel, &secondaryNetworkError{Message: sel.Failure}
	}
	return sel, nil
}

// listNetworkAttachmentDefinitions lists NADs in every namespace, a page at a
// time. The reader is the uncached APIReader: detection runs once per
// Workflow creation, so it needs no informer and no watch.
func listNetworkAttachmentDefinitions(ctx context.Context, reader client.Reader) ([]unstructured.Unstructured, error) {
	var result []unstructured.Unstructured
	continuation := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(nadGVK.GroupVersion().WithKind(nadGVK.Kind + "List"))
		opts := []client.ListOption{client.Limit(200)}
		if continuation != "" {
			opts = append(opts, client.Continue(continuation))
		}
		if err := reader.List(ctx, list, opts...); err != nil {
			return nil, err
		}
		result = append(result, list.Items...)
		if list.GetContinue() == "" {
			return result, nil
		}
		continuation = list.GetContinue()
	}
}

// nadListErrorMessage reports whether a list error is terminal, and the
// message to fail with. A missing grant or a missing API needs a cluster or
// chart change; anything else may pass on the next reconcile. A client that
// resolved the API before its CRD was removed gets NotFound rather than
// NoMatch; a list of a served resource never returns NotFound.
func nadListErrorMessage(err error) (string, bool) {
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Sprintf("OCI RDMA network detection cannot list NetworkAttachmentDefinitions (%v). "+
			"The chart grants get and list on network-attachment-definitions.k8s.cni.cncf.io to the manager; "+
			"restore that grant, or list the networks explicitly with networkAttachments.", err), true
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		return "OCI RDMA network detection found no NetworkAttachmentDefinition API (k8s.cni.cncf.io/v1) on the cluster, " +
			"so the nvidia.com/mlnxnics request has no Multus network to attach. " + ociNetworkWaysForward, true
	}
	return "", false
}

// ociNetworkWaysForward is in every zero-match message.
const ociNetworkWaysForward = "Create a NetworkAttachmentDefinition annotated " + catalog.NADResourceNameAnnotation + ": " +
	catalog.OCIMlnxResourceName + ", list the networks explicitly with networkAttachments, " +
	"or set mlnxPerNode: 0, which certifies without RDMA NICs unless rdmaDeviceAccess: hostPath is also set " +
	"on a cluster that exposes /dev/infiniband without a device plugin (ADR-088)."

// selectOCISecondaryNetwork applies ADR-103's resolution rules. The
// candidates are the matching NADs in the workload namespace if there are
// any, otherwise all matching NADs. One candidate is used; several equivalent
// ones resolve to the first in sorted order; several that differ, or none,
// fail.
func selectOCISecondaryNetwork(nads []unstructured.Unstructured, workloadNamespace string) ociNetworkSelection {
	var local, all []nadCandidate
	for i := range nads {
		nad := &nads[i]
		if nad.GetAnnotations()[catalog.NADResourceNameAnnotation] != catalog.OCIMlnxResourceName {
			continue
		}
		c := nadCandidate{ref: nad.GetNamespace() + "/" + nad.GetName(), config: nadConfigWithoutName(nad)}
		all = append(all, c)
		if nad.GetNamespace() == workloadNamespace {
			local = append(local, c)
		}
	}
	candidates := all
	if len(local) > 0 {
		candidates = local
	}
	slices.SortFunc(candidates, func(a, b nadCandidate) int { return strings.Compare(a.ref, b.ref) })

	switch {
	case len(candidates) == 0:
		return ociNetworkSelection{Failure: ociNetworkZeroMatchMessage(nads)}
	case len(candidates) == 1:
		return ociNetworkSelection{Name: candidates[0].ref}
	}
	if allEquivalent(candidates) {
		skipped := make([]string, 0, len(candidates)-1)
		for _, c := range candidates[1:] {
			skipped = append(skipped, c.ref)
		}
		return ociNetworkSelection{Name: candidates[0].ref, Skipped: skipped}
	}
	return ociNetworkSelection{Failure: ociNetworkDifferMessage(candidates, len(local) > 0, workloadNamespace)}
}

// nadConfigWithoutName parses spec.config and drops its top-level name, which
// is all two copies of the same network may differ in. It returns nil for a
// config that is empty, where Multus reads the config from disk by the NAD's
// name, or that is not a JSON object, where Multus fails the attachment.
// NVCRE can compare neither, so it is never equivalent.
func nadConfigWithoutName(nad *unstructured.Unstructured) map[string]any {
	raw, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config == nil {
		return nil
	}
	delete(config, "name")
	return config
}

func allEquivalent(candidates []nadCandidate) bool {
	for _, c := range candidates {
		if c.config == nil || !reflect.DeepEqual(c.config, candidates[0].config) {
			return false
		}
	}
	return true
}

func ociNetworkZeroMatchMessage(nads []unstructured.Unstructured) string {
	var b strings.Builder
	b.WriteString("OCI RDMA network detection found no NetworkAttachmentDefinition annotated " +
		catalog.NADResourceNameAnnotation + ": " + catalog.OCIMlnxResourceName)
	if len(nads) == 0 {
		b.WriteString(", and the cluster has no NetworkAttachmentDefinitions at all. ")
		b.WriteString(ociNetworkWaysForward)
		return b.String()
	}
	found := make([]string, 0, len(nads))
	for i := range nads {
		nad := &nads[i]
		ref := nad.GetNamespace() + "/" + nad.GetName()
		annotations := nad.GetAnnotations()
		switch {
		case annotations[catalog.NADResourceNameAnnotation] != "":
			found = append(found, fmt.Sprintf("%s (resourceName %s)", ref, annotations[catalog.NADResourceNameAnnotation]))
		case annotations[annotationNADResourceNameWrongKey] != "":
			found = append(found, fmt.Sprintf("%s (%s is set under %s, which Multus does not read from a NetworkAttachmentDefinition)",
				ref, annotations[annotationNADResourceNameWrongKey], annotationNADResourceNameWrongKey))
		default:
			found = append(found, ref+" (no resourceName)")
		}
	}
	slices.Sort(found)
	// The ways forward come before the list, so an event note cut at its size
	// limit loses NAD names rather than the remedy.
	b.WriteString(". ")
	b.WriteString(ociNetworkWaysForward)
	fmt.Fprintf(&b, " NetworkAttachmentDefinitions found: %s.", joinLimited(found))
	return b.String()
}

func ociNetworkDifferMessage(candidates []nadCandidate, local bool, workloadNamespace string) string {
	refs := make([]string, 0, len(candidates))
	var opaque []string
	for _, c := range candidates {
		refs = append(refs, c.ref)
		if c.config == nil {
			opaque = append(opaque, c.ref)
		}
	}
	scope := "across namespaces"
	if local {
		scope = "in the workload namespace " + workloadNamespace
	}
	var b strings.Builder
	fmt.Fprintf(&b, "OCI RDMA network detection found %d NetworkAttachmentDefinitions for %s %s that are not equivalent.",
		len(candidates), catalog.OCIMlnxResourceName, scope)
	b.WriteString(" NVCRE does not choose between them: list the network to attach explicitly with networkAttachments," +
		" or remove the ones that should not be used.")
	if slices.Contains(refs, catalog.DefaultOCISecondaryNetwork) {
		b.WriteString(" If " + catalog.DefaultOCISecondaryNetwork + " is left over from an earlier setup or was created by hand" +
			" as the workaround for issue #542, delete it: the platform's own network is detected now.")
	}
	// The remedy comes before the lists, so an event note cut at its size
	// limit loses names rather than the remedy.
	fmt.Fprintf(&b, " Candidates: %s.", joinLimited(refs))
	if keys := differingConfigKeys(candidates); len(keys) > 0 {
		fmt.Fprintf(&b, " Their spec.config differs in %s.", strings.Join(keys, ", "))
	}
	if len(opaque) > 0 {
		fmt.Fprintf(&b, " The spec.config of %s is empty or not a JSON object, so it cannot be compared.", joinLimited(opaque))
	}
	return b.String()
}

// differingConfigKeys returns the sorted top-level spec.config keys whose
// values are not the same across the candidates that parse.
func differingConfigKeys(candidates []nadCandidate) []string {
	var parsed []map[string]any
	keys := map[string]struct{}{}
	for _, c := range candidates {
		if c.config == nil {
			continue
		}
		parsed = append(parsed, c.config)
		for k := range c.config {
			keys[k] = struct{}{}
		}
	}
	var differ []string
	for k := range keys {
		v0, ok0 := parsed[0][k]
		for _, config := range parsed[1:] {
			// A key set to null in one config and absent from another differs.
			if v, ok := config[k]; ok != ok0 || !reflect.DeepEqual(v, v0) {
				differ = append(differ, k)
				break
			}
		}
	}
	slices.Sort(differ)
	return differ
}

func joinLimited(items []string) string {
	if len(items) <= nadMessageListLimit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:nadMessageListLimit], ", "), len(items)-nadMessageListLimit)
}

// ociNetworkSkippedMessage is the Normal SecondaryNetworkDetection event text
// for a pick among equivalent candidates.
func ociNetworkSkippedMessage(sel ociNetworkSelection) string {
	return fmt.Sprintf("OCI RDMA network detection attached %s; equivalent NetworkAttachmentDefinitions for %s skipped: %s.",
		sel.Name, catalog.OCIMlnxResourceName, joinLimited(sel.Skipped))
}
