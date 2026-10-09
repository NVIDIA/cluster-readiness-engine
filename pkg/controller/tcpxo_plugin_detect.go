// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// TCPXO NCCL plugin version detection for the GCP H100 overrides (issues
// #438, #439).
//
// GKE's nccl-tcpxo-installer DaemonSet copies the TCPXO NCCL plugin onto each
// A3 Mega node from an init container whose image tag is the plugin release.
// The plugin loads its CUDA runtime from the workload image, and Google pairs
// each release with one tcpxo-daemon release, so the release decides the GCP
// H100 workload images and the daemon image (catalog.TCPXOPluginProfileFor).
// Google allows upgrading the plugin one node pool at a time, so it is read
// from the installer pods on the job's target nodes only. A release is used
// only when every target node runs an installer and all installers on the
// target nodes name the same vMAJOR.MINOR.PATCH release; a build suffix
// ("v1.0.17-1", a rebuild of v1.0.17) does not change the release.
// An unmapped release renders the nearest safe mapped profile (the newest
// mapped release not newer than it, or the minimum for an older one), and no
// release at all renders the minimum; each of those emits a Warning event
// saying why.
//
// The read is one namespace-scoped List of kube-system pods per category
// Workflow creation. In the controller it goes through the manager cache,
// whose cluster-wide Pod informer already exists for the Pod field indexes,
// and the cache's namespace index serves it. It is still a different access
// pattern from the controller's other Pod reads, which all narrow by an
// indexed field or JobSet labels. The manager ClusterRole already grants
// pods get/list/watch cluster-wide.
//
// A refusal from the cache (no release found) is confirmed against the API
// server before it is acted on: detection runs once per Workflow, so an
// installer pod the cache has not caught up with yet would otherwise pin the
// fallback images for the whole run. The live List only happens on that
// refusal path, and a live List that fails keeps the cached refusal. A
// cached read that found a release, mapped or not, is used as is.

const (
	// tcpxoInstallerNamespace is where GKE's nccl-tcpxo-installer runs.
	tcpxoInstallerNamespace = "kube-system"
	// tcpxoPluginImageName is the last path segment of the plugin installer
	// image, us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/
	// nccl-plugin-gpudirecttcpx-dev:<release>. Matching on it rather than on
	// the DaemonSet's labels or the full repository keeps a relabeled
	// DaemonSet or a mirrored registry detectable.
	tcpxoPluginImageName = "nccl-plugin-gpudirecttcpx-dev"

	// tcpxoMessageMaxNodes caps how many node names a detection message
	// lists, so the event note stays well under its size cap.
	tcpxoMessageMaxNodes = 5
)

// tcpxoPluginDetection is the outcome of one TCPXO plugin version detection
// pass, as resolved by resolveTCPXOPluginVersion.
type tcpxoPluginDetection struct {
	// Version is the plugin release every target node runs, mapped or not:
	// the installer tag, e.g. "v1.0.17" or "v1.0.17-1", or just the release
	// when the nodes' tags differ only in a build suffix. Empty when
	// detection did not run or found no single release.
	Version string
	// Exact is true when Version is a release the catalog maps, so its own
	// images render. Anything else renders a fallback profile.
	Exact bool
	// Ran is true only when the GCP H100 gate matched, which is exactly when
	// a non-exact result should be surfaced to the user.
	Ran bool

	// The fields below explain a fallback in tcpxoPluginDetectionMessage.

	// ListErr is the error from listing the installer pods.
	ListErr error
	// Nodes is how many target nodes detection read.
	Nodes int
	// Missing are the target nodes with no running installer pod, sorted.
	Missing []string
	// Versions are the distinct releases found across the target nodes,
	// sorted. An untagged (digest-only) image is recorded as "".
	Versions []string
}

