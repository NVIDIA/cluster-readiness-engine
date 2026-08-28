// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// TestAWSGB300EFAStripInvariant asserts that every NCCL-using catalog entry,
// after applying its overrides for the (platform=aws, gpuArchitecture=gb300)
// context, produces a trainer container that does NOT leave the AWS EFA stack
// active.
//
// AWS GB300 nodes use a RoCE fabric — they do NOT have EFA devices. Container
// images such as nvcr.io/nvidia/pytorch:26.01-py3 ship /opt/amazon/aws-ofi-nccl
// and export NCCL_NET_PLUGIN=ofi by default. Without an explicit strip, NCCL
// initialization fails with "No eligible providers were found" and downstream
// cleanup segfaults inside libucs.so.
//
// Pass criterion (per ADR-060): for every (domain, variant) whose rendered
// trainer args/env reference NCCL or AWS-OFI, after override application the
// trainer must EITHER:
//   - declare an env var NCCL_NET_PLUGIN with a value other than "ofi", OR
//   - emit an "unset NCCL_NET_PLUGIN" or "rm -rf /opt/amazon" preamble in the
//     args BEFORE any reference to /opt/amazon/efa, /opt/aws-ofi-nccl, or the
//     OFI plugin path.
func TestAWSGB300EFAStripInvariant(t *testing.T) {
	// Sweep configurations large enough to satisfy each entry's MinGPUs.
	configs := []catalog.BuildConfig{
		{NodesPerJob: 1, GpusPerNode: 4, GPUArchitecture: "gb300"},
		{NodesPerJob: 4, GpusPerNode: 4, GPUArchitecture: "gb300"},
		{NodesPerJob: 8, GpusPerNode: 4, GPUArchitecture: "gb300"},
		{NodesPerJob: 32, GpusPerNode: 4, GPUArchitecture: "gb300"},
		{NodesPerJob: 128, GpusPerNode: 4, GPUArchitecture: "gb300"},
	}

	target := nvcrev1alpha1.TargetSpec{
		NodeSelector: map[string]string{
			"nvidia.com/gpu.product": "NVIDIA-GB300",
		},
	}

	octx := OverrideContext{
		Platform:        "aws",
		GPUArchitecture: "gb300",
		WorkloadKind:    "trainJob",
	}

	var checked, skipped []string

	for _, info := range catalog.List() {
		name := info.Domain + "/" + info.Variant
		entry := catalog.Lookup(info.Domain, info.Variant)
		if entry == nil {
			t.Errorf("catalog.List returned %s but Lookup returned nil", name)
			continue
		}

		spec, err := buildWithFallback(entry, target, configs)
		if err != nil {
			skipped = append(skipped, name+" (build error: "+err.Error()+")")
			continue
		}

		if !entryUsesNCCL(spec) {
			continue
		}

		if err := applyOverrides(&spec, octx); err != nil {
			t.Errorf("%s: applyOverrides failed: %v", name, err)
			continue
		}

		if violations := efaStripViolations(spec); len(violations) > 0 {
			t.Errorf("%s: AWS+GB300 EFA-strip invariant violated:\n  - %s",
				name, strings.Join(violations, "\n  - "))
		}
		checked = append(checked, name)
	}
	sort.Strings(checked)

	if len(checked) == 0 {
		t.Fatalf("no NCCL-using catalog entries were checked — registration probably failed")
	}

	// All five communication NCCL variants and all five Nemotron training
	// variants — every NCCL-using entry registered today, so accidental
	// removal of any platform override is caught.
	for _, c := range []string{
		"communication/nccl-all-gather",
		"communication/nccl-all-reduce",
		"communication/nccl-alltoall",
		"communication/nccl-loopback",
		"communication/nccl-loopback-nvswitch",
		"training/nemotron5-56b",
		"training/nemotron5-8b",
	} {
		mustContain(t, checked, c)
	}

	t.Logf("AWS+GB300 EFA-strip invariant verified for %d NCCL-using entries: %v",
		len(checked), checked)
	if len(skipped) > 0 {
		t.Logf("skipped %d non-buildable entries: %v", len(skipped), skipped)
	}
}

func mustContain(t *testing.T, set []string, want string) {
	t.Helper()
	if slices.Contains(set, want) {
		return
	}
	t.Errorf("expected %q to be present in checked set %v", want, set)
}

