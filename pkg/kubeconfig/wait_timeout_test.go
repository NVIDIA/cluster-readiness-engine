// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateWaitTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		wait    bool
		wantErr string
	}{
		{name: "zero without wait is ignored", timeout: 0},
		{name: "negative without wait is ignored", timeout: -time.Second},
		{name: "1ms without wait is ignored", timeout: time.Millisecond},
		{
			name:    "zero with wait is rejected",
			timeout: 0,
			wait:    true,
			wantErr: "--timeout must be at least 1s when --wait is set, got 0s",
		},
		{
			name:    "negative with wait is rejected",
			timeout: -time.Second,
			wait:    true,
			wantErr: "--timeout must be at least 1s when --wait is set, got -1s",
		},
		{
			name:    "1ms with wait is rejected",
			timeout: time.Millisecond,
			wait:    true,
			wantErr: "--timeout must be at least 1s when --wait is set, got 1ms",
		},
		{name: "1s with wait is accepted", timeout: time.Second, wait: true},
		{name: "30m with wait is accepted", timeout: 30 * time.Minute, wait: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateWaitTimeout(tt.timeout, tt.wait)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.wantErr)
		})
	}
}
