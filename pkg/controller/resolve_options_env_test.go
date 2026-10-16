// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	nvlsEnableVar = "NCCL_NVLS_ENABLE"
	ibHCAVar      = "NCCL_IB_HCA"
	debugVar      = "NCCL_DEBUG"
	debugInfo     = "INFO"
	hcaMlx5_0     = "mlx5_0"
)

// TestResolveOptionsEnvDoesNotAliasGlobal pins the aliasing invariant that
// makes the per-name environment merge safe, the same one
// TestResolveOptionsWorkloadMetadataDoesNotAliasGlobal pins for labels.
// ResolveOptions starts with `resolved := *global`, a struct copy that leaves
// every map, slice and pointer aliased to the Certification's own spec. Env is
// merged rather than replaced, so without detaching it first a category's
// variables would be written into the Certification they were read from, and
// because the controller resolves categories in a loop the first category's
// variables would leak into every later one.
//
// An aliasing invariant has no structured output worth snapshotting; what the
// merge produces is covered by the golden cases in pkg/platform, and this
// covers what it must not touch.
func TestResolveOptionsEnvDoesNotAliasGlobal(t *testing.T) {
	global := &nvcrev1alpha1.CategoryOptions{
		Env: []corev1.EnvVar{{Name: nvlsEnableVar, Value: "0"}},
	}
	override := &nvcrev1alpha1.CategoryOptions{
		Env: []corev1.EnvVar{{Name: ibHCAVar, Value: hcaMlx5_0}},
	}

	resolved := ResolveOptions(global, override)

	if got := len(resolved.Env); got != 2 {
		t.Fatalf("resolved env = %d entries, want 2 (global merged with per-category)", got)
	}
	if len(global.Env) != 1 {
		t.Errorf("global env gained the per-category variable: %v", global.Env)
	}
	if len(override.Env) != 1 {
		t.Errorf("per-category env gained the global variable: %v", override.Env)
	}

	// Writing through the result must not reach either source.
	resolved.Env[0].Value = "rewritten"
	if global.Env[0].Value != "0" {
		t.Error("resolved env aliases the global slice")
	}
}

// TestResolveOptionsEnvPerCategoryWins covers the merge rule itself: a category
// that names a variable the global set already carries replaces that value in
// place rather than appending a second entry, and keeps the variables it did
// not name. Order matters as much as content here, because for an MPI entry
// these become positional mpirun arguments.
func TestResolveOptionsEnvPerCategoryWins(t *testing.T) {
	global := &nvcrev1alpha1.CategoryOptions{
		Env: []corev1.EnvVar{
			{Name: nvlsEnableVar, Value: "0"},
			{Name: debugVar, Value: debugInfo},
		},
	}
	override := &nvcrev1alpha1.CategoryOptions{
		Env: []corev1.EnvVar{
			{Name: nvlsEnableVar, Value: "1"},
			{Name: ibHCAVar, Value: hcaMlx5_0},
		},
	}

	resolved := ResolveOptions(global, override)

	want := []corev1.EnvVar{
		{Name: nvlsEnableVar, Value: "1"},
		{Name: debugVar, Value: debugInfo},
		{Name: ibHCAVar, Value: hcaMlx5_0},
	}
	if len(resolved.Env) != len(want) {
		t.Fatalf("resolved env = %v, want %v", resolved.Env, want)
	}
	for i := range want {
		if resolved.Env[i] != want[i] {
			t.Errorf("resolved env[%d] = %v, want %v", i, resolved.Env[i], want[i])
		}
	}
}

// TestResolveOptionsEnvWithoutOverride covers the early return. ResolveOptions
// bails out before the per-field merges when a category has no options at all,
// so the detaching has to happen before that point — otherwise the common case
// would be the aliased one.
//
// It also pins that a Certification setting no variables anywhere resolves to
// an absent field rather than an empty slice, which is what keeps every
// existing render byte-identical to before this field existed.
func TestResolveOptionsEnvWithoutOverride(t *testing.T) {
	global := &nvcrev1alpha1.CategoryOptions{
		Env: []corev1.EnvVar{{Name: nvlsEnableVar, Value: "0"}},
	}

	resolved := ResolveOptions(global, nil)

	if len(resolved.Env) != 1 || resolved.Env[0].Value != "0" {
		t.Fatalf("global env not carried through: %v", resolved.Env)
	}
	resolved.Env[0].Value = "rewritten"
	if global.Env[0].Value != "0" {
		t.Error("resolved env aliases the global slice on the no-override path")
	}

	if env := ResolveOptions(&nvcrev1alpha1.CategoryOptions{}, nil).Env; env != nil {
		t.Errorf("unconfigured env resolved to %+v, want nil", env)
	}
}
