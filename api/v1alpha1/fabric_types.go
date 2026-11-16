// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Fabric names the inter-node network fabric a workload is rendered for
// (ADR-110). The value set matches AICR ADR-028 by agreement: if either side
// grows it, the other repository gets an issue before the change ships.
// +kubebuilder:validation:Enum=efa;ib;roce;tcpxo
type Fabric string

const (
	// FabricEFA is AWS Elastic Fabric Adapter, requested as the
	// vpc.amazonaws.com/efa extended resource.
	FabricEFA Fabric = "efa"
	// FabricIB is InfiniBand.
	FabricIB Fabric = "ib"
	// FabricRoCE is RDMA over Converged Ethernet.
	FabricRoCE Fabric = "roce"
	// FabricTCPXO is GCP's GPUDirect-TCPXO.
	FabricTCPXO Fabric = "tcpxo"
)
