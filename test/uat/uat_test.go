// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

package uat

import (
	"os"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/env"
)

var testenv env.Environment

type contextKey string

const nsKey contextKey = "namespace"

func TestMain(m *testing.M) {
	testenv = env.New()
	os.Exit(testenv.Run(m))
}
