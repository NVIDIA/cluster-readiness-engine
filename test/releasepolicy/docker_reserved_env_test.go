// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package releasepolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dockerReservedEnv are variables the Docker CLI reads from its own
// environment. Setting one changes how every subsequent docker command
// behaves, whatever the command line says.
//
// They are listed here because reusing one as a build parameter is a mistake
// that looks completely harmless in review: the name reads as a description of
// what it holds, and the collision only appears at runtime, as an error about
// something the command never mentioned.
var dockerReservedEnv = []string{
	"DOCKER_CONTEXT",
	"DOCKER_HOST",
	"DOCKER_CONFIG",
	"DOCKER_API_VERSION",
	"DOCKER_CERT_PATH",
	"DOCKER_TLS_VERIFY",
	"DOCKER_DEFAULT_PLATFORM",
}

// TestNoReservedDockerEnvAsBuildParameter is the regression guard for a defect
// that reached CI and would have reached a release.
//
// The GEMM harness image build passed its build context in a Makefile variable
// named DOCKER_CONTEXT, which `make docker-build DOCKER_CONTEXT=...` exports
// and which build-image.yml also set as a job env. DOCKER_CONTEXT is how the
// Docker CLI selects a daemon context, so docker read the build context path
// as a context name and every build failed with
//
//	ERROR: no builder "images/cutlass-gemm" found
//
// before it ever looked at the Dockerfile. The message names a builder nobody
// asked for, which is why this is worth a guard rather than a comment: the
// error gives no hint that the variable name is the problem.
//
// The manager image was unaffected only because its context is the default,
// so the variable was never set on its path. The harness image is the first
// one to pass a context, and that is what exposed it.
func TestNoReservedDockerEnvAsBuildParameter(t *testing.T) {
	t.Run("Makefile", func(t *testing.T) {
		raw, err := os.ReadFile("../../Makefile")
		if err != nil {
			t.Fatalf("read Makefile: %v", err)
		}
		for _, name := range dockerReservedEnv {
			// An assignment, not a mention: the Makefile documents the name in
			// a comment explaining why it is avoided, and that must stay legal.
			assign := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*[:?+]?=`)
			if assign.Match(raw) {
				t.Errorf("the Makefile assigns %s, which the Docker CLI reads from its own "+
					"environment. Passing it to a recipe exports it, and docker then uses it "+
					"instead of the command line. Pick a name docker does not own.", name)
			}
			// And it must not be handed to a sub-make as a parameter.
			param := regexp.MustCompile(`\$\(MAKE\)[^\n]*\b` + regexp.QuoteMeta(name) + `=`)
			if param.Match(raw) {
				t.Errorf("the Makefile passes %s to a sub-make, which exports it to docker. "+
					"Pick a name docker does not own.", name)
			}
		}
	})

	files, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob %s: %v", workflowDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows found under %s; this guard must not pass by finding "+
			"nothing to check", workflowDir)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			for i, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "#") {
					continue
				}
				for _, name := range dockerReservedEnv {
					if strings.HasPrefix(trimmed, name+":") {
						t.Errorf("%s:%d sets %s in a workflow. The Docker CLI reads it from "+
							"the environment, so it overrides what the build command asks "+
							"for. Pick a name docker does not own.",
							filepath.Base(f), i+1, name)
					}
				}
			}
		})
	}
}
