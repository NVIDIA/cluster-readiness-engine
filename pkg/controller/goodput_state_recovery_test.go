// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// getOrCreateState rebuilds the in-memory JobState from persisted status after
// a controller restart. These cases pin what a restarted controller recovers —
// in particular that LastCountedCheckpointStep mirrors LastCheckpointStep:
// every checkpoint recorded in status has already been folded into
// checkpointSaveTimeSec, so a restart-replay of a log window that still shows
// that checkpoint must not count its save time a second time.
func TestGoodputStateRecovery(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "goodput-state-recovery",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Status crev1alpha1.GoodputMeasurementStatus `yaml:"status"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		m := &crev1alpha1.GoodputMeasurement{}
		m.Name = "m"
		m.Namespace = "ns"
		m.Status = in.Status

		r := &GoodputMeasurementReconciler{}
		state := r.getOrCreateState("ns/m", m)

		b, err := json.MarshalIndent(struct {
			TrainingStarted           bool `json:"trainingStarted"`
			HighestStep               int  `json:"highestStep"`
			LastKnownStep             int  `json:"lastKnownStep"`
			LastCheckpointStep        int  `json:"lastCheckpointStep"`
			LastCountedCheckpointStep int  `json:"lastCountedCheckpointStep"`
			LastNonWarmupStep         int  `json:"lastNonWarmupStep"`
			WarmupBaseStep            int  `json:"warmupBaseStep"`
			HasPendingInterruption    bool `json:"hasPendingInterruption"`
		}{
			TrainingStarted:           state.TrainingStarted,
			HighestStep:               state.HighestStep,
			LastKnownStep:             state.LastKnownStep,
			LastCheckpointStep:        state.LastCheckpointStep,
			LastCountedCheckpointStep: state.LastCountedCheckpointStep,
			LastNonWarmupStep:         state.LastNonWarmupStep,
			WarmupBaseStep:            state.WarmupBaseStep,
			HasPendingInterruption:    state.PendingInterruption != nil,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
