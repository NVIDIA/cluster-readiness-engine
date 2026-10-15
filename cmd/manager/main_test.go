// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
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

		result := struct {
			Valid bool `json:"valid"`
		}{Valid: true}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

func TestControllerConcurrencyFlagDefaults(t *testing.T) {
	cmd := newRootCommand()
	p := testutil.TestCaseParser{
		Subdir:         "concurrency-flag-defaults",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
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

func TestValidateNamespaceAccessTimeout(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "namespace-access-timeout-validate",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Timeout string `json:"timeout"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		d, err := time.ParseDuration(input.Timeout)
		if err != nil {
			return err
		}
		if err := validateNamespaceAccessTimeout(d); err != nil {
			return err
		}
		data, err := json.MarshalIndent(map[string]bool{"valid": true}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestControllerServiceAccount pins how the manager learns the ServiceAccount
// it names in the RoleBinding a waiting Workflow asks for: the chart sets both
// variables from the downward API, and a manager run outside the chart falls
// back to the one `nvcrectl setup init` installs.
func TestControllerServiceAccount(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "controller-service-account",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Env map[string]string `json:"env"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		sa := controllerServiceAccount(func(key string) string { return input.Env[key] })
		data, err := json.MarshalIndent(map[string]string{
			"namespace": sa.Namespace,
			"name":      sa.Name,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestValidateWorkloadCreationDeadline pins the --workload-creation-deadline
// bounds (issue #488): zero or negative would fail every rejected create on
// its first attempt.
func TestValidateWorkloadCreationDeadline(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "workload-creation-deadline-validate",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Deadline string `json:"deadline"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		d, err := time.ParseDuration(input.Deadline)
		if err != nil {
			return err
		}
		if err := validateWorkloadCreationDeadline(d); err != nil {
			return err
		}
		data, err := json.MarshalIndent(map[string]bool{"valid": true}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
