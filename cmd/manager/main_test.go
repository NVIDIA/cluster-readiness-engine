// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControllerConcurrencyOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    controllerConcurrencyOptions
		wantErr string
	}{
		{
			name: "defaults",
			opts: controllerConcurrencyOptions{
				maxConcurrentReconciles:            defaultMaxConcurrentReconciles,
				measurementMaxConcurrentReconciles: defaultMeasurementMaxConcurrentReconciles,
			},
		},
		{
			name: "core zero",
			opts: controllerConcurrencyOptions{
				maxConcurrentReconciles:            0,
				measurementMaxConcurrentReconciles: defaultMeasurementMaxConcurrentReconciles,
			},
			wantErr: "--max-concurrent-reconciles must be greater than 0, got 0",
		},
		{
			name: "measurement zero",
			opts: controllerConcurrencyOptions{
				maxConcurrentReconciles:            defaultMaxConcurrentReconciles,
				measurementMaxConcurrentReconciles: 0,
			},
			wantErr: "--measurement-max-concurrent-reconciles must be greater than 0, got 0",
		},
		{
			name: "core negative",
			opts: controllerConcurrencyOptions{
				maxConcurrentReconciles:            -1,
				measurementMaxConcurrentReconciles: defaultMeasurementMaxConcurrentReconciles,
			},
			wantErr: "--max-concurrent-reconciles must be greater than 0, got -1",
		},
		{
			name: "measurement negative",
			opts: controllerConcurrencyOptions{
				maxConcurrentReconciles:            defaultMaxConcurrentReconciles,
				measurementMaxConcurrentReconciles: -1,
			},
			wantErr: "--measurement-max-concurrent-reconciles must be greater than 0, got -1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestControllerConcurrencyFlagDefaults(t *testing.T) {
	cmd := newRootCommand()
	tests := map[string]string{
		"max-concurrent-reconciles":             "10",
		"measurement-max-concurrent-reconciles": "5",
	}

	for name, want := range tests {
		flag := cmd.Flags().Lookup(name)
		require.NotNil(t, flag, "flag %q was not registered", name)
		assert.Equal(t, want, flag.DefValue, "flag %q default", name)
	}
}
