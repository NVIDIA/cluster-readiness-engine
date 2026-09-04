// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
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
