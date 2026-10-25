// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
)

// Every NCCL entry fails fast (issue #498). A launcher, or the one pod of a
// single-pod entry, that restarted in place with restartPolicy OnFailure and
// the default backoffLimit of 6 crash-looped for about seven minutes before
// the Job failed, with every target node holding its GPUs. The MPI launcher
// keeps one replacement pod because the dependsOn gate proves each worker's
// own sshd, not that the launcher can reach it; a single-pod entry has no peer
// to wait for and fails on its first non-zero exit. MPI workers keep
// OnFailure: they only run sshd, and the JobSet tears them down once the
// launcher Job fails.
func TestNCCLEntriesFailFast(t *testing.T) {
	var checked int
	for _, c := range List() {
		if c.Domain != "communication" || !strings.HasPrefix(c.Variant, "nccl-") {
			continue
		}
		spec := buildForRestartPolicy(t, c)
		jobs := replicatedJobsOf(t, spec)
		if len(jobs) == 0 {
			t.Fatalf("%s/%s: no replicatedJobs rendered", c.Domain, c.Variant)
		}
		launcher, hasLauncher := jobs["launcher"]
		node, hasNode := jobs["node"]
		if !hasNode {
			t.Fatalf("%s/%s: no node replicatedJob", c.Domain, c.Variant)
		}
		if hasLauncher {
			requireFailFast(t, c.Variant+" launcher", launcher, 1)
			if got := restartPolicyOf(node); got != "OnFailure" {
				t.Errorf("%s worker: restartPolicy = %q, want OnFailure", c.Variant, got)
			}
		} else {
			requireFailFast(t, c.Variant+" node", node, 0)
		}
		checked++
	}
	if checked < 5 {
		t.Fatalf("checked %d NCCL entries, want at least 5 (all-reduce, all-gather, alltoall, loopback, loopback-nvswitch)", checked)
	}
}

func requireFailFast(t *testing.T, what string, job map[string]any, backoffLimit float64) {
	t.Helper()
	if got := restartPolicyOf(job); got != "Never" {
		t.Errorf("%s: restartPolicy = %q, want Never", what, got)
	}
	jobSpec := dig(job, "template", "spec")
	if got, ok := jobSpec["backoffLimit"].(float64); !ok || got != backoffLimit {
		t.Errorf("%s: backoffLimit = %v, want %v", what, jobSpec["backoffLimit"], backoffLimit)
	}
	rules, _ := dig(jobSpec, "podFailurePolicy")["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("%s: podFailurePolicy rules = %v, want one DisruptionTarget Ignore rule", what, rules)
	}
	rule, _ := rules[0].(map[string]any)
	conds, _ := rule["onPodConditions"].([]any)
	// Trainer's TrainingRuntime schema requires the condition status, so a
	// rule without it would make the runtime fail admission.
	if rule["action"] != "Ignore" || len(conds) != 1 ||
		conds[0].(map[string]any)["type"] != "DisruptionTarget" || conds[0].(map[string]any)["status"] != "True" {
		t.Errorf("%s: podFailurePolicy rule = %v, want Ignore on DisruptionTarget status True", what, rule)
	}
}

func restartPolicyOf(job map[string]any) string {
	v, _ := dig(job, "template", "spec", "template", "spec")["restartPolicy"].(string)
	return v
}

func dig(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, _ := m[k].(map[string]any)
		if next == nil {
			return map[string]any{}
		}
		m = next
	}
	return m
}

// buildForRestartPolicy renders an entry on the first GPU shape it accepts;
// some NCCL entries exist only for one architecture.
func buildForRestartPolicy(t *testing.T, c CategoryInfo) nvcrev1alpha1.WorkflowSpec {
	t.Helper()
	entry := Lookup(c.Domain, c.Variant)
	shapes := []struct {
		product string
		gpus    int32
	}{
		{"NVIDIA-H100-80GB-HBM3", 8},
		{"NVIDIA-GB300", 4},
		{"NVIDIA-B300-SXM6-AC", 8},
	}
	var lastErr error
	for _, s := range shapes {
		selector := map[string]string{gpu.ProductLabel: s.product}
		for _, nodes := range []int32{2, 1} {
			spec, err := entry.Build(nvcrev1alpha1.TargetSpec{NodeSelector: selector}, BuildConfig{
				NodesPerJob:     nodes,
				GpusPerNode:     s.gpus,
				GPUArchitecture: GPUArchFromNodeSelector(selector),
			})
			if err == nil {
				return spec
			}
			lastErr = err
		}
	}
	t.Fatalf("%s/%s: no GPU shape built: %v", c.Domain, c.Variant, lastErr)
	return nvcrev1alpha1.WorkflowSpec{}
}

// replicatedJobsOf returns the replicatedJobs of the entry's runtime by name;
// only a TrainingRuntime dependency has spec.template.spec.replicatedJobs.
func replicatedJobsOf(t *testing.T, spec nvcrev1alpha1.WorkflowSpec) map[string]map[string]any {
	t.Helper()
	jobs := map[string]map[string]any{}
	for _, dep := range spec.Dependencies {
		var obj map[string]any
		if err := json.Unmarshal(dep.Raw, &obj); err != nil {
			t.Fatalf("unmarshal dependency: %v", err)
		}
		list, _ := dig(obj, "spec", "template", "spec")["replicatedJobs"].([]any)
		for _, j := range list {
			job, _ := j.(map[string]any)
			name, _ := job["name"].(string)
			jobs[name] = job
		}
	}
	return jobs
}
