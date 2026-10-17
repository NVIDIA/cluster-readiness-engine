// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// MaxNetworkAttachments caps networkAttachments. Sixteen covers one NIC per
// GPU on an eight-GPU node twice over (for example two attachments per rail)
// and keeps the CEL cost of the per-entry rules bounded. nvcrectl checks the
// same cap offline, since the CRD rule only binds server-side.
const MaxNetworkAttachments = 16

// NetworkAttachment pairs one Multus secondary network with the extended
// resource that backs it (issue #400). Each entry adds the network to the
// worker pod's k8s.v1.cni.cncf.io/networks annotation, in list order, and
// requests count units of resourceName on the worker container. The two are
// kept in one entry rather than two parallel lists because the Network
// Operator matches them by position: the n-th attachment in the annotation is
// wired to a device of the n-th requested resource.
type NetworkAttachment struct {
	// network names the NetworkAttachmentDefinition, either "<name>" (resolved
	// in the workload's namespace) or "<namespace>/<name>". The namespace must
	// be a DNS-1123 label and the name a DNS-1123 subdomain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self.split('/')[self.split('/').size() - 1].size() <= 253",message="network name must be at most 253 characters"
	Network string `json:"network"`

	// resourceName is the extended resource requested for this attachment on
	// the worker container (for example "nvidia.com/r0"). It follows the same
	// rules as nicResourceName: a DNS-subdomain domain, a slash, and a name
	// segment of at most 63 characters, outside the reserved kubernetes.io and
	// k8s.io domains.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[a-zA-Z0-9]([-A-Za-z0-9_.]{0,61}[a-zA-Z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('kubernetes.io/') && !self.startsWith('k8s.io/') && !self.contains('.k8s.io/')",message="resourceName must not use the reserved kubernetes.io or k8s.io domains"
	ResourceName string `json:"resourceName"`

	// count is the number of resourceName units requested for this
	// attachment. Defaults to 1 when unset. Entries that share a resourceName
	// add up on the container.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count *int32 `json:"count,omitempty"`
}

// EffectiveCount returns count, or 1 when unset.
func (a NetworkAttachment) EffectiveCount() int32 {
	if a.Count == nil {
		return 1
	}
	return *a.Count
}
