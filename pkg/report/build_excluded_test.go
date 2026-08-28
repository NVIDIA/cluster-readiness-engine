// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A run that excluded nodes reports INCOMPLETE, not PASSED: it did not certify
// what was asked, but the skipped nodes were never tested, so FAILED would
// assert a fault nobody observed. This drives the whole path: real
// OrchestrationStatus on a Workflow, through Build, to the rendered report. The
// golden is the rendered text, because that is what an operator actually sees.
func TestBuildExcludedReport(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-excluded-report",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return err
		}
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}

		builder := fake.NewClientBuilder().WithScheme(scheme)
		var cert *nvcrev1alpha1.Certification
		for _, o := range objs {
			builder = builder.WithObjects(o)
			if c, ok := o.(*nvcrev1alpha1.Certification); ok {
				cert = c
			}
		}
		c := builder.Build()

		report := Build(context.Background(), c, cert)

		var buf bytes.Buffer
		Print(&buf, report)
		tc.Actual = buf.String()
		return nil
	})
}
