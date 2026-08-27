// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A threshold violation leaves the Job with Succeeded=True and
// ValidationFailed=True — no Failed condition — so a report that scans only
// Failed shows the group with an empty reason (#176, ADR-071). These cases
// drive the whole path: Certification + Workflow + Job objects on a fake
// client, through Build, to both output surfaces. The golden is the rendered
// human report followed by the results JSON, because those are the two
// artifacts an operator actually reads.
func TestBuildFailedGroups(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-failed-groups",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return err
		}
		if err := crev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}

		builder := fake.NewClientBuilder().WithScheme(scheme)
		var cert *crev1alpha1.Certification
		for _, o := range objs {
			builder = builder.WithObjects(o)
			if c, ok := o.(*crev1alpha1.Certification); ok {
				cert = c
			}
		}
		c := builder.Build()

		report := Build(context.Background(), c, cert)

		var buf bytes.Buffer
		Print(&buf, report)
		buf.WriteString("--- results.json ---\n")
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteString("\n")
		tc.Actual = buf.String()
		return nil
	})
}
