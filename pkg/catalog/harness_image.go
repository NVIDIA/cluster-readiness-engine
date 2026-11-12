// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"regexp"
	"strings"
)

// DevHarnessImageTag is what an unversioned build renders.
//
// It names no published image on purpose. A local build has no release to
// point at, and inventing one would mean a developer's run silently pulled
// some other release's harness. The lanes that run this entry without a
// release already select their image explicitly: Mokka and UAT set
// spec.image, and so does anyone testing a harness change.
const DevHarnessImageTag = "dev"

// releaseVersion matches a released version exactly: a leading v, three
// numeric components, nothing else. git describe appends to this form rather
// than replacing it, which is what makes the prefix below safe to take.
var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// buildVersion is the running binary's version, as -ldflags set it.
//
// Package state rather than a BuildConfig field, because it describes the
// process and not the render. A field would have to be set identically at
// every construction site and would silently fall back to "dev" wherever
// someone forgot, which is the same class of mistake deriving the tag exists
// to remove. It is written once from main before any reconcile or render, and
// only read afterwards.
var buildVersion string

// SetBuildVersion records the running binary's version for entries that
// derive an image tag from it. Called once from main, ahead of any render.
func SetBuildVersion(v string) { buildVersion = v }

// BuildVersion returns what SetBuildVersion recorded.
func BuildVersion() string { return buildVersion }

// HarnessImageTag turns a build version into the tag of a harness image this
// repository publishes for itself.
//
// This exists so that no catalog entry carries a literal version. release.yml
// builds both the binary and the harness image from the same tag, so the
// binary's own version IS the tag of the image that shipped with it, and
// deriving it removes the step where someone bumps a pin to the release they
// are about to cut. That step had no check behind it: forgetting it shipped a
// catalog naming an image that was never built, and the first symptom was
// ImagePullBackOff on a cluster that had already installed the release.
//
// Three cases, because `git describe --tags --always --dirty` produces three
// shapes:
//
//   - "v1.2.3", a release build. Used as given.
//   - "v1.2.3-15-gabc1234" or "v1.2.3-dirty", a build after a tag. The tag
//     prefix is taken, so a developer gets the newest harness that actually
//     exists rather than a tag nobody published.
//   - "dev", "abc1234" or empty, a build with no tag in sight. There is
//     nothing to derive, so it renders DevHarnessImageTag.
func HarnessImageTag(buildVersion string) string {
	v := strings.TrimSpace(buildVersion)
	if v == "" {
		return DevHarnessImageTag
	}
	if releaseVersion.MatchString(v) {
		return v
	}
	// A describe string carries its tag ahead of the first "-". Taking the
	// prefix is only correct when that prefix is itself a release version;
	// "abc1234-dirty" has no tag in it and must not yield "abc1234".
	if prefix, _, found := strings.Cut(v, "-"); found && releaseVersion.MatchString(prefix) {
		return prefix
	}
	return DevHarnessImageTag
}
