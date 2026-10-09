// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"strings"

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

// MPIEnvArgs returns mpirun -x args forwarding env to the MPI ranks, which
// start under sshd with a fresh environment and never see container env.
// Names mpiArgs already forwards are skipped, so platform and user mpiArgs
// keep precedence over env.
func MPIEnvArgs(env []corev1.EnvVar, mpiArgs []string) []string {
	forwarded := make(map[string]bool)
	for i := range mpiArgs {
		if name, ok := mpiEnvArgName(mpiArgs, i); ok {
			forwarded[name] = true
		}
	}
	var args []string
	for _, e := range env {
		if !forwarded[e.Name] {
			args = append(args, "-x", mpiEnvArg(e))
		}
	}
	return args
}

// mpiEnvArg renders an env var as an mpirun -x operand. A valueFrom var is
// forwarded by bare name, which mpirun reads from its own (launcher) env,
// where Kubernetes has already resolved it.
func mpiEnvArg(e corev1.EnvVar) string {
	if e.ValueFrom != nil {
		return e.Name
	}
	return e.Name + "=" + e.Value
}

// mpiEnvArgName returns the variable name forwarded by a "-x" at args[i].
func mpiEnvArgName(args []string, i int) (string, bool) {
	if args[i] != "-x" || i+1 >= len(args) {
		return "", false
	}
	name, _, _ := strings.Cut(args[i+1], "=")
	return name, true
}
