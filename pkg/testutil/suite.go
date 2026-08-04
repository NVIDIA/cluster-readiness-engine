// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides a minimal envtest wrapper and golden-file test
// harness for integration tests, replacing the dependency on
// sigs.k8s.io/usage-metrics-collector/pkg/testutil (which dragged in
// containerd → runc as transitive dependencies).
package testutil

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// IntegrationTestSuite wraps envtest.Environment and exposes a ready-to-use
// Config and Client after SetupTestSuite is called.
//
// The client is initialised with k8s.io/client-go/kubernetes/scheme.Scheme.
// Register any additional types into that scheme in the test's init() function
// before calling SetupTestSuite.
type IntegrationTestSuite struct {
	Environment envtest.Environment
	Config      *rest.Config
	Client      client.Client
}

// SetupTestSuite starts the embedded envtest control plane and populates
// Config and Client.
func (s *IntegrationTestSuite) SetupTestSuite(t *testing.T) {
	t.Helper()
	var err error
	s.Config, err = s.Environment.Start()
	require.NoError(t, err, "starting envtest control plane")
	s.Client, err = client.New(s.Config, client.Options{Scheme: clientgoscheme.Scheme})
	require.NoError(t, err, "creating envtest client")
}

// TearDownTestSuite stops the envtest control plane.
func (s *IntegrationTestSuite) TearDownTestSuite(t *testing.T) {
	t.Helper()
	require.NoError(t, s.Environment.Stop())
}
