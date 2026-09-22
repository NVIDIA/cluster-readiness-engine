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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// transitionResult is the golden-file shape for an update-transition case: how
// the API server answered the create, and then the update applied on top of it.
type transitionResult struct {
	Create validationResult `json:"create"`
	Update validationResult `json:"update"`
}

// TestUpdateTransitionValidation exercises the CRD transition rules that
// ordinary create-time validation cannot reach, against a real API server.
//
// The rules under test are the pair ADR-079 prescribes for an optional
// immutable field: a field-level `self == oldSelf` plus a parent-level
// `has(self.f) == has(oldSelf.f)`. The field-level rule alone does not run
// when an optional field is added or removed, so without the parent rule a
// Job could drop `workloadMetadata` and re-add it under a different queue —
// which is exactly the update these cases attempt.
//
// Each case creates `input_create.yaml`, then applies `input_update.yaml` on
// top of the created object, so the golden records both answers. Cases cover
// `spec.workloadMetadata` on a direct Job, the same field propagated through
// `JobTemplateSpec.Spec` into a Workflow, and `spec.gangScheduler` on a
// WorkflowSpec. Objects whose stored spec omits the new field are covered too:
// they must stay without it.
//
// These have to run against the generated CRDs and the API server rather than
// a Go validator, because the rules being checked are CEL in the schema and
// nothing else evaluates them.
func TestUpdateTransitionValidation(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{nvcreCRDDirectory}
	suite.Environment.ErrorIfCRDPathMissing = true
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	parser := &testutil.TestCaseParser{
		Subdir:         "immutability",
		ExpectedSuffix: testutil.SuffixJSON,
	}

	parser.TestDir(t, func(tc *testutil.TestCase) error {
		ctx := context.Background()

		created, err := decodeSingle(tc, "input_create.yaml")
		if err != nil {
			return err
		}
		updated, err := decodeSingle(tc, "input_update.yaml")
		if err != nil {
			return err
		}

		result := transitionResult{}
		result.Create = applyAndRecord(ctx, suite.Client, created, false)
		if !result.Create.Accepted {
			return fmt.Errorf("case setup is wrong: the create was rejected, so the update proves nothing")
		}
		// Carry the server-assigned resourceVersion onto the update so it is a
		// genuine update of the stored object rather than a conflict.
		updated.SetResourceVersion(created.GetResourceVersion())
		result.Update = applyAndRecord(ctx, suite.Client, updated, true)

		if delErr := suite.Client.Delete(ctx, created); delErr != nil {
			return delErr
		}

		b, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// decodeSingle pulls exactly one object out of the named input file.
func decodeSingle(tc *testutil.TestCase, name string) (client.Object, error) {
	content, ok := tc.Inputs[name]
	if !ok {
		return nil, fmt.Errorf("missing %s", name)
	}
	single := &testutil.TestCase{Inputs: map[string]string{name: content}}
	objs, _, err := single.GetObjects(scheme.Scheme)
	if err != nil {
		return nil, err
	}
	if len(objs) != 1 {
		return nil, fmt.Errorf("expected exactly one object in %s, got %d", name, len(objs))
	}
	return objs[0], nil
}

// applyAndRecord creates or updates obj and records the API server's answer in
// the same shape the create-only validation cases use, so both suites read
// alike.
func applyAndRecord(
	ctx context.Context, c client.Client, obj client.Object, update bool,
) validationResult {
	var err error
	if update {
		err = c.Update(ctx, obj)
	} else {
		err = c.Create(ctx, obj)
	}
	if err == nil {
		return validationResult{Accepted: true}
	}

	result := validationResult{Accepted: false}
	status, ok := err.(apierrors.APIStatus)
	if !ok {
		return validationResult{
			Accepted: false,
			Causes: []validationCause{{
				Type:    "Unexpected",
				Message: err.Error(),
			}},
		}
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
	return result
}
