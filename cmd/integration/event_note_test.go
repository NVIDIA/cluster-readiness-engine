// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// This is a byte-boundary persistence pin, not a resource snapshot. Exercise
// the production phase hook and real events/v1 broadcaster against envtest;
// a fake recorder cannot detect API-server rejection of oversized notes.
func TestLongTransitionEventPersistsWithoutTruncatingStatus(t *testing.T) {
	suite := &testutil.IntegrationTestSuite{}
	suite.Environment.CRDDirectoryPaths = []string{nvcreCRDDirectory}
	suite.Environment.ErrorIfCRDPathMissing = true
	suite.SetupTestSuite(t)
	defer suite.TearDownTestSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clientset, err := kubernetes.NewForConfig(suite.Config)
	require.NoError(t, err)
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: clientset.EventsV1()})
	defer broadcaster.Shutdown()
	require.NoError(t, broadcaster.StartRecordingToSinkWithContext(ctx))
	r := &controller.WorkloadRunReconciler{
		Client: suite.Client, Scheme: scheme.Scheme,
		Recorder: broadcaster.NewRecorder(scheme.Scheme, "event-note-test"),
	}
	run := &nvcrev1alpha1.WorkloadRun{
		Name: "long-event-note", Namespace: corev1.NamespaceDefault,
		Spec: nvcrev1alpha1.WorkloadRunSpec{
			Image: "test-image:latest", NumNodes: 1,
			Framework: nvcrev1alpha1.FrameworkSpec{Exec: &nvcrev1alpha1.ExecFramework{Command: []string{"/bin/true"}}},
		},
	}
	require.NoError(t, suite.Client.Create(ctx, run))
	key := client.ObjectKeyFromObject(run)
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	workflow := &nvcrev1alpha1.Workflow{}
	require.NoError(t, suite.Client.Get(ctx, key, workflow))
	const prefix = "failure 100%: %s "
	const suffix = "... [truncated]"
	message := prefix + strings.Repeat("界", 600)
	meta.SetStatusCondition(&workflow.Status.Conditions, metav1.Condition{
		Type: nvcrev1alpha1.WorkflowFailed, Status: metav1.ConditionTrue,
		Reason: "WorkloadFailed", Message: message,
	})
	require.NoError(t, suite.Client.Status().Update(ctx, workflow))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, suite.Client.Get(ctx, key, run))
	condition := meta.FindStatusCondition(run.Status.Conditions, nvcrev1alpha1.WorkloadRunFailed)
	require.NotNil(t, condition)
	require.Equal(t, metav1.ConditionTrue, condition.Status)
	require.Equal(t, message, condition.Message, "status must retain the complete diagnostic")
	var persisted eventsv1.Event
	require.Eventually(t, func() bool {
		list := &eventsv1.EventList{}
		if err := suite.Client.List(ctx, list, client.InNamespace(run.Namespace)); err != nil {
			return false
		}
		for _, event := range list.Items {
			if event.Regarding.UID == run.UID && event.Reason == condition.Reason {
				persisted = event
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "truncated transition Event must be accepted by the API server")
	want := prefix + strings.Repeat("界", (1024-len(prefix)-len(suffix))/len("界")) + suffix
	require.Equal(t, want, persisted.Note)
	require.LessOrEqual(t, len(persisted.Note), 1024)
	require.True(t, utf8.ValidString(persisted.Note))
	require.Equal(t, corev1.EventTypeWarning, persisted.Type)
	require.Equal(t, condition.Reason, persisted.Action)
	require.NoError(t, suite.Client.Get(ctx, key, run))
	require.Equal(t, message, meta.FindStatusCondition(run.Status.Conditions, nvcrev1alpha1.WorkloadRunFailed).Message)
}
