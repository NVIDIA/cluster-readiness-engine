// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"cmp"
	"fmt"
	"slices"
)

// TCPXO NCCL plugin version → image mapping for the GCP H100 overrides
// (issues #438, #439).
//
// GKE's nccl-tcpxo-installer DaemonSet puts the TCPXO NCCL plugin on every
// A3 Mega node, and the GCP H100 TCPXO patch mounts it into the workload at
// /usr/local/nvidia. The plugin does not bundle a CUDA runtime: it loads
// libcudart.so.<major> from the workload image, so the image's CUDA major
// must match the one the plugin was built against, or NCCL fails at plugin
// load with "Error loading libnccl-net_internal.so: libcudart.so.<major>".
// Google also couples each plugin release to one tcpxo-daemon (RxDM) release.
// Both follow from the installed plugin version, which the controller
// detects from the installer pods on the target nodes.
//
// Sources: the gpudirect-tcpxo README in GoogleCloudPlatform/
// container-engine-accelerators, which lists each plugin release with its
// qualified CUDA version and its paired tcpxo-daemon image.

const (
	// MinimumTCPXOPluginVersion is the oldest plugin release the catalog
	// maps, and the one rendered when no version is known: the release
	// AICR-provisioned clusters pin, validated on hardware. The NCCL
	// environment already requires plugin v1.0.9 or later, so nothing older
	// is worth a profile.
	MinimumTCPXOPluginVersion = "v1.0.15"

	// tcpxoDaemonArgs are the flags Google's samples pass to daemon v1.0.21
	// and later. v1.0.21 still accepts a retired --num_nics, so one value
	// covers every mapped release.
	tcpxoDaemonArgs = "--num_hops=2"

	// tcpxoDaemonRepo is the tcpxo-daemon (RxDM) image repository.
	tcpxoDaemonRepo = "us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev"

	// pytorchCUDA12Image is the last CUDA 12 NGC PyTorch release (CUDA
	// 12.9.1, OpenMPI 4.1.7). It ships the *_perf_mpi binaries the NCCL
	// entries run and the Megatron-LM dependencies the training entries use.
	pytorchCUDA12Image = "nvcr.io/nvidia/pytorch:25.06-py3"
	// pytorchNCCLCUDA13Image is the NCCL entries' base image (CUDA 13.1),
	// the one every other non-AWS platform override already runs.
	pytorchNCCLCUDA13Image = "nvcr.io/nvidia/pytorch:26.01-py3"
	// pytorchTrainingCUDA13Image is the training entries' base image
	// (CUDA 13.0).
	pytorchTrainingCUDA13Image = "nvcr.io/nvidia/pytorch:25.08-py3"
)

// TCPXOPluginProfile is the set of images the GCP H100 overrides render for
// one TCPXO NCCL plugin version.
type TCPXOPluginProfile struct {
	// NCCLImage is the workload image of the GCP H100 NCCL test entries.
	NCCLImage string
	// TrainingImage is the workload image of the GCP H100 training entries.
	TrainingImage string
	// DaemonImage is the tcpxo-daemon sidecar image Google pairs with the
	// plugin release.
	DaemonImage string
	// DaemonArgs are the flags passed to the daemon's
	// entrypoint_rxdm_container.sh. They follow the daemon release: v1.0.21
	// and later retire --num_nics, --uid and --alsologtostderr (a logged
	// error, not fatal) and reject --enforce_kernel_ipv6_support outright,
	// exiting before the receive path starts.
	DaemonArgs string
}

// tcpxoPluginProfiles maps each TCPXO plugin release tag to its images.
// v1.0.15 and v1.0.16 are qualified on CUDA 12 (v1.0.16 on CUDA 12.8 and
// driver 580.65.06; v1.0.15 was observed on AICR clusters to need
// libcudart.so.12). v1.0.17 is qualified on CUDA 13.2, driver 595.71.05 and
// GKE 1.33.5-gke.1125000 or later; both CUDA 13 images here are an earlier
// CUDA 13 minor, which shares the libcudart.so.13 soname. Adding a release is
// a one-entry edit here. Keys are vMAJOR.MINOR.PATCH with no build suffix.
var tcpxoPluginProfiles = map[string]TCPXOPluginProfile{
	"v1.0.15": {
		NCCLImage:     pytorchCUDA12Image,
		TrainingImage: pytorchCUDA12Image,
		DaemonImage:   tcpxoDaemonRepo + ":v1.0.21",
		DaemonArgs:    tcpxoDaemonArgs,
	},
	"v1.0.16": {
		NCCLImage:     pytorchCUDA12Image,
		TrainingImage: pytorchCUDA12Image,
		DaemonImage:   tcpxoDaemonRepo + ":v1.0.22",
		DaemonArgs:    tcpxoDaemonArgs,
	},
	"v1.0.17": {
		NCCLImage:     pytorchNCCLCUDA13Image,
		TrainingImage: pytorchTrainingCUDA13Image,
		DaemonImage:   tcpxoDaemonRepo + ":v1.0.23",
		DaemonArgs:    tcpxoDaemonArgs,
	},
}

