// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// managerPodLabel and managerPodLabelValue select the controller's pods;
	// the chart sets them on the pod template (templates/deployment.yaml).
	// control-plane: manager alone is a common controller label, so the pods
	// are also selected by the release's instance label.
	managerPodLabel      = "control-plane"
	managerPodLabelValue = "manager"
	releaseInstanceLabel = "app.kubernetes.io/instance"
	// managerContainerName is the controller container in those pods.
	managerContainerName = "manager"

	// managerStableFor is how long every controller pod must have been Ready
	// with no restart before init reports success. helm --wait passes on a
	// single Ready sample, so a pod that is Ready for a few seconds and then
	// OOMKilled during its initial cache sync passes it (issue #504).
	managerStableFor = time.Minute
	// managerStableTimeout bounds the whole check. It leaves room for a pod
	// that Helm saw Ready only just before it returned.
	managerStableTimeout = 3 * time.Minute
	// managerPollInterval is how often the controller pods are read.
	managerPollInterval = 2 * time.Second
	// managerReadTimeout bounds each read of the controller pods. The
	// kubeconfig sets no request timeout, so without it a stalled read could
	// hold the check past managerStableTimeout, or hold back Helm's own error
	// while a failed install is diagnosed.
	managerReadTimeout = 10 * time.Second

	reasonOOMKilled        = "OOMKilled"
	reasonCrashLoopBackOff = "CrashLoopBackOff"
	// reasonError is the kubelet's reason for a non-zero exit; it also stands
	// in when a termination carries no reason.
	reasonError = "Error"
)

