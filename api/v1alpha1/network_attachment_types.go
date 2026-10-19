// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// MaxNetworkAttachments caps networkAttachments. Sixteen covers one NIC per
// GPU on an eight-GPU node twice over (for example two attachments per rail)
// and keeps the CEL cost of the per-entry rules bounded. nvcrectl checks the
// same cap offline, since the CRD rule only binds server-side.
const MaxNetworkAttachments = 16

// NetworkAttachment names one Multus secondary network and the extended
// resource that backs it (issue #400). Each entry adds the network to the
// worker pod's k8s.v1.cni.cncf.io/networks annotation, in list order, and
// requests one unit of resourceName on the worker container.
//
// Multus does not pair the two by position. The NetworkAttachmentDefinition
// names its own resource in its k8s.v1.cni.cncf.io/resourceName annotation,
// and Multus hands the CNI a device allocated to the pod for that resource,
// so what has to hold is that every attached network's resource is requested
// often enough. resourceName must therefore be the resource the named
// NetworkAttachmentDefinition declares; NVCRE does not read the definition
// and cannot check that. List order still matters: it decides which network
// becomes net1, net2 and so on inside the pod, which is what rail and HCA
// mapping keys on.
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
	// k8s.io domains. Each entry requests one unit, since one entry attaches
	// one interface; entries that share a resourceName add up on the
	// container, so listing a network twice requests two devices for its two
	// interfaces.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[a-zA-Z0-9]([-A-Za-z0-9_.]{0,61}[a-zA-Z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('kubernetes.io/') && !self.startsWith('k8s.io/') && !self.contains('.k8s.io/')",message="resourceName must not use the reserved kubernetes.io or k8s.io domains"
	ResourceName string `json:"resourceName"`
}
