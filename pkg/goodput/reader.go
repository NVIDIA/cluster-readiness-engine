// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import (
	"context"
	"fmt"
	"sync"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
)

// LogReader reads logs from Kubernetes pods and parses them using a ProfileParser.
type LogReader struct {
	fetcher podlogs.PodLogFetcher
	parser  *ProfileParser
}

// NewLogReader creates a new LogReader with the given PodLogFetcher and parser.
func NewLogReader(fetcher podlogs.PodLogFetcher, parser *ProfileParser) *LogReader {
	return &LogReader{
		fetcher: fetcher,
		parser:  parser,
	}
}

// ReadLogs reads and parses logs from a single pod.
func (r *LogReader) ReadLogs(ctx context.Context, namespace, podName string, opts podlogs.LogOptions) (*ParseResult, error) {
	lines, err := r.fetcher.FetchLogs(ctx, namespace, podName, opts)
	if err != nil {
		return nil, fmt.Errorf("reading logs from pod %s/%s: %w", namespace, podName, err)
	}
	return r.parser.ParseLogs(lines), nil
}

// ReadMultiWorkerLogs reads from two pods in parallel and merges results.
// worker0Pod provides lifecycle data (application start, checkpoint restore),
// lastWorkerPod provides training step data (steps, checkpoints).
// If both fail, the worker0 error is returned. If one fails, the other's result is returned.
func (r *LogReader) ReadMultiWorkerLogs(ctx context.Context, namespace, worker0Pod, lastWorkerPod string, opts podlogs.LogOptions) (*ParseResult, error) {
	type podResult struct {
		result *ParseResult
		err    error
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var worker0Res, lastWorkerRes podResult

	go func() {
		defer wg.Done()
		res, err := r.ReadLogs(ctx, namespace, worker0Pod, opts)
		worker0Res = podResult{result: res, err: err}
	}()

	go func() {
		defer wg.Done()
		res, err := r.ReadLogs(ctx, namespace, lastWorkerPod, opts)
		lastWorkerRes = podResult{result: res, err: err}
	}()

	wg.Wait()

	// If both failed, return the worker0 error.
	if worker0Res.err != nil && lastWorkerRes.err != nil {
		return nil, fmt.Errorf("both workers failed: worker0: %w", worker0Res.err)
	}

	// If worker0 failed, return lastWorker result only.
	if worker0Res.err != nil {
		return lastWorkerRes.result, nil
	}

	// If lastWorker failed, return worker0 result only.
	if lastWorkerRes.err != nil {
		return worker0Res.result, nil
	}

	// Both succeeded: merge lifecycle from worker0 + steps from lastWorker.
	merged := &ParseResult{
		// Lifecycle data from worker0 (app start, checkpoints, restore).
		ApplicationStartTime: worker0Res.result.ApplicationStartTime,
		CheckpointRestore:    worker0Res.result.CheckpointRestore,
		LastCheckpoint:       worker0Res.result.LastCheckpoint,
		Checkpoints:          worker0Res.result.Checkpoints,
		PendingSave:          worker0Res.result.PendingSave,

		// Training step data from lastWorker.
		FirstStep:        lastWorkerRes.result.FirstStep,
		LastStep:         lastWorkerRes.result.LastStep,
		Steps:            lastWorkerRes.result.Steps,
		LastLogTimestamp: lastWorkerRes.result.LastLogTimestamp,
	}

	// If lastWorker did not have a last log timestamp, fall back to worker0.
	if merged.LastLogTimestamp.IsZero() {
		merged.LastLogTimestamp = worker0Res.result.LastLogTimestamp
	}

	return merged, nil
}
