// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"
)

func TestControllerConcurrencyOptionsValidate(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "concurrency-validate",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			MaxConcurrentReconciles            int `json:"maxConcurrentReconciles"`
			MeasurementMaxConcurrentReconciles int `json:"measurementMaxConcurrentReconciles"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		opts := controllerConcurrencyOptions{
			maxConcurrentReconciles:            input.MaxConcurrentReconciles,
			measurementMaxConcurrentReconciles: input.MeasurementMaxConcurrentReconciles,
		}
		if err := opts.validate(); err != nil {
			return err
		}

		data, err := json.MarshalIndent(input, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestControllerConcurrencyFlagDefaults(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "concurrency-flag-defaults",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		cmd := newRootCommand()
		defaults := map[string]string{}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			// Go's test harness registers test.* flags on flag.CommandLine.
			// They are not part of the manager CLI and vary across Go versions.
			if !strings.HasPrefix(f.Name, "test.") {
				defaults[f.Name] = f.DefValue
			}
		})

		names := make([]string, 0, len(defaults))
		for name := range defaults {
			names = append(names, name)
		}
		sort.Strings(names)

		type flagDefault struct {
			Name    string `json:"name"`
			Default string `json:"default"`
		}
		result := make([]flagDefault, 0, len(names))
		for _, name := range names {
			result = append(result, flagDefault{Name: name, Default: defaults[name]})
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
