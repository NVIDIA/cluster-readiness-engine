// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeconfig

import (
	"fmt"
	"time"
)

// MinWaitTimeout is the shortest --timeout accepted when --wait is set.
// Smaller values recreate issue #409: the watch deadline fires before the
// first status check, the CLI prints a near-empty partial report, and
// --cleanup then tears the run down immediately.
const MinWaitTimeout = time.Second

// ValidateWaitTimeout rejects a --timeout below MinWaitTimeout when --wait
// is set. Without --wait, timeout is ignored (including 0 and negatives).
func ValidateWaitTimeout(timeout time.Duration, wait bool) error {
	if !wait {
		return nil
	}
	if timeout < MinWaitTimeout {
		return fmt.Errorf("--timeout must be at least %s when --wait is set, got %s", MinWaitTimeout, timeout)
	}
	return nil
}
