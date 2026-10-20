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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

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
// server. The resources cases check that a negative or malformed quantity
// string is rejected at admission by pattern, and that an override whose
// request exceeds its matching limit is accepted there: no CEL rule may read a
// quantity, because Kubernetes 1.29 through 1.33 refuse the CRD over it, so
// the controller rejects the pair instead (ADR-101, issue #83). The maxBytes
// cases check that documented values pass the pattern and invalid units are
// rejected (issue #283). The gangScheduler cases check that the field stays
// optional, that a scheduler name is mandatory once the field is set, and that
// the queue is held to the Kubernetes label value rules before it is copied
// into the queue label (issue #300). The queueLabelKey cases check that the
// key is held to the Kubernetes label key rules: the qualified-name pattern
// rejects a bad name segment, the CEL rule rejects a prefix over 253
// characters, which the pattern alone cannot count, and an explicit empty
// string is accepted, since the code resolves it to the default key the same
// way an empty queue resolves to default-queue (ADR-076). The nicResourceName
// cases check that both CRDs hold the field to the extended resource name
// rules — the pattern caps the name segment after the slash at 63 characters
// and the CEL rule rejects the reserved kubernetes.io and k8s.io domains
// (including subdomains) — at every level the field appears (ADR-075). The
// dcgmHost cases check that the pattern holds the port to 1-65535 rather than
// to a digit count and rejects empty DNS labels, at both levels (issue #428).
func TestCertificationValidation(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{nvcreCRDDirectory}
	suite.Environment.ErrorIfCRDPathMissing = true
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	parser := &testutil.TestCaseParser{
		Subdir:         "validation",
		ExpectedSuffix: ".json",
	}

	parser.TestDir(t, func(tc *testutil.TestCase) error {
		ctx := context.Background()

		obj, err := validationInput(tc)
		if err != nil {
			return err
		}

		result := validationResult{Accepted: true}
		if createErr := suite.Client.Create(ctx, obj); createErr != nil {
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
		} else if delErr := suite.Client.Delete(ctx, obj); delErr != nil {
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

// rawInputFile names a validation case input that is submitted exactly as
// written. input.yaml goes through the typed Certification first, which turns
// a bare integer quantity into a string and refuses a quantity Go cannot
// parse before it ever reaches the API server; the cases that test what
// admission alone stops need neither to happen (ADR-101).
const rawInputFile = "input_raw.yaml"

// validationInput returns the one object a validation case submits: the
// document in input_raw.yaml as unstructured, or else the typed object
// decoded from input.yaml.
func validationInput(tc *testutil.TestCase) (client.Object, error) {
	if raw, ok := tc.Inputs[rawInputFile]; ok {
		obj := &unstructured.Unstructured{}
		if err := sigsyaml.Unmarshal([]byte(raw), &obj.Object); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", rawInputFile, err)
		}
		return obj, nil
	}
	objs, _, err := tc.GetObjects(scheme.Scheme)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("expected exactly one object in input.yaml, got %d", len(objs))
	}
	return objs[0], nil
}