// tcpxoPluginImageVersion returns the release tag of a TCPXO plugin installer
// image, and whether image is one. "repo/nccl-plugin-gpudirecttcpx-dev:v1.0.15"
// and the same with an "@sha256:..." digest appended both give "v1.0.15"; a
// digest-only reference gives "" with ok true.
func tcpxoPluginImageVersion(image string) (version string, ok bool) {
	ref, _, _ := strings.Cut(image, "@")
	name := ref[strings.LastIndex(ref, "/")+1:]
	name, version, _ = strings.Cut(name, ":")
	return version, name == tcpxoPluginImageName
}

// detectTCPXOPluginVersion reads the TCPXO plugin release from the
// nccl-tcpxo-installer pods on nodes. Only Running pods that are not being
// deleted count: a Pending installer has not copied the plugin yet, and a
// terminating one is being replaced. A release is reported only when every
// node runs an installer and all installers on the nodes name the same
// vMAJOR.MINOR.PATCH release; Exact says whether the catalog maps it. How
// many installers a node runs is not checked: Google's installer DaemonSet
// rolls out without surge, and a terminating installer is already skipped.
func detectTCPXOPluginVersion(ctx context.Context, reader client.Reader, nodes []corev1.Node) tcpxoPluginDetection {
	d := tcpxoPluginDetection{Ran: true, Nodes: len(nodes)}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(tcpxoInstallerNamespace)); err != nil {
		d.ListErr = err
		return d
	}

	target := make(map[string]bool, len(nodes))
	for i := range nodes {
		target[nodes[i].Name] = true
	}
	versionsByNode := map[string][]string{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !target[pod.Spec.NodeName] || pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
			if v, ok := tcpxoPluginImageVersion(c.Image); ok {
				versionsByNode[pod.Spec.NodeName] = append(versionsByNode[pod.Spec.NodeName], v)
				break
			}
		}
	}

	for i := range nodes {
		vs, ok := versionsByNode[nodes[i].Name]
		if !ok {
			d.Missing = append(d.Missing, nodes[i].Name)
			continue
		}
		for _, v := range vs {
			if !slices.Contains(d.Versions, v) {
				d.Versions = append(d.Versions, v)
			}
		}
	}
	slices.Sort(d.Missing)
	slices.Sort(d.Versions)

	if len(nodes) == 0 || len(d.Missing) > 0 || len(d.Versions) == 0 {
		return d
	}
	// Tags that differ only in a build suffix are one release: a rolling
	// update from v1.0.17 to its rebuild v1.0.17-1 leaves both on the nodes.
	release := ""
	for _, v := range d.Versions {
		r, ok := catalog.TCPXOPluginRelease(v)
		if !ok || (release != "" && r != release) {
			return d
		}
		release = r
	}
	d.Version = d.Versions[0]
	if len(d.Versions) > 1 {
		d.Version = release
	}
	_, _, d.Exact = catalog.TCPXOPluginProfileFor(d.Version)
	return d
}

// resolveTCPXOPluginVersion resolves the TCPXO plugin release the GCP H100
// overrides render images for, the way every consumer (the Certification
// controller and the CLI dry-run path) must agree on: detection runs only for
// the GCP H100 target.
//
// reader is read first. When it finds no release at all and liveReader is
// non-nil, detection runs again against liveReader, so a cache that lags the
// installer pods cannot force a fallback. The live result is used unless its
// List failed, which says nothing about the installers. A release the cached
// read did find, even an unmapped one, is not re-read: it already picks the
// right profile, and a failed live read must not replace it with the
// minimum. The CLI passes its uncached client as reader and nil for
// liveReader.
func resolveTCPXOPluginVersion(
	ctx context.Context, reader, liveReader client.Reader, platformName, gpuArch string, nodes []corev1.Node,
) tcpxoPluginDetection {
	if !isGCPH100(platformName, gpuArch) {
		return tcpxoPluginDetection{}
	}
	d := detectTCPXOPluginVersion(ctx, reader, nodes)
	if d.Version == "" && liveReader != nil {
		if live := detectTCPXOPluginVersion(ctx, liveReader, nodes); live.ListErr == nil {
			d = live
		}
	}
	return d
}

