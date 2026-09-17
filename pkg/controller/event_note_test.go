// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// Single-value boundary assertions: no resource or Event projection is serialized.
func TestFormatEventNote(t *testing.T) {
	t.Parallel()
	limit := maxEventNoteBytes - len(eventNoteTruncationSuffix)
	for _, tc := range []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"percent", "failure at 100%: %s", "failure at 100%: %s"},
		{"below", strings.Repeat("a", 1023), strings.Repeat("a", 1023)},
		{"exact", strings.Repeat("a", 1024), strings.Repeat("a", 1024)},
		{"over", strings.Repeat("a", 1025), strings.Repeat("a", limit) + eventNoteTruncationSuffix},
		{"unicode-exact", strings.Repeat("界", 341) + "!", strings.Repeat("界", 341) + "!"},
		{"unicode-over", strings.Repeat("界", 342), strings.Repeat("界", limit/3) + eventNoteTruncationSuffix},
		{"split-four-byte", strings.Repeat("a", limit-1) + "😀" + strings.Repeat("z", 32), strings.Repeat("a", limit-1) + eventNoteTruncationSuffix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatEventNote("%s", tc.input)
			require.Equal(t, tc.want, got)
			require.LessOrEqual(t, len(got), maxEventNoteBytes)
			require.True(t, utf8.ValidString(got))
		})
	}
	require.Equal(t, "failure 7: 100%", formatEventNote("failure %d: %s", 7, "100%"))
}

func TestRecorderWrappersBoundEventNotes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"job-recorder", "workflow-recorder", "certification-recorder", "workloadrun-recorder", "goodput-recorder", "bandwidth-recorder"} {
		t.Run(name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(2)
			var emit func(runtime.Object, string, string, ...any)
			switch name {
			case "job-recorder":
				emit = (&JobReconciler{Recorder: recorder}).warnf
			case "workflow-recorder":
				emit = func(obj runtime.Object, reason, format string, args ...any) {
					(&WorkflowReconciler{Recorder: recorder}).eventf(obj, corev1.EventTypeWarning, reason, format, args...)
				}
			case "certification-recorder":
				emit = (&CertificationReconciler{Recorder: recorder}).warnf
			case "workloadrun-recorder":
				emit = (&WorkloadRunReconciler{Recorder: recorder}).warnf
			case "goodput-recorder":
				emit = (&GoodputMeasurementReconciler{Recorder: recorder}).warnf
			case "bandwidth-recorder":
				emit = (&BandwidthMeasurementReconciler{Recorder: recorder}).warnf
			}
			for _, message := range []string{"100%: %s", "100%: %s " + strings.Repeat("界", 1024)} {
				obj := &nvcrev1alpha1.Job{Name: "note-probe", Namespace: testNS}
				emit(obj, "Probe", "failure: %s", message)
				require.Equal(t, "Warning Probe "+formatEventNote("failure: %s", message), <-recorder.Events)
			}
		})
	}
}
