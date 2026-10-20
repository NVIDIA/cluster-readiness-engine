// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
)

// newPodReader returns the Pod reader for a reconciler (ADR-102): cached is
// its client, which reads through the job-scoped cache, and live its
// APIReader. Every SetupWithManager defaults APIReader from the manager; a
// unit test that leaves it nil reads both sources from its fake client.
func newPodReader(cached, live client.Reader) *podread.Reader {
	if live == nil {
		live = cached
	}
	return podread.New(cached, live)
}
