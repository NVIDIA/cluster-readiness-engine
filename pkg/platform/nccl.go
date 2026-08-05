// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	corev1 "k8s.io/api/core/v1"
)

// BaseNCCLEnvVars returns model-independent NCCL environment variables
// that are safe to auto-inject for any workload. These are common across
// all training models and NCCL tests.
//
// Platform-specific NCCL vars (FastRak, IB settings) are injected via
// overrides, not here.
func BaseNCCLEnvVars(enableMNNVL bool) []corev1.EnvVar {
	mnnvl := "0"
	if enableMNNVL {
		mnnvl = "1"
	}

	return []corev1.EnvVar{
		{Name: "NCCL_DEBUG", Value: "INFO"},
		{Name: "NCCL_DEBUG_SUBSYS", Value: "NET,INIT"},
		{Name: "NCCL_NVLS_ENABLE", Value: "1"},
		{Name: "NCCL_CUMEM_ENABLE", Value: "1"},
		{Name: "NCCL_NET_GDR_C2C", Value: "1"},
		{Name: "NCCL_NET_GDR_LEVEL", Value: "PHB"},
		{Name: "NCCL_P2P_NET_CHUNKSIZE", Value: "2097152"},
		{Name: "NCCL_SHM_DISABLE", Value: "1"},
		{Name: "NCCL_MNNVL_ENABLE", Value: mnnvl},
		{Name: "NCCL_SOCKET_IFNAME", Value: "eth0"},
	}
}

// MergeEnvVars merges base env vars with user-provided env vars.
// User-provided values take precedence (override by name).
func MergeEnvVars(base, user []corev1.EnvVar) []corev1.EnvVar {
	if len(user) == 0 {
		return base
	}

	// Build set of user-provided names for quick lookup.
	userNames := make(map[string]struct{}, len(user))
	for _, e := range user {
		userNames[e.Name] = struct{}{}
	}

	// Start with base vars not overridden by user.
	var merged []corev1.EnvVar
	for _, e := range base {
		if _, overridden := userNames[e.Name]; !overridden {
			merged = append(merged, e)
		}
	}

	// Append all user vars.
	merged = append(merged, user...)
	return merged
}
