// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A persisted condition cannot carry invalid UTF-8: the apiserver replaces
// it on write, so the Reconcile path never observes the bytes this budget
// exists for. Drive eventf itself through the real broadcaster.
func TestBinaryEventNotePersistsWithinAPILimit(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientset, err := kubernetes.NewForConfig(suite.Config)
	require.NoError(t, err)
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: clientset.EventsV1()})
	defer broadcaster.Shutdown()
	require.NoError(t, broadcaster.StartRecordingToSinkWithContext(ctx))
	cm := &corev1.ConfigMap{Name: "binary-event-note", Namespace: corev1.NamespaceDefault}
	require.NoError(t, suite.Client.Create(ctx, cm))
	raw := strings.Repeat("a\x80", 600)
	r := &WorkloadRunReconciler{Recorder: broadcaster.NewRecorder(scheme.Scheme, "event-note-test")}
	r.eventf(cm, corev1.EventTypeWarning, "Probe", "%s", raw)

	var persisted eventsv1.Event
	require.Eventually(t, func() bool {
		list := &eventsv1.EventList{}
		if err := suite.Client.List(ctx, list, client.InNamespace(cm.Namespace)); err != nil {
			return false
		}
		for _, event := range list.Items {
			if event.Regarding.UID == cm.UID && event.Reason == "Probe" {
				persisted = event
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "binary Event note must be accepted by the API server")
	require.Equal(t, formatEventNote("%s", raw), persisted.Note)
	require.LessOrEqual(t, len(persisted.Note), maxEventNoteBytes)
	require.True(t, utf8.ValidString(persisted.Note))
	require.Contains(t, persisted.Note, "\uFFFD")
	require.NotContains(t, persisted.Note, "\x80")
	require.Contains(t, persisted.Note, eventNoteTruncationSuffix)
	require.NotEqual(t, eventNoteTruncationSuffix, persisted.Note)
}