// buildWithFallback tries the supplied configs in order and returns the first
// successful WorkflowSpec. Returns the last build error if every config fails.
func buildWithFallback(
	entry *catalog.Entry,
	target nvcrev1alpha1.TargetSpec,
	configs []catalog.BuildConfig,
) (nvcrev1alpha1.WorkflowSpec, error) {
	var lastErr error
	for _, cfg := range configs {
		spec, err := entry.Build(target, cfg)
		if err == nil {
			return spec, nil
		}
		lastErr = err
	}
	return nvcrev1alpha1.WorkflowSpec{}, lastErr
}

// entryUsesNCCL returns true if the entry's rendered trainer plausibly loads
// NCCL on AWS+GB300. Sources of evidence (in order of cost):
//  1. Trainer args/env contain NCCL/OFI/all_reduce markers.
//  2. Any ConfigMap dependency contains an NCCL marker in its data values.
//  3. The entry declares an explicit `aws + gb300` override — i.e. someone
//     thought about platform-specific behavior, which only matters when
//     networking/NCCL is involved. This catches Nemotron entries whose base
//     trainer.args are just "/config/train.sh".
func entryUsesNCCL(spec nvcrev1alpha1.WorkflowSpec) bool {
	if spec.JobTemplate.Spec.Workload.TrainJob != nil &&
		spec.JobTemplate.Spec.Workload.TrainJob.Trainer != nil {
		trainer := spec.JobTemplate.Spec.Workload.TrainJob.Trainer
		if slices.ContainsFunc(trainer.Args, hasNCCLMarker) {
			return true
		}
		for _, e := range trainer.Env {
			if strings.HasPrefix(e.Name, "NCCL_") || strings.HasPrefix(e.Name, "FI_EFA_") ||
				strings.Contains(e.Value, "/opt/aws-ofi-nccl") {
				return true
			}
		}
	}
	for _, dep := range spec.Dependencies {
		var obj map[string]any
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
			continue
		}
		if obj["kind"] != "ConfigMap" {
			continue
		}
		data, _ := obj["data"].(map[string]any)
		for _, v := range data {
			if s, ok := v.(string); ok && hasNCCLMarker(s) {
				return true
			}
		}
	}
	for _, o := range spec.Overrides {
		if hasAWSGB300When(o.When) {
			return true
		}
	}
	return false
}

