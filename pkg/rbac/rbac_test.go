// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rbac

import (
	"bytes"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"
)

// TestPrintRunBinding pins the manifest users apply by hand. Its names are a
// contract with the chart: the role must match templates/run-role.yaml and the
// subject the chart's ServiceAccount.
func TestPrintRunBinding(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "print", ExpectedSuffix: ".yaml"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Namespace           string `json:"namespace"`
			ControllerNamespace string `json:"controllerNamespace"`
			ServiceAccount      string `json:"serviceAccount"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		sa := DefaultServiceAccount()
		if input.ControllerNamespace != "" {
			sa.Namespace = input.ControllerNamespace
		}
		if input.ServiceAccount != "" {
			sa.Name = input.ServiceAccount
		}
		var out bytes.Buffer
		if err := printRunBinding(&out, input.Namespace, sa); err != nil {
			return err
		}
		tc.Actual = out.String()
		return nil
	})
}

func TestGrantsRunRole(t *testing.T) {
	sa := DefaultServiceAccount()
	for _, tc := range []struct {
		name   string
		mutate func(*ServiceAccount, *string)
		want   bool
	}{
		{name: "matching binding", want: true},
		{name: "other subject", mutate: func(s *ServiceAccount, _ *string) { s.Name = "someone-else" }},
		{name: "other subject namespace", mutate: func(s *ServiceAccount, _ *string) { s.Namespace = "elsewhere" }},
		{name: "other role", mutate: func(_ *ServiceAccount, role *string) { *role = "admin" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject, role := sa, RunRoleName
			if tc.mutate != nil {
				tc.mutate(&subject, &role)
			}
			binding := RunBinding("run-ns", subject)
			binding.RoleRef.Name = role
			if got := GrantsRunRole(binding, sa); got != tc.want {
				t.Fatalf("GrantsRunRole = %v, want %v", got, tc.want)
			}
		})
	}
}