// tcpxoPluginDetectionMessage renders the user-facing explanation for a
// detection pass that did not find a mapped release: what it found, and the
// images rendered instead. Used verbatim as the TCPXOPluginDetection event
// message by the controller and printed by the CLI dry-run path.
func tcpxoPluginDetectionMessage(d tcpxoPluginDetection) string {
	profile, release, _ := catalog.TCPXOPluginProfileFor(d.Version)
	images := fmt.Sprintf("NCCL image %s, training image %s, tcpxo-daemon %s",
		profile.NCCLImage, profile.TrainingImage, profile.DaemonImage)

	if d.Version != "" {
		switch order, _ := catalog.CompareTCPXOPluginVersion(d.Version); {
		case order > 0:
			return fmt.Sprintf("TCPXO plugin version detection found plugin %s, newer than the latest"+
				" mapped release %s; rendering the %s images (%s), which are not validated against %s."+
				" If they fail, set categories[].options.image on the GCP H100 NCCL and training categories.",
				d.Version, release, release, images, d.Version)
		case order == 0:
			return fmt.Sprintf("TCPXO plugin version detection found plugin %s, which has no image mapping"+
				" of its own; rendering the images for %s, the newest mapped release before it (%s), which"+
				" are not validated against %s. If they fail, set categories[].options.image on the GCP H100"+
				" NCCL and training categories.",
				d.Version, release, images, d.Version)
		}
		return fmt.Sprintf("TCPXO plugin version detection found plugin %s, older than the minimum"+
			" supported release %s; rendering the %s images (%s). Upgrade the nccl-tcpxo-installer, or"+
			" set categories[].options.image on the GCP H100 NCCL and training categories.",
			d.Version, release, release, images)
	}

	var reason string
	switch {
	case d.ListErr != nil:
		reason = fmt.Sprintf("could not list %s pods: %v", tcpxoInstallerNamespace, d.ListErr)
	case d.Nodes == 0:
		reason = "had no target nodes to read"
	case len(d.Missing) > 0:
		reason = fmt.Sprintf("found no running nccl-tcpxo-installer pod (image %s) on %d of %d target nodes%s",
			tcpxoPluginImageName, len(d.Missing), d.Nodes, nodeListSuffix(d.Missing))
	case len(d.Versions) > 1:
		reason = fmt.Sprintf("found different plugin versions across the target nodes (%s)",
			strings.Join(quoteVersions(d.Versions), ", "))
	case len(d.Versions) == 1 && d.Versions[0] == "":
		reason = fmt.Sprintf("found a %s image with no version tag", tcpxoPluginImageName)
	case len(d.Versions) == 1:
		reason = fmt.Sprintf("found plugin tag %q, which is not a release version", d.Versions[0])
	default:
		reason = "could not determine the plugin version"
	}
	return fmt.Sprintf("TCPXO plugin version detection %s; rendering the images for %s, the minimum"+
		" supported release and the one AICR pins (%s). If the plugin is v1.0.17 or later, set"+
		" categories[].options.image to a CUDA 13 image on the GCP H100 NCCL and training categories.",
		reason, release, images)
}

// nodeListSuffix renders ": a, b, c" for up to tcpxoMessageMaxNodes names,
// with a count of the rest, or "" when names is empty.
func nodeListSuffix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	shown := names[:min(len(names), tcpxoMessageMaxNodes)]
	s := ": " + strings.Join(shown, ", ")
	if rest := len(names) - len(shown); rest > 0 {
		s += fmt.Sprintf(" and %d more", rest)
	}
	return s
}

// quoteVersions renders an untagged version as "untagged" so a message never
// shows an empty list entry.
func quoteVersions(versions []string) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		if v == "" {
			v = "untagged"
		}
		out[i] = v
	}
	return out
}
