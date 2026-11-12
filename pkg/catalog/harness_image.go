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

// releaseVersion matches a tag this repository can release from, which is the
// same shape release.yml's own guard accepts: three numeric components and an
// optional prerelease suffix. The suffix has to be allowed, because the
// repository has released v0.2.0-rc.2 and its siblings, and on such a release
// the harness image is tagged with the prerelease too. Matching only
// vMAJOR.MINOR.PATCH would resolve v0.2.0-rc.3 to v0.2.0 and name an image
// that release never built.
var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// describeSuffix matches what `git describe` appends when HEAD is past the tag:
// the commit count and the abbreviated sha. This is the only part that can be
// stripped to recover a tag, and it has to be stripped rather than cut at the
// first "-" because a prerelease tag contains a "-" of its own.
var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

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
//   - "v1.2.3", or "v1.2.3-rc.1", a release build. Used as given, including
//     the prerelease suffix, because that is how the image was tagged.
//   - "v1.2.3-15-gabc1234", "v1.2.3-rc.1-15-gabc1234" or either with "-dirty",
//     a build after a tag. The describe suffix is removed and the tag
//     underneath is used, so a developer gets the newest harness that actually
//     exists rather than a tag nobody published.
//   - "dev", "abc1234" or empty, a build with no tag in sight. There is
//     nothing to derive, so it renders DevHarnessImageTag.
//
// The stripping is deliberately narrow. Cutting at the first "-" would be
// wrong for every prerelease: it turns v1.2.3-rc.1 into v1.2.3, which is a
// different release and quite possibly one that never existed.
func HarnessImageTag(buildVersion string) string {
	v := strings.TrimSpace(buildVersion)
	if v == "" {
		return DevHarnessImageTag
	}
	// Remove what git describe added, in the order it adds it, leaving the tag.
	v = strings.TrimSuffix(v, "-dirty")
	v = describeSuffix.ReplaceAllString(v, "")
	if releaseVersion.MatchString(v) {
		return v
	}
	return DevHarnessImageTag
}
