// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testSchedulerName = "kai-scheduler"
	testDefaultQueue  = "default-queue"
	testMPIQueue      = "mpi-queue"
)

// podSchedulerName extracts spec.template.spec.replicatedJobs[i].template.spec.template.spec.schedulerName
// from a marshalled TrainingRuntime.
func podSchedulerName(t *testing.T, raw []byte, replicatedJobIdx int) string {
	t.Helper()
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	jobs := rt["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["replicatedJobs"].([]any)
	podSpec := jobs[replicatedJobIdx].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	v, _ := podSpec["schedulerName"].(string)
	return v
}

// podQueueLabel extracts spec.template.spec.replicatedJobs[i].template.metadata.labels["kai.scheduler/queue"].
func podQueueLabel(t *testing.T, raw []byte, replicatedJobIdx int) string {
	t.Helper()
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	jobs := rt["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["replicatedJobs"].([]any)
	meta, ok := jobs[replicatedJobIdx].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	labels, ok := meta["labels"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := labels["kai.scheduler/queue"].(string)
	return v
}

func baseRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		EntryName:   "test-run",
		Image:       "nvcr.io/test:latest",
		NodesPerJob: 2,
		GpusPerNode: 8,
	}
}

func TestGangSchedulerQueue_Default(t *testing.T) {
	cfg := RuntimeConfig{}
	if got := gangSchedulerQueue(cfg.GangSchedulerQueue); got != testDefaultQueue {
		t.Errorf("got %q, want %q", got, testDefaultQueue)
	}
}

func TestGangSchedulerQueue_Custom(t *testing.T) {
	cfg := RuntimeConfig{GangSchedulerQueue: "my-queue"}
	if got := gangSchedulerQueue(cfg.GangSchedulerQueue); got != "my-queue" {
		t.Errorf("got %q, want %q", got, "my-queue")
	}
}

func TestBuildTorchRuntime_NoGangScheduler(t *testing.T) {
	dep := BuildTorchRuntime(baseRuntimeConfig())
	if podSchedulerName(t, dep.Raw, 0) != "" {
		t.Error("expected no schedulerName when GangSchedulerName is empty")
	}
	if podQueueLabel(t, dep.Raw, 0) != "" {
		t.Error("expected no queue label when GangSchedulerName is empty")
	}
}

func TestBuildTorchRuntime_WithGangScheduler(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = testSchedulerName

	dep := BuildTorchRuntime(cfg)

	if got := podSchedulerName(t, dep.Raw, 0); got != testSchedulerName {
		t.Errorf("schedulerName = %q, want %q", got, testSchedulerName)
	}
	if got := podQueueLabel(t, dep.Raw, 0); got != testDefaultQueue {
		t.Errorf("queue label = %q, want %q", got, testDefaultQueue)
	}
}

func TestBuildTorchRuntime_WithGangSchedulerAndCustomQueue(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = testSchedulerName
	cfg.GangSchedulerQueue = "gpu-team-queue"

	dep := BuildTorchRuntime(cfg)

	if got := podSchedulerName(t, dep.Raw, 0); got != testSchedulerName {
		t.Errorf("schedulerName = %q, want %q", got, testSchedulerName)
	}
	if got := podQueueLabel(t, dep.Raw, 0); got != "gpu-team-queue" {
		t.Errorf("queue label = %q, want %q", got, "gpu-team-queue")
	}
}

func TestBuildMPIRuntime_NoGangScheduler(t *testing.T) {
	dep := BuildMPIRuntime(baseRuntimeConfig())
	// worker is index 0, launcher is index 1
	if podSchedulerName(t, dep.Raw, 0) != "" {
		t.Error("worker: expected no schedulerName when GangSchedulerName is empty")
	}
	if podSchedulerName(t, dep.Raw, 1) != "" {
		t.Error("launcher: expected no schedulerName when GangSchedulerName is empty")
	}
}

func TestBuildMPIRuntime_WithGangScheduler(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = testSchedulerName
	cfg.GangSchedulerQueue = testMPIQueue

	dep := BuildMPIRuntime(cfg)

	// worker (index 0)
	if got := podSchedulerName(t, dep.Raw, 0); got != testSchedulerName {
		t.Errorf("worker schedulerName = %q, want %q", got, testSchedulerName)
	}
	if got := podQueueLabel(t, dep.Raw, 0); got != testMPIQueue {
		t.Errorf("worker queue label = %q, want %q", got, testMPIQueue)
	}

	// launcher (index 1)
	if got := podSchedulerName(t, dep.Raw, 1); got != testSchedulerName {
		t.Errorf("launcher schedulerName = %q, want %q", got, testSchedulerName)
	}
	if got := podQueueLabel(t, dep.Raw, 1); got != testMPIQueue {
		t.Errorf("launcher queue label = %q, want %q", got, testMPIQueue)
	}
}

func TestBuildExecRuntime_WithGangScheduler(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = testSchedulerName

	dep := BuildExecRuntime(cfg)

	if got := podSchedulerName(t, dep.Raw, 0); got != testSchedulerName {
		t.Errorf("schedulerName = %q, want %q", got, testSchedulerName)
	}
}

// jobSetStartupPolicyOrder extracts
// spec.template.spec.startupPolicy.startupPolicyOrder from a marshalled
// TrainingRuntime. Empty means the JobSet template carries no startup policy.
func jobSetStartupPolicyOrder(t *testing.T, raw []byte) string {
	t.Helper()
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	spec := rt["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	policy, ok := spec["startupPolicy"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := policy["startupPolicyOrder"].(string)
	return v
}

// launcherDependsOn extracts the names the launcher replicated job (index 1)
// waits for, from spec.template.spec.replicatedJobs[1].dependsOn[].name.
func launcherDependsOn(t *testing.T, raw []byte) []string {
	t.Helper()
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	jobs := rt["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["replicatedJobs"].([]any)
	list, ok := jobs[1].(map[string]any)["dependsOn"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		if name, ok := item.(map[string]any)["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// launcherInitArg extracts the launcher replicated job's init-container command
// script, and the volume names its init container mounts.
func launcherInit(t *testing.T, raw []byte) (script string, mounts []string) {
	t.Helper()
	var rt map[string]any
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	jobs := rt["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["replicatedJobs"].([]any)
	podSpec := jobs[1].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	inits, _ := podSpec["initContainers"].([]any)
	if len(inits) == 0 {
		return "", nil
	}
	init := inits[0].(map[string]any)
	args, _ := init["args"].([]any)
	for _, a := range args {
		if s, ok := a.(string); ok {
			script += s
		}
	}
	for _, m := range init["volumeMounts"].([]any) {
		if name, ok := m.(map[string]any)["name"].(string); ok {
			mounts = append(mounts, name)
		}
	}
	return script, mounts
}

// TestBuildMPIRuntime_KAIGangScheduler_MovesTheBarrierIntoTheLauncher pins the
// contract KAI forces: no gate and no startup policy (KAI requires every
// sub-group of the PodGroup to have pods before it is schedulable, so any
// ordering that leaves the launcher sub-group empty blocks the whole gang —
// measured live on KAI v0.16.4 with both dependsOn and startupPolicy: InOrder),
// and the launcher instead waits inside its init container until every worker
// answers sshd, using Trainer's MPI hostfile for the worker list.
func TestBuildMPIRuntime_KAIGangScheduler_MovesTheBarrierIntoTheLauncher(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = testSchedulerName

	dep := BuildMPIRuntime(cfg)

	if deps := launcherDependsOn(t, dep.Raw); len(deps) != 0 {
		t.Errorf("launcher dependsOn = %v, want none under KAI", deps)
	}
	if got := jobSetStartupPolicyOrder(t, dep.Raw); got != "" {
		t.Errorf("startupPolicyOrder = %q, want empty under KAI (InOrder leaves the launcher sub-group empty)", got)
	}

	script, mounts := launcherInit(t, dep.Raw)
	if !strings.Contains(script, "ssh -n -o BatchMode=yes") {
		t.Errorf("launcher init does not probe the workers over ssh:\n%s", script)
	}
	if !strings.Contains(script, "timeout 10 ssh") {
		t.Errorf("ssh probe is not bounded by a hard timeout:\n%s", script)
	}
	if !strings.Contains(script, "no hosts in") {
		t.Errorf("launcher init does not fail when the hostfile has no hosts:\n%s", script)
	}
	// With mpi.runLauncherAsNode the hostfile also lists this pod's own endpoint; probing it
	// would deadlock against this very init container.
	if !strings.Contains(script, `case "$h" in`) || !strings.Contains(script, "$self") {
		t.Errorf("launcher init does not skip its own endpoint in the probe loop:\n%s", script)
	}
	if !strings.Contains(script, "/etc/mpi/hostfile") {
		t.Errorf("launcher init does not read Trainer's hostfile:\n%s", script)
	}
	found := false
	for _, m := range mounts {
		if m == "mpi-hostfile" {
			found = true
		}
	}
	if !found {
		t.Errorf("launcher init mounts = %v, want the Trainer hostfile volume", mounts)
	}
	// The gang injection itself stays untouched.
	if got := podSchedulerName(t, dep.Raw, 1); got != testSchedulerName {
		t.Errorf("launcher schedulerName = %q, want %q", got, testSchedulerName)
	}
}

// TestBuildMPIRuntime_NonKAIGangScheduler_KeepsDependsOn pins the gate for the
// other gang schedulers: only KAI needs the rewrite, so a Run:ai-style name must
// keep dependsOn and must not grow a startup policy.
func TestBuildMPIRuntime_NonKAIGangScheduler_KeepsDependsOn(t *testing.T) {
	cfg := baseRuntimeConfig()
	cfg.GangSchedulerName = "runai-scheduler"
	cfg.GangSchedulerQueueLabelKey = "runai/queue"

	dep := BuildMPIRuntime(cfg)

	if deps := launcherDependsOn(t, dep.Raw); len(deps) != 1 || deps[0] != "node" {
		t.Errorf("launcher dependsOn = %v, want [node]", deps)
	}
	if got := jobSetStartupPolicyOrder(t, dep.Raw); got != "" {
		t.Errorf("startupPolicyOrder = %q, want empty for a non-KAI scheduler", got)
	}
}

// TestBuildMPIRuntime_NoGangScheduler_KeepsDependsOn is the no-op guard: without
// a gang scheduler the render must stay byte-identical to the historical shape.
func TestBuildMPIRuntime_NoGangScheduler_KeepsDependsOn(t *testing.T) {
	dep := BuildMPIRuntime(baseRuntimeConfig())

	if deps := launcherDependsOn(t, dep.Raw); len(deps) != 1 || deps[0] != "node" {
		t.Errorf("launcher dependsOn = %v, want [node]", deps)
	}
	if got := jobSetStartupPolicyOrder(t, dep.Raw); got != "" {
		t.Errorf("startupPolicyOrder = %q, want empty without a gang scheduler", got)
	}
}