// managerStability checks, after helm --wait has passed, that the controller
// stays up. now and sleep are injected so tests can run without a real clock.
type managerStability struct {
	c           client.Reader
	stableFor   time.Duration
	timeout     time.Duration
	interval    time.Duration
	readTimeout time.Duration
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

func newManagerStability(c client.Reader) managerStability {
	return managerStability{
		c:           c,
		stableFor:   managerStableFor,
		timeout:     managerStableTimeout,
		interval:    managerPollInterval,
		readTimeout: managerReadTimeout,
		now:         time.Now,
		sleep:       sleepContext,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// verify returns nil once every controller pod has been Ready, with its
// manager container running and not restarted, for stableFor. It fails as
// soon as a container exits or restarts during the check or sits in
// CrashLoopBackOff, and when the pods have not settled by timeout. Each read
// is bounded by readTimeout, so the check ends within timeout plus one read.
//
// A restart count the pod already had when the check first saw it is not a
// failure by itself: a controller that restarted once days ago is healthy.
// Only a restart seen during the check counts, and a pod that restarted
// shortly before the check must still stay up for stableFor afterwards.
func (m managerStability) verify(ctx context.Context) error {
	deadline := m.now().Add(m.timeout)
	baseline := map[types.UID]int32{}
	for {
		pods, err := listManagerPods(ctx, m.c, m.readTimeout)
		if err != nil {
			return err
		}
		for i := range pods {
			pod := &pods[i]
			cs := managerContainerStatus(pod)
			if cs == nil {
				continue
			}
			before, seen := baseline[pod.UID]
			if !seen {
				baseline[pod.UID] = cs.RestartCount
			}
			// The kubelet reports an exited container as terminated for a few
			// seconds before it restarts it; the controller never exits on its
			// own, so that is already a crash.
			if (seen && cs.RestartCount > before) || isCrashLoopBackOff(cs) || cs.State.Terminated != nil {
				return managerCrashError(pod, cs)
			}
		}
		now := m.now()
		if managerPodsStable(pods, now, m.stableFor) {
			return nil
		}
		if !now.Before(deadline) {
			return managerNotStableError(pods, m.stableFor, m.timeout)
		}
		if err := m.sleep(ctx, m.interval); err != nil {
			return fmt.Errorf("waiting for the NVCRE controller: %w", err)
		}
	}
}

// diagnoseManagerCrash reads the controller pods once, within readTimeout, and
// returns why one is crash-looping, or nil when none shows it or the read
// fails. A failed helm --wait reports only "context deadline exceeded"; this
// names the cause, such as OOMKilled. The bound keeps a stalled read from
// holding back Helm's error.
func diagnoseManagerCrash(ctx context.Context, c client.Reader, readTimeout time.Duration) error {
	pods, err := listManagerPods(ctx, c, readTimeout)
	if err != nil {
		return nil
	}
	for i := range pods {
		pod := &pods[i]
		cs := managerContainerStatus(pod)
		if cs == nil {
			continue
		}
		// A Ready container that exited at some point is not why helm
		// failed; only one that is down after an exit is.
		if isCrashLoopBackOff(cs) || (!cs.Ready && latestTermination(cs) != nil) {
			return managerCrashError(pod, cs)
		}
	}
	return nil
}

// latestTermination returns the container's most recent exit. An exited
// container the kubelet has not restarted yet holds it in State, and
// LastTerminationState then still describes the attempt before; once the
// container restarts, LastTerminationState holds it.
func latestTermination(cs *corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	if cs.State.Terminated != nil {
		return cs.State.Terminated
	}
	return cs.LastTerminationState.Terminated
}

// listManagerPods returns the NVCRE release's controller pods that are not
// being deleted, read within readTimeout. A rollout leaves the previous pod
// terminating for a while; it is not the controller the release now runs.
func listManagerPods(ctx context.Context, c client.Reader, readTimeout time.Duration) ([]corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	list := &corev1.PodList{}
	if err := c.List(ctx, list, client.InNamespace(nvcreNamespace), client.MatchingLabels{
		managerPodLabel:      managerPodLabelValue,
		releaseInstanceLabel: helmReleaseName,
	}); err != nil {
		return nil, fmt.Errorf("list NVCRE controller pods: %w", err)
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			pods = append(pods, list.Items[i])
		}
	}
	return pods, nil
}

// managerContainerStatus returns the status of the controller container, or
// nil when the kubelet has not reported it yet.
func managerContainerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == managerContainerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func isCrashLoopBackOff(cs *corev1.ContainerStatus) bool {
	return cs.State.Waiting != nil && cs.State.Waiting.Reason == reasonCrashLoopBackOff
}

// managerPodsStable reports whether there is at least one controller pod and
// every one is Ready with its manager container running since at least
// stableFor before now.
func managerPodsStable(pods []corev1.Pod, now time.Time, stableFor time.Duration) bool {
	if len(pods) == 0 {
		return false
	}
	for i := range pods {
		cs := managerContainerStatus(&pods[i])
		if cs == nil || !cs.Ready || cs.State.Running == nil || !podReady(&pods[i]) {
			return false
		}
		if now.Sub(cs.State.Running.StartedAt.Time) < stableFor {
			return false
		}
	}
	return true
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// managerCrashError describes a crash-looping controller container: how often
// it restarted and how it last exited. An OOMKill also names the memory
// limit and the setup init flag that raises it, which init keeps across runs
// (ADR-098).
func managerCrashError(pod *corev1.Pod, cs *corev1.ContainerStatus) error {
	var b strings.Builder
	fmt.Fprintf(&b, "the NVCRE controller is crash-looping: pod %s/%s restarted %d %s",
		pod.Namespace, pod.Name, cs.RestartCount, plural(int(cs.RestartCount), "time", "times"))
	last := latestTermination(cs)
	if last != nil {
		fmt.Fprintf(&b, ", last exit %s (exit code %d)", terminationReason(last), last.ExitCode)
	}
	b.WriteString(".\n")
	if last != nil && last.Reason == reasonOOMKilled {
		b.WriteString(oomHint(pod))
	} else {
		fmt.Fprintf(&b, "Inspect its last run with: kubectl -n %s logs %s -c %s --previous",
			pod.Namespace, pod.Name, managerContainerName)
	}
	return fmt.Errorf("%s", b.String())
}

// oomHint tells the operator to raise the controller's memory limit. The
// controller caches cluster-wide objects, so its memory grows with the size
// of the cluster; doubling the limit is a starting point, not a sizing rule.
func oomHint(pod *corev1.Pod) string {
	var limit *resource.Quantity
	for i := range pod.Spec.Containers {
		if c := pod.Spec.Containers[i]; c.Name == managerContainerName {
			if l, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				limit = &l
			}
		}
	}
	if limit == nil || limit.IsZero() {
		return "It ran out of memory. Give it a memory request and limit that fit this cluster, for example:\n" +
			"  nvcrectl setup init --set manager.resources.requests.memory=2Gi --set manager.resources.limits.memory=4Gi"
	}
	doubled := resource.NewQuantity(limit.Value()*2, resource.BinarySI)
	return fmt.Sprintf("It ran out of memory at its %s limit. The controller's memory grows with the cluster's size; "+
		"raise the limit, for example:\n  nvcrectl setup init --set manager.resources.limits.memory=%s",
		limit.String(), doubled.String())
}

func terminationReason(t *corev1.ContainerStateTerminated) string {
	if t.Reason != "" {
		return t.Reason
	}
	return reasonError
}

// managerNotStableError explains why the controller pods did not settle in
// time: missing, not Ready, or Ready for less than stableFor.
func managerNotStableError(pods []corev1.Pod, stableFor, timeout time.Duration) error {
	if len(pods) == 0 {
		return fmt.Errorf("no NVCRE controller pod appeared in namespace %s within %s", nvcreNamespace, timeout)
	}
	var states []string
	for i := range pods {
		pod := &pods[i]
		state := "no container status"
		if cs := managerContainerStatus(pod); cs != nil {
			switch {
			case cs.State.Waiting != nil:
				state = "waiting: " + cs.State.Waiting.Reason
			case cs.State.Terminated != nil:
				state = "terminated: " + terminationReason(cs.State.Terminated)
			case !cs.Ready || !podReady(pod):
				state = "running, not Ready"
			default:
				state = "Ready"
			}
		}
		states = append(states, fmt.Sprintf("%s (%s)", pod.Name, state))
	}
	return fmt.Errorf("the NVCRE controller did not stay Ready for %s within %s: %s",
		stableFor, timeout, strings.Join(states, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// verifyManagerStable runs the post-install check and reports it on out.
func verifyManagerStable(ctx context.Context, m managerStability, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "[helm] Checking that the NVCRE controller stays Ready for %s...\n", m.stableFor)
	return m.verify(ctx)
}
