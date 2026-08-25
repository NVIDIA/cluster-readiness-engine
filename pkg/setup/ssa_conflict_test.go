// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestClassifierMatchesRealSSAConflict pins the conflict classifier to real
// apiserver-generated conflict wording (ADR-073). envtest runs a real
// kube-apiserver, so real server-side-apply field ownership is available:
// the test applies a webhook-shaped Secret as field manager "helm"
// (simulating the first install), force-applies different cert bytes as
// "trainer-controller" (the ownership takeover observed in the field), then
// applies again as "helm" without force and captures the 409 conflict
// listing the .data fields. The captured text, wrapped in Helm's error
// framing, is fed through classifyTrainerInstallFailure and golden-asserted.
//
// The test needs KUBEBUILDER_ASSETS (as cmd/integration does); `make test`
// exports it for the unit run too. The test skips when it is unset so a
// plain `go test ./pkg/setup/` still passes without envtest binaries.
func TestClassifierMatchesRealSSAConflict(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; skipping envtest SSA conflict fixture")
	}

	suite := &testutil.IntegrationTestSuite{}
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)

	ctx := context.Background()
	c := suite.Client

	require.NoError(t, c.Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: trainerNamespace}}))

	const secretName = "kubeflow-trainer-webhook-cert"
	webhookSecret := func(crt, key string) *corev1ac.SecretApplyConfiguration {
		return corev1ac.Secret(secretName, trainerNamespace).
			WithType(corev1.SecretTypeOpaque).
			WithData(map[string][]byte{
				"tls.crt": []byte(crt),
				"tls.key": []byte(key),
			})
	}

	// 1. The first install: helm owns the freshly rendered cert data.
	require.NoError(t, c.Apply(ctx, webhookSecret("cert-render-1", "key-render-1"),
		client.FieldOwner("helm")))

	// 2. The ownership takeover observed in the field: another manager
	// force-applies different cert bytes and takes the .data fields.
	require.NoError(t, c.Apply(ctx, webhookSecret("cert-rotated", "key-rotated"),
		client.FieldOwner("trainer-controller"), client.ForceOwnership))

	// 3. The retry: the chart re-renders fresh cert bytes and helm applies
	// without force. The apiserver must answer 409 listing the .data fields.
	conflictErr := c.Apply(ctx, webhookSecret("cert-render-2", "key-render-2"),
		client.FieldOwner("helm"))
	require.Error(t, conflictErr)
	require.True(t, apierrors.IsConflict(conflictErr), "want a 409 conflict, got: %v", conflictErr)

	// 4. Wrap the real conflict text in Helm's error framing (matched
	// loosely by the classifier) and golden-assert the classification and
	// the recovery decision.
	framed := fmt.Sprintf("Error: UPGRADE FAILED: cannot patch Secret %q in namespace %s: %s",
		secretName, trainerNamespace, conflictErr.Error())

	p := testutil.TestCaseParser{
		Subdir:         "ssa-conflict",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		paths := regexp.MustCompile(`\.data\.[^\s,:]+`).FindAllString(conflictErr.Error(), -1)
		sort.Strings(paths)

		classified := classifyTrainerInstallFailure(framed) == failureClassSSAConflict
		decision := "fail fast with the raw helm output"
		if classified && helmStateFailedOrPending("failed") {
			decision = "automatic recovery arm (safety gate still applies)"
		}
		tc.Actual = fmt.Sprintf(
			"classifiedAsSSAConflict: %v\nconflictPaths: %s\nrecoveryDecision(releaseState=failed): %s\n",
			classified, strings.Join(paths, " "), decision)
		return nil
	})
}