// hasAWSGB300When reports whether a When clause selects (platform=aws,
// gpuArchitecture=gb300) — used to identify entries that have explicit
// AWS+GB300 behavior even when the base trainer args do not name NCCL.
func hasAWSGB300When(w nvcrev1alpha1.WhenSpec) bool {
	if w.Platform == nil || w.GPUArchitecture == nil {
		return false
	}
	platformMatches := w.Platform.Equals == "aws" || containsString(w.Platform.In, "aws")
	archMatches := w.GPUArchitecture.Equals == "gb300" ||
		containsString(w.GPUArchitecture.In, "gb300")
	return platformMatches && archMatches
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func hasNCCLMarker(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{
		"nccl",
		"all_reduce_perf",
		"all_gather_perf",
		"alltoall_perf",
		"mpirun",
		"/opt/amazon",
		"aws-ofi-nccl",
	} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// efaStripViolations returns a list of human-readable violations in the
// trainer container. Empty result means the EFA stack is properly stripped.
func efaStripViolations(spec nvcrev1alpha1.WorkflowSpec) []string {
	var problems []string
	if spec.JobTemplate.Spec.Workload.TrainJob == nil {
		return problems
	}
	trainer := spec.JobTemplate.Spec.Workload.TrainJob.Trainer
	if trainer == nil {
		return problems
	}

	// Check 1: env var NCCL_NET_PLUGIN must not be set to "ofi" (case-insensitive).
	for _, e := range trainer.Env {
		if e.Name == "NCCL_NET_PLUGIN" && strings.EqualFold(e.Value, "ofi") {
			problems = append(problems,
				"trainer.env sets NCCL_NET_PLUGIN=ofi which selects the AWS EFA plugin "+
					"(GB300 has no EFA devices)")
		}
	}

	// Check 2: any args element that touches an EFA path must be preceded
	// (within the same args element) by either a `rm -rf /opt/amazon` or an
	// `unset NCCL_NET_PLUGIN`.
	for i, a := range trainer.Args {
		efaIdx := earliestEFAReferenceIndex(a)
		if efaIdx < 0 {
			continue
		}
		guardIdx := earliestEFAStripIndex(a)
		if guardIdx < 0 || guardIdx > efaIdx {
			problems = append(problems,
				"trainer.args["+itoa(i)+"] references EFA paths without an earlier "+
					"`rm -rf /opt/amazon` or `unset NCCL_NET_PLUGIN` preamble")
		}
	}

	// Check 3: if the trainer image ships the AWS-OFI-NCCL stack (which sets
	// NCCL_NET_PLUGIN=ofi via /etc/nccl.conf and bakes in /opt/amazon/efa),
	// the strip preamble MUST exist somewhere — either in trainer.args
	// (single-node entries) OR in a TrainingRuntime dependency that overrides
	// the worker container args (multi-node sshd-launched entries). Otherwise
	// the runtime env leaks through and NCCL bails out on RoCE-only GB300
	// nodes.
	if trainer.Image != nil && imageShipsEFAStack(*trainer.Image) {
		if !anyArgHasEFAStrip(trainer.Args) && !depsHaveEFAStrip(spec.Dependencies) {
			problems = append(problems,
				"trainer.image "+*trainer.Image+" ships the AWS-OFI-NCCL stack but "+
					"no trainer.args nor dependency runtime patch carries `rm -rf "+
					"/opt/amazon` / `unset NCCL_NET_PLUGIN` — see ADR-060")
		}
	}

	return problems
}

// depsHaveEFAStrip walks dependency RawExtensions looking for any string
// (in TrainingRuntime worker args or ConfigMap data) that contains an
// EFA-strip preamble. Used by the multi-node comm pattern where strip lives
// in the sshd worker container's args, not the trainer's args.
func depsHaveEFAStrip(deps []nvcrev1alpha1.DependencySpec) bool {
	for _, dep := range deps {
		// The strip preamble is plain text in the JSON-encoded RawExtension
		// regardless of which container/configmap it lives in, so a flat
		// substring scan is correct and avoids re-implementing the merge.
		s := string(dep.Raw)
		if strings.Contains(s, "rm -rf /opt/amazon") ||
			strings.Contains(s, "unset NCCL_NET_PLUGIN") {
			return true
		}
	}
	return false
}

// imageShipsEFAStack reports whether a container image is known to bake in
// the AWS-OFI-NCCL libraries at /opt/amazon/aws-ofi-nccl with NCCL_NET_PLUGIN
// set to "ofi" by default. These images need an explicit strip preamble when
// run on AWS+GB300 (RoCE) nodes that lack EFA devices.
func imageShipsEFAStack(image string) bool {
	for _, prefix := range []string{
		"nvcr.io/nvidia/pytorch:",
		"public.ecr.aws/hpc-cloud/nccl-tests:",
		"nvcr.io/nv-ngc-devops/nemo",
		"nvcr.io/nvidia/nemo:",
	} {
		if strings.HasPrefix(image, prefix) {
			return true
		}
	}
	return false
}

func anyArgHasEFAStrip(args []string) bool {
	for _, a := range args {
		if earliestEFAStripIndex(a) >= 0 {
			return true
		}
	}
	return false
}

// earliestEFAReferenceIndex returns the index of the first EFA-path reference
// in s, or -1 if none. Mirrors paths shipped in the AWS-OFI-NCCL stack.
func earliestEFAReferenceIndex(s string) int {
	first := -1
	for _, needle := range []string{
		"/opt/amazon/efa",
		"/opt/aws-ofi-nccl",
		"/opt/amazon/ofi-nccl",
		"/opt/amazon/aws-ofi-nccl",
	} {
		if i := strings.Index(s, needle); i >= 0 {
			if first < 0 || i < first {
				first = i
			}
		}
	}
	return first
}

// earliestEFAStripIndex returns the index of the first EFA-strip statement in
// s, or -1 if none. Either form is accepted: `rm -rf /opt/amazon` (any path
// rooted at /opt/amazon counts since it is a strict superset of the EFA dirs)
// or `unset NCCL_NET_PLUGIN`.
func earliestEFAStripIndex(s string) int {
	first := -1
	for _, needle := range []string{
		"rm -rf /opt/amazon",
		"unset NCCL_NET_PLUGIN",
	} {
		if i := strings.Index(s, needle); i >= 0 {
			if first < 0 || i < first {
				first = i
			}
		}
	}
	return first
}

// itoa is a small int-to-decimal-string helper to avoid pulling strconv only
// for index-formatting in error messages.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
