// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run the fatal-path assertion in a subprocess so the parent can verify that
// expired collection stops before attempting to use the client.
func TestEventCollectionRejectsExpiredDeadline(t *testing.T) {
	if os.Getenv("CRE_TEST_EXPIRED_COLLECTION") == "1" {
		collectEventProjections(t, nil, []eventCollectionSpec{{
			InvolvedKind: "Certification", InvolvedName: "expired", Namespace: "default",
		}}, time.Now().Add(-time.Second))
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestEventCollectionRejectsExpiredDeadline$")
	cmd.Env = append(os.Environ(), "CRE_TEST_EXPIRED_COLLECTION=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "exceeded its cumulative")
	require.NotContains(t, string(output), "panic:")
}

// ADR-080 decision 6 gives event-enabled cases one cumulative budget from
// recorder startup through collection, so the recorder's periodic refresh
// (30 minutes) and idle-series cleanup (6 minutes) cannot change the count a
// golden asserts. The per-wait timeouts already in the harness do not bound the
// total on their own: several waits each under their own limit can still add up
// past the point where a series would be flushed or finalized.

// TestEventCaseDeadlineOnlyAppliesToOptedInCases pins that a case without an
// events list keeps the old unbounded behaviour, which is what leaves the
// existing goldens untouched.
func TestEventCaseDeadlineOnlyAppliesToOptedInCases(t *testing.T) {
	require.True(t, eventCaseDeadline(waitConfig{}).IsZero(),
		"a case with no events list must not acquire a deadline")

	cfg := waitConfig{Events: []eventCollectionSpec{{
		InvolvedKind: "Job",
		InvolvedName: "test-job",
		Namespace:    "default",
	}}}
	deadline := eventCaseDeadline(cfg)
	require.False(t, deadline.IsZero(), "an opted-in case must acquire a deadline")
	require.WithinDuration(t, time.Now().Add(eventCaseTimeout), deadline, 5*time.Second)
	require.WithinDuration(t, time.Now().Add(eventCaseTimeout),
		eventCaseDeadline(waitConfig{VerifyCheckpointEvents: true}), 5*time.Second,
		"recorder-observed checkpoint cases share the same cumulative deadline")
	require.WithinDuration(t, time.Now().Add(eventCaseTimeout),
		eventCaseDeadline(waitConfig{VerifyNodePollEvents: true}), 5*time.Second,
		"recorder-observed node polling shares the same cumulative deadline")
}

// TestBoundedWaitTimeoutSharesOneBudget is the core of the rule: each wait gets
// the smaller of its configured timeout and what is left of the case budget, so
// a long configured wait cannot push the case past its deadline.
func TestBoundedWaitTimeoutSharesOneBudget(t *testing.T) {
	t.Run("no deadline returns the configured timeout", func(t *testing.T) {
		require.Equal(t, 30*time.Second,
			boundedWaitTimeout(t, 30*time.Second, time.Time{}))
	})

	t.Run("a generous remaining budget leaves the wait unchanged", func(t *testing.T) {
		deadline := time.Now().Add(eventCaseTimeout)
		require.Equal(t, 5*time.Second,
			boundedWaitTimeout(t, 5*time.Second, deadline))
	})

	t.Run("a wait longer than the remaining budget is clamped", func(t *testing.T) {
		deadline := time.Now().Add(2 * time.Second)
		got := boundedWaitTimeout(t, time.Minute, deadline)
		require.Less(t, got, time.Minute,
			"the configured timeout must not outlive the case deadline")
		require.Positive(t, got)
	})

	t.Run("successive waits draw down the same budget", func(t *testing.T) {
		// Two waits whose configured timeouts each fit comfortably, but whose
		// sum does not. The second must see less headroom than the first.
		deadline := time.Now().Add(3 * time.Second)
		first := boundedWaitTimeout(t, time.Minute, deadline)
		time.Sleep(50 * time.Millisecond)
		second := boundedWaitTimeout(t, time.Minute, deadline)
		require.Less(t, second, first,
			"an individual wait must not reset the cumulative budget")
	})
}

// TestEnsureBeforeDeadlinePassesWithBudgetRemaining pins the collection-time
// guard's non-fatal path. TestEventCollectionRejectsExpiredDeadline checks the
// fatal expiry path in a subprocess.
func TestEnsureBeforeDeadlinePassesWithBudgetRemaining(t *testing.T) {
	require.NotPanics(t, func() {
		ensureBeforeDeadline(t, time.Time{})
		ensureBeforeDeadline(t, time.Now().Add(eventCaseTimeout))
	})
}

// TestContextForDeadlineTracksTheCaseBudget pins that the contexts used for
// event lookups expire with the case rather than running unbounded.
func TestContextForDeadlineTracksTheCaseBudget(t *testing.T) {
	ctx, cancel := contextForDeadline(time.Time{})
	defer cancel()
	_, ok := ctx.Deadline()
	require.False(t, ok, "a case without events gets a plain cancellable context")

	deadline := time.Now().Add(eventCaseTimeout)
	dctx, dcancel := contextForDeadline(deadline)
	defer dcancel()
	got, ok := dctx.Deadline()
	require.True(t, ok, "an opted-in case propagates its deadline to lookups")
	require.WithinDuration(t, deadline, got, time.Second)
}
