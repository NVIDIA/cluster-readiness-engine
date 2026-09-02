// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// retainedTestConfig is the input_config.yaml shape for the
// print-retained-resources golden tests.
type retainedTestConfig struct {
	// SkipPhases mirrors the --skip-phases flag value passed to reset.
	SkipPhases string `json:"skipPhases"`
	// Forbidden makes every Get fail with a Forbidden error, the way an API
	// server does when the user may not read the resource.
	Forbidden bool `json:"forbidden"`
}

func TestPrintRetainedResources(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "print-retained-resources",
		ExpectedSuffix: testutil.SuffixTXT,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg retainedTestConfig
		if raw, ok := tc.Inputs["input_config.yaml"]; ok {
			if err := sigsyaml.Unmarshal([]byte(raw), &cfg); err != nil {
				return err
			}
		}

		scheme := newSetupScheme(tc.T.(*testing.T))
		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}

		builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
		if cfg.Forbidden {
			builder = builder.WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey,
					obj client.Object, _ ...client.GetOption) error {
					return apierrors.NewForbidden(
						schema.GroupResource{}, key.Name, errors.New("denied"))
				},
			})
		}
		c := builder.Build()

		var out bytes.Buffer
		printRetainedResources(context.Background(), c, parseSkipPhases(cfg.SkipPhases), &out)
		tc.Actual = out.String()
		return nil
	})
}
