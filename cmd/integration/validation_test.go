// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// validationResult is the golden-file shape for CRD admission test cases:
// whether the API server accepted the object and, when it rejected it, the
// field-level causes returned by schema validation.
type validationResult struct {
	Accepted bool              `json:"accepted"`
	Causes   []validationCause `json:"causes,omitempty"`
}

type validationCause struct {
	Type    string `json:"type"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// TestCertificationValidation exercises the schema validation rules on the
// Certification CRD (CEL rules and OpenAPI patterns) against a real API
// server. The resources cases check that an override whose request exceeds
// its matching limit is rejected at admission while partial and correctly
// ordered overrides are accepted (issue #83). The maxBytes cases check that
// documented values pass the pattern and invalid units are rejected (issue
// #283). The gangScheduler cases check that the field stays optional, that a
// scheduler name is mandatory once the field is set, and that the queue is
// held to the Kubernetes label value rules before it is copied into the
// kai.scheduler/queue label (issue #300).
func TestCertificationValidation(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{"../../helm/cluster-readiness-engine/crds"}
	suite.Environment.ErrorIfCRDPathMissing = true
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	parser := &testutil.TestCaseParser{
		Subdir:         "validation",
		ExpectedSuffix: ".json",
	}

	parser.TestDir(t, func(tc *testutil.TestCase) error {
		ctx := context.Background()

		objs, _, err := tc.GetObjects(scheme.Scheme)
		if err != nil {
			return err
		}
		if len(objs) != 1 {
			return fmt.Errorf("expected exactly one object in input.yaml, got %d", len(objs))
		}

		result := validationResult{Accepted: true}
		if createErr := suite.Client.Create(ctx, objs[0]); createErr != nil {
			if !apierrors.IsInvalid(createErr) {
				return createErr
			}
			result.Accepted = false
			status, ok := createErr.(apierrors.APIStatus)
			if !ok {
				return fmt.Errorf("invalid error does not expose a Status: %w", createErr)
			}
			if details := status.Status().Details; details != nil {
				for _, cause := range details.Causes {
					result.Causes = append(result.Causes, validationCause{
						Type:    string(cause.Type),
						Field:   cause.Field,
						Message: cause.Message,
					})
				}
			}
			// The API server reports CEL violations for sibling fields in
			// nondeterministic order; sort so goldens are stable.
			sort.Slice(result.Causes, func(i, j int) bool {
				if result.Causes[i].Field != result.Causes[j].Field {
					return result.Causes[i].Field < result.Causes[j].Field
				}
				if result.Causes[i].Message != result.Causes[j].Message {
					return result.Causes[i].Message < result.Causes[j].Message
				}
				return result.Causes[i].Type < result.Causes[j].Type
			})
		} else if delErr := suite.Client.Delete(ctx, objs[0]); delErr != nil {
			// Accepted: remove the object so cases stay independent.
			return delErr
		}

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}