// TCPXOPluginProfileFor returns the images to render for a detected plugin
// release tag, and the mapped release they belong to. The tag is placed by
// its vMAJOR.MINOR.PATCH release, so a build suffix does not change it:
// Google republishes some releases as a rebuild ("v1.0.13-1" a week after
// "v1.0.13"), built against the same CUDA major and paired with the same
// daemon. exact is true when that release is mapped; otherwise the nearest
// safe release is used:
//
//   - newer than every mapped release: the latest mapped one, since a newer
//     plugin keeps at least the newest CUDA major and daemon generation;
//   - between mapped releases but not mapped itself: the newest mapped
//     release older than it, for the same reason;
//   - older than MinimumTCPXOPluginVersion, empty, or not a
//     vMAJOR.MINOR.PATCH tag: MinimumTCPXOPluginVersion, the CUDA 12 release
//     validated on hardware.
func TCPXOPluginProfileFor(version string) (profile TCPXOPluginProfile, release string, exact bool) {
	release = MinimumTCPXOPluginVersion
	v, ok := parseTCPXOPluginVersion(version)
	if !ok {
		return tcpxoPluginProfiles[release], release, false
	}
	for _, known := range knownTCPXOPluginVersions() {
		kv, _ := parseTCPXOPluginVersion(known)
		c := compareTCPXOPluginVersions(kv, v)
		if c > 0 {
			break
		}
		release, exact = known, c == 0
	}
	return tcpxoPluginProfiles[release], release, exact
}

// TCPXOPluginRelease returns the vMAJOR.MINOR.PATCH release of a plugin tag,
// dropping any build suffix ("v1.0.17-1" gives "v1.0.17"), and false when
// version is not a release tag.
func TCPXOPluginRelease(version string) (string, bool) {
	v, ok := parseTCPXOPluginVersion(version)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]), true
}

// latestTCPXOPluginVersion returns the newest mapped plugin release.
func latestTCPXOPluginVersion() string {
	known := knownTCPXOPluginVersions()
	return known[len(known)-1]
}

// knownTCPXOPluginVersions returns the mapped plugin release tags, oldest
// first.
func knownTCPXOPluginVersions() []string {
	versions := make([]string, 0, len(tcpxoPluginProfiles))
	for v := range tcpxoPluginProfiles {
		versions = append(versions, v)
	}
	slices.SortFunc(versions, func(a, b string) int {
		va, _ := parseTCPXOPluginVersion(a)
		vb, _ := parseTCPXOPluginVersion(b)
		return compareTCPXOPluginVersions(va, vb)
	})
	return versions
}

// CompareTCPXOPluginVersion places version against the mapped releases: -1
// when older than MinimumTCPXOPluginVersion, +1 when newer than
// latestTCPXOPluginVersion, 0 otherwise. ok is false when version is not a
// vMAJOR.MINOR.PATCH tag.
func CompareTCPXOPluginVersion(version string) (order int, ok bool) {
	v, ok := parseTCPXOPluginVersion(version)
	if !ok {
		return 0, false
	}
	minV, _ := parseTCPXOPluginVersion(MinimumTCPXOPluginVersion)
	maxV, _ := parseTCPXOPluginVersion(latestTCPXOPluginVersion())
	switch {
	case compareTCPXOPluginVersions(v, minV) < 0:
		return -1, true
	case compareTCPXOPluginVersions(v, maxV) > 0:
		return 1, true
	}
	return 0, true
}

// parseTCPXOPluginVersion parses a "vMAJOR.MINOR.PATCH" release tag. A build
// suffix ("v1.0.13-1", a rebuild of v1.0.13) is ignored.
func parseTCPXOPluginVersion(version string) ([3]int, bool) {
	var v [3]int
	var rest string
	n, _ := fmt.Sscanf(version, "v%d.%d.%d%s", &v[0], &v[1], &v[2], &rest)
	if n < 3 || (rest != "" && rest[0] != '-') {
		return v, false
	}
	return v, true
}

// compareTCPXOPluginVersions compares two parsed release versions.
func compareTCPXOPluginVersions(a, b [3]int) int {
	for i := range a {
		if c := cmp.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}
