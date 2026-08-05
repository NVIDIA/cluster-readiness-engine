// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import "time"

// CalculateGoodput computes the runtime goodput from timing values.
// Formula: Runtime Goodput = (t_w - t_c - t_re - t_save) / (t_w - t_re)
// Where t_c = sum(t_rm + t_ch), t_save = checkpoint save overhead
func CalculateGoodput(tW, tCh, tRe, tRm, tSave float64) float64 {
	tc := tRm + tCh // Cost of interruptions (badput)
	denominator := tW - tRe
	if denominator <= 0 {
		return 0
	}
	numerator := tW - tc - tRe - tSave
	if numerator < 0 {
		return 0
	}
	return numerator / denominator
}

// NewCumulativeMetrics creates a new CumulativeMetrics.
func NewCumulativeMetrics() *CumulativeMetrics {
	return &CumulativeMetrics{
		Status:        "Initializing",
		Interruptions: make([]InterruptionEvent, 0),
		LastUpdated:   time.Now(),
	}
}

// UpdateTrainingProgress updates training state from log parsing.
func (c *CumulativeMetrics) UpdateTrainingProgress(currentStep, checkpointStep int, checkpointTime time.Time) {
	c.CurrentStep = currentStep
	if currentStep > c.HighestStep {
		c.HighestStep = currentStep
	}
	if checkpointStep > 0 {
		c.LastCheckpointStep = checkpointStep
		c.LastCheckpointTime = checkpointTime
	}
	c.LastUpdated = time.Now()
}

// UpdateTrainingTime updates the training time (t_w) and recalculates goodput.
func (c *CumulativeMetrics) UpdateTrainingTime(trainingTime float64) {
	c.TrainingTime = trainingTime
	c.recalculateGoodput()
	c.LastUpdated = time.Now()
}

// recalculateGoodput recomputes goodput from current values.
func (c *CumulativeMetrics) recalculateGoodput() {
	c.Goodput = CalculateGoodput(c.TrainingTime, c.LostWorkTime, c.RescheduleTime, c.ResumeTime, c.CheckpointSaveTime)
}

// SetStatus updates the status.
func (c *CumulativeMetrics) SetStatus(status string) {
	c.Status = status
	c.LastUpdated = time.Now()
}
