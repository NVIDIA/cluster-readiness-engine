// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package goodput

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
)

// fakeLogFetcher returns pre-loaded log lines keyed by pod name.
type fakeLogFetcher struct {
	logs map[string][]string
}

func (f *fakeLogFetcher) FetchLogs(_ context.Context, _, podName string, _ podlogs.LogOptions) ([]string, error) {
	return f.logs[podName], nil
}

func TestReadMultiWorkerLogs(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "read-multi-worker-logs",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var profile v1alpha1.LogProfile
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_profile.yaml"]), &profile); err != nil {
			return err
		}
		parser, err := NewProfileParser(&profile)
		if err != nil {
			return err
		}

		fetcher := &fakeLogFetcher{logs: make(map[string][]string)}
		for name, content := range tc.Inputs {
			if strings.HasPrefix(name, "input_logs_") && strings.HasSuffix(name, ".txt") {
				podName := strings.TrimSuffix(strings.TrimPrefix(name, "input_logs_"), ".txt")
				raw := strings.TrimRight(content, "\n")
				if raw != "" {
					fetcher.logs[podName] = strings.Split(raw, "\n")
				}
			}
		}

		reader := NewLogReader(fetcher, parser)
		result, err := reader.ReadMultiWorkerLogs(context.Background(), "default", "worker0", "lastworker", podlogs.LogOptions{})
		if err != nil {
			return err
		}

		output := toTestOutput(result)
		b, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}
