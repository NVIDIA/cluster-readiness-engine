// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// hostCompilingDockerfiles are the images whose compiler targets the build
// host, so they cannot be cross-compiled and must be built on a runner of the
// architecture they are for.
//
// The controller image is deliberately not here: it cross-compiles with
// `GOARCH=${TARGETARCH}` and `CGO_ENABLED=0`, which is what makes one amd64
// runner able to produce both platforms.
var hostCompilingDockerfiles = []string{
	"images/cutlass-gemm/Dockerfile",
}

// TestHostCompilingImagesBuildNatively is the regression guard for a defect
// that published a working-looking image containing binaries for the wrong
// architecture.
//
// `make docker-buildx` rewrites the first `FROM` to
// `FROM --platform=${BUILDPLATFORM}` so the builder stage runs on the native
// machine while the output targets another architecture. For a Go binary that
// is exactly right. For the GEMM harness the first stage is the CUDA compile
// stage and `nvcc` compiles for the host, so the arm64 leg produced x86-64
// binaries and `COPY --from=build` put them into an arm64 runtime image. The
// `ldd` check in that Dockerfile would fail the build, so the visible symptom
// was a red release rather than a silently broken image, but the amd64 PR
// check builds a single architecture and never reached it.
//
// So: every caller that builds one of these Dockerfiles must ask for native
// per-architecture builds.
func TestHostCompilingImagesBuildNatively(t *testing.T) {
	type job struct {
		Uses string            `json:"uses"`
		With map[string]any    `json:"with"`
		Name string            `json:"name"`
		Env  map[string]string `json:"env"`
	}
	var doc struct {
		Jobs map[string]job `json:"jobs"`
	}

	files, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob %s: %v", workflowDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows under %s; this guard must not pass by finding nothing", workflowDir)
	}

	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		doc.Jobs = nil
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for name, j := range doc.Jobs {
			df, _ := j.With["dockerfile"].(string)
			if df == "" {
				continue
			}
			if !slices.Contains(hostCompilingDockerfiles, df) {
				continue
			}
			checked++
			native, ok := j.With["native_per_arch"]
			if !ok || native != true {
				t.Errorf("%s job %q builds %s but does not pass native_per_arch: true. "+
					"That Dockerfile's compiler targets the build host, so the "+
					"cross-compile path would publish an image whose binaries are for "+
					"the wrong architecture.", filepath.Base(f), name, df)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("found no workflow job building any of %v; if the harness build moved, "+
			"update this list rather than deleting the test", hostCompilingDockerfiles)
	}
}

// TestNativeAndCrossTargetsStaySeparate pins that the two Makefile targets do
// not call each other.
//
// `docker-buildx` must keep the `FROM` rewrite, because that is what makes the
// controller image's cross-compilation work. `docker-buildx-native` must not
// have it. A future edit that routed one through the other would reintroduce
// the defect in whichever direction it went, and the result would be an image
// that builds and passes its checks while containing the wrong machine code.
func TestNativeAndCrossTargetsStaySeparate(t *testing.T) {
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	body := string(raw)

	native := makeRecipe(t, body, "docker-buildx-native")
	cross := makeRecipe(t, body, "docker-buildx")

	if strings.Contains(native, "sed") {
		t.Error("docker-buildx-native rewrites the Dockerfile. It exists precisely because " +
			"the rewrite is wrong for an image whose compiler targets the host.")
	}
	if !strings.Contains(cross, "sed") {
		t.Error("docker-buildx no longer rewrites the first FROM to BUILDPLATFORM. That " +
			"rewrite is what lets the controller image cross-compile on one runner; " +
			"without it the manager build changes behaviour.")
	}
	if !strings.Contains(native, "--provenance=false") {
		t.Error("docker-buildx-native does not pass --provenance=false. With attestations " +
			"on, a push-by-digest returns the digest of a small index rather than of " +
			"the image manifest, and assembling that into the published index yields a " +
			"child with no usable platform.")
	}
	if !strings.Contains(native, "push-by-digest=true") {
		t.Error("docker-buildx-native does not push by digest, so the per-architecture " +
			"legs would fight over one tag instead of being assembled into an index")
	}
	// The GEMM target must use the native one. Reaching for docker-buildx here
	// is the single edit that reintroduces the original defect.
	gemm := makeRecipe(t, body, "gemm-image-native")
	if !strings.Contains(gemm, "docker-buildx-native") {
		t.Errorf("gemm-image-native does not call docker-buildx-native; recipe was:\n%s", gemm)
	}
	if regexp.MustCompile(`docker-buildx\s`).MatchString(gemm) {
		t.Error("gemm-image-native calls the cross-compiling docker-buildx, which would " +
			"publish an arm64 harness image full of x86-64 binaries")
	}
}

// makeRecipe returns the recipe lines of one Makefile target.
func makeRecipe(t *testing.T, body, target string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, target+":") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("the Makefile has no %q target; if it was renamed, update this test "+
			"rather than deleting it", target)
	}
	var out []string
	for _, l := range lines[start:] {
		if l == "" || (!strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, " ")) {
			break
		}
		// Comments are dropped. These recipes explain at length why the
		// rewrite is wrong here, so a prose mention of `sed` would otherwise
		// satisfy the assertion that looks for the command.
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestBuildersAreDistinctPerDockerfile pins that two concurrent builds do not
// share a buildx builder.
//
// Every Dockerfile in this repository is named "Dockerfile", so deriving the
// builder name from the basename gave the controller build and the harness
// build the same one. Each recipe's trap removes the builder it used on exit,
// so two builds running at once had one delete the other's mid-build, which
// surfaces as an unrelated BuildKit error rather than as a name collision.
func TestBuildersAreDistinctPerDockerfile(t *testing.T) {
	raw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	line := regexp.MustCompile(`(?m)^BUILDX_BUILDER\s*\?=\s*(.+)$`).FindStringSubmatch(string(raw))
	if line == nil {
		t.Fatal("the Makefile no longer sets BUILDX_BUILDER; if it moved, update this test")
	}
	def := line[1]
	if strings.Contains(def, "notdir") {
		t.Errorf("BUILDX_BUILDER is %q, which uses notdir. Every Dockerfile here is "+
			"named \"Dockerfile\", so that gives concurrent builds the same builder and "+
			"each recipe's trap deletes the other's.", def)
	}
	if !strings.Contains(def, "DOCKERFILE") {
		t.Errorf("BUILDX_BUILDER is %q, which does not depend on the Dockerfile at all, "+
			"so every build shares one builder", def)
	}
}
