// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The Helm chart NVCRE installs and the Go module it compiles against are pinned
// in two different files, so they can drift apart without anything failing.
// They did: the chart constant said v2.2.0 while go.mod said v2.2.1, from the
// initial commit onwards. This test ties them together.
func TestTrainerChartVersionMatchesGoModule(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	require.NoError(t, err, "go.mod must be readable from pkg/setup")

	m := regexp.MustCompile(`(?m)^\s*github\.com/kubeflow/trainer/v2\s+(v\S+)`).FindSubmatch(data)
	require.NotNil(t, m, "could not find github.com/kubeflow/trainer/v2 in go.mod")

	require.Equal(t, string(m[1]), kubeflowTrainerVersion,
		"kubeflowTrainerVersion must match the kubeflow/trainer module in go.mod; "+
			"bump both together, and remember Trainer v2.3 is a required migration hop")
}
