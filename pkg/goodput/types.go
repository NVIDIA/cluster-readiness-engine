// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package goodput provides types and utilities for computing runtime goodput
// of training jobs by parsing pod logs and tracking interruption events.
package goodput

import (
	"sync"
	"time"
)

// TrainingStepInfo represents a training step extracted from logs.
type TrainingStepInfo struct {
	GlobalStep  int       `json:"globalStep"`
	Epoch       int       `json:"epoch"`
	Iteration   int       `json:"iteration"`
	Timestamp   time.Time `json:"timestamp"`
	StepTiming  float64   `json:"stepTiming,omitempty"` // seconds (normalized)
	Loss        float64   `json:"loss,omitempty"`
	TFLOPS      float64   `json:"tflops,omitempty"`
	ElapsedTime float64   `json:"elapsedTime,omitempty"` // seconds (normalized)
	IsWarmup    bool      `json:"isWarmup,omitempty"`
}

// CheckpointInfo represents a detected checkpoint from logs.
type CheckpointInfo struct {
	Step         int       `json:"step"`
	Timestamp    time.Time `json:"timestamp"`
	Path         string    `json:"path,omitempty"`
	SaveDuration float64   `json:"saveDuration,omitempty"` // seconds (normalized)
}

// CheckpointRestoreInfo represents a detected checkpoint restore from logs.
type CheckpointRestoreInfo struct {
	Path      string    `json:"path"`
	Timestamp time.Time `json:"timestamp"`
	Step      int       `json:"step,omitempty"`
}

// InterruptionEvent represents a single interruption (failure/preemption) event.
type InterruptionEvent struct {
	// Timestamps
	TCheckpoint time.Time `json:"tCheckpoint"`
	TInterrupt  time.Time `json:"tInterrupt"`
	TScheduled  time.Time `json:"tScheduled,omitempty"`
	TResumed    time.Time `json:"tResumed,omitempty"`

	// Durations (in seconds)
	TCh float64 `json:"tCh"` // t_interrupt - t_checkpoint (lost work)
	TRe float64 `json:"tRe"` // t_scheduled - t_interrupt (schedule + startup)
	TRm float64 `json:"tRm"` // t_resumed - t_scheduled (checkpoint load + resume)

	// Context
	Reason         string `json:"reason"`
	CheckpointStep int    `json:"checkpointStep"`
	LastStep       int    `json:"lastStep"`
}

// CumulativeMetrics represents consolidated goodput metrics for a job.
type CumulativeMetrics struct {
	// Current status
	Status string `json:"status"` // Training, PendingRestart, Succeeded, Failed

	// Training progress
	CurrentStep        int       `json:"currentStep"`
	HighestStep        int       `json:"highestStep"`
	LastCheckpointStep int       `json:"lastCheckpointStep"`
	LastCheckpointTime time.Time `json:"lastCheckpointTime,omitempty"`

	// Consolidated timing metrics (in seconds)
	TrainingTime       float64 `json:"trainingTime"`       // t_w
	LostWorkTime       float64 `json:"lostWorkTime"`       // sum of t_ch
	RescheduleTime     float64 `json:"rescheduleTime"`     // sum of t_re
	ResumeTime         float64 `json:"resumeTime"`         // sum of t_rm
	CheckpointSaveTime float64 `json:"checkpointSaveTime"` // sum of t_save

	// Checkpoint overhead details
	CheckpointCount        int     `json:"checkpointCount"`
	LastCheckpointDuration float64 `json:"lastCheckpointDuration,omitempty"`
	AvgCheckpointDuration  float64 `json:"avgCheckpointDuration,omitempty"`

	// Goodput
	Goodput float64 `json:"goodput"` // 0.0 to 1.0

	// Interruption tracking
	InterruptionCount   int                 `json:"interruptionCount"`
	Interruptions       []InterruptionEvent `json:"interruptions,omitempty"`
	PendingInterruption *InterruptionEvent  `json:"pendingInterruption,omitempty"`

	// Tracking metadata
	TrainingStartTime time.Time `json:"trainingStartTime,omitempty"`
	LastUpdated       time.Time `json:"lastUpdated"`
}

// JobState represents the in-memory state of a tracked job.
//
// The embedded Mutex guards every field below it. Callers that obtain a JobState
// from a shared cache (see GoodputMeasurementReconciler.getOrCreateState) must
// hold it for the duration of a read-modify-write cycle: the reconciler's own
// mutex guards only the map, not the values it hands out, so concurrent
// reconciles of the same measurement would otherwise race here.
//
// JobState must not be copied once in use — `go vet`'s copylocks check enforces
// this.
type JobState struct {
	sync.Mutex

	JobName   string
	Namespace string
	JobUID    string

	Phase     string
	StartTime time.Time

	PendingInterruption *InterruptionEvent

	LastCheckpointStep        int
	LastCheckpointTime        time.Time
	LastCountedCheckpointStep int
	LastKnownStep             int
	HighestStep               int
	LastNonWarmupStep         int
	WarmupBaseStep            int
	LastLogFetch              time.Time
	ApplicationStopTime       time.Time

	// PendingCheckpointSave persists a save start across samples so that
	// checkpoint save duration can be computed when the done line arrives
	// in a later tail window.
	PendingCheckpointSave *PendingCheckpointSave

	TrainingStarted      bool
	ApplicationStartTime time.Time
}

// ParseResult contains all parsed information from a log stream.
type ParseResult struct {
	// ApplicationStartTime is the first framework log marker timestamp.
	ApplicationStartTime time.Time

	// FirstStep is the first training step seen in the log window.
	FirstStep *TrainingStepInfo

	// LastStep is the last training step seen in the log window.
	LastStep *TrainingStepInfo

	// LastCheckpoint is the last checkpoint saved.
	LastCheckpoint *CheckpointInfo

	// CheckpointRestore is the detected checkpoint restore (if any).
	CheckpointRestore *CheckpointRestoreInfo

	// Steps contains the training steps (limited to recent ones).
	Steps []*TrainingStepInfo

	// Checkpoints contains all detected checkpoints.
	Checkpoints []*CheckpointInfo

	// LastLogTimestamp is the timestamp of the last log line (any format).
	LastLogTimestamp time.Time

	// LogInterval is the training framework's log interval from the LogProfile.
	// Used as the default step delta when no prior step exists in the window.
	LogInterval int

	// PendingSave is set when a checkpoint save line was seen but no corresponding
	// done line followed in the same log window. The controller persists this
	// across samples so the duration can be computed when the done line appears.
	PendingSave *PendingCheckpointSave
}

// PendingCheckpointSave represents a checkpoint save start that hasn't been
// completed yet (no done line seen in the same log window).
type PendingCheckpointSave struct {
	Step      int
	Timestamp time.Time
	Path      string
}
