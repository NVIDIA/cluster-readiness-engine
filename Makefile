# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Version from git tags (e.g. v1.2.3, v1.2.3-4-gabcdef, or short SHA).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
# Linker flags to inject version into binaries.
LDFLAGS ?= -s -w -X main.version=$(VERSION)

# Image configuration
# Registry and repository for the controller image
IMAGE_REGISTRY ?= ghcr.io
IMAGE_REPOSITORY ?= nvidia/cluster-readiness-engine/manager
IMAGE_TAG ?= $(VERSION)
# Full image URL
IMG ?= $(IMAGE_REGISTRY)/$(IMAGE_REPOSITORY):$(IMAGE_TAG)

# Helm chart directory (used by manifests and helm-* targets).
HELM_CHART_DIR ?= helm/cluster-readiness-engine

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Comma variable for use in $(subst) to split comma-separated lists.
comma := ,

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs directly into the Helm chart (RBAC is maintained manually in templates/manager-role.yaml).
	mkdir -p "$(HELM_CHART_DIR)/crds" "$(HELM_CHART_DIR)/templates"
	"$(CONTROLLER_GEN)" rbac:roleName=nvcre-manager-role crd webhook paths="./..." \
		output:crd:artifacts:config="$(HELM_CHART_DIR)/crds" \
		output:rbac:none

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code, including build-tagged test suites.
	go vet ./...
	go vet -tags=uat ./test/uat/...

.PHONY: test
test: setup-envtest ## Run unit and integration tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
	go test -v $$(go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v /cmd/integration)
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
	go test ./cmd/integration/ -v -timeout 300s -count=1

.PHONY: test-ci
test-ci: setup-envtest gotestsum gocover-cobertura ## Run tests with JUnit XML and coverage reports for CI.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
	"$(GOTESTSUM)" --junitfile unit-report.xml -- \
		-coverprofile=cover-unit.out -covermode=atomic \
		$$(go list -f '{{if .TestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v /cmd/integration)
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
	"$(GOTESTSUM)" --junitfile integration-report.xml -- \
		-coverprofile=cover-integration.out -covermode=atomic \
		./cmd/integration/ -timeout 300s -count=1
	go tool cover -func=cover-unit.out
	"$(GOCOVER_COBERTURA)" < cover-unit.out > coverage.xml

.PHONY: test-integration
test-integration: fmt vet setup-envtest ## Run integration tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
	go test ./cmd/integration/ -v -timeout 300s -count=1

##@ UAT Tests (Kind + KWOK + Tilt + e2e-framework)

KIND_CLUSTER_UAT ?= nvcre-test-uat
KIND_NODE_IMAGE ?=
# Deliberately manual pin (GitHub-release binary, not a go.mod tool). See
# .github/dependabot.yml.
KWOK_VERSION ?= v0.7.0
UAT_IMG ?= $(IMAGE_REGISTRY)/$(IMAGE_REPOSITORY):uat-test

.PHONY: setup-test-uat
setup-test-uat: ## Create Kind cluster for UAT tests and wait for it to be ready.
	@command -v $(KIND) >/dev/null 2>&1 || { echo "Kind is not installed. Please install Kind manually."; exit 1; }
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER_UAT)"*) \
			echo "Kind cluster '$(KIND_CLUSTER_UAT)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER_UAT)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER_UAT) $(if $(KIND_NODE_IMAGE),--image "$(KIND_NODE_IMAGE)",) ;; \
	esac
	@echo "Waiting for nodes to be ready..."
	@$(KUBECTL) wait --for=condition=Ready nodes --all --timeout=120s

.PHONY: tilt-uat
tilt-uat: setup-test-uat ## Start Tilt for UAT (interactive dev mode with hot reload).
	cd test/uat && IMG=$(UAT_IMG) KWOK_VERSION=$(KWOK_VERSION) tilt up

.PHONY: tilt-uat-ci
tilt-uat-ci: setup-test-uat ## Start Tilt for UAT in CI mode (headless, exits after deploy).
	cd test/uat && IMG=$(UAT_IMG) KWOK_VERSION=$(KWOK_VERSION) tilt ci

.PHONY: test-uat
test-uat: tilt-uat-ci ## Run UAT tests (Tilt deploys everything, then run tests).
	KIND_CLUSTER=$(KIND_CLUSTER_UAT) NVCRECTL=$(LOCALBIN)/nvcrectl \
		go test -tags=uat ./test/uat/ -v -timeout 1800s -count=1
	$(MAKE) cleanup-test-uat

.PHONY: test-uat-run
test-uat-run: ## Run UAT tests against existing Tilt-managed cluster (dev iteration).
	NVCRECTL=$(LOCALBIN)/nvcrectl go test -tags=uat ./test/uat/ -v -timeout 1800s -count=1

.PHONY: cleanup-test-uat
cleanup-test-uat: ## Delete Kind cluster for UAT tests.
	@$(KIND) delete cluster --name $(KIND_CLUSTER_UAT)

.PHONY: check-agents-sync
check-agents-sync: ## Verify AGENTS.md is an exact copy of CLAUDE.md.
	@cmp -s CLAUDE.md AGENTS.md || { echo "AGENTS.md is out of sync with CLAUDE.md. Run: cp CLAUDE.md AGENTS.md"; exit 1; }

.PHONY: lint
lint: golangci-lint check-agents-sync ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: verify-codegen
verify-codegen: manifests generate ## Verify generated CRDs, RBAC, and DeepCopy code are committed.
	@git diff --exit-code -- api helm || { \
	  echo "ERROR: generated files are stale. Run 'make manifests generate' and commit the result."; \
	  exit 1; \
	}

.PHONY: verify-mod
verify-mod: ## Verify go.mod and go.sum are tidy, in this module and in tools/.
	go mod tidy
	go -C tools mod tidy
	@git diff --exit-code -- go.mod go.sum tools/go.mod tools/go.sum || { \
	  echo "ERROR: a go.mod or go.sum is not tidy. Run 'go mod tidy' and"; \
	  echo "'go -C tools mod tidy', then commit the result."; \
	  exit 1; \
	}

.PHONY: verify-license-headers
verify-license-headers: addlicense ## Verify Go sources carry the SPDX license header.
	"$(ADDLICENSE)" -check -ignore '**/testdata/**' -ignore 'bin/**' \
	  -ignore '**/*.yaml' -ignore '**/*.yml' -ignore '**/*.json' \
	  api pkg cmd test hack

.PHONY: verify-doc-links
verify-doc-links: ## Verify relative markdown links in README.md and docs/ resolve to files in the tree.
	hack/verify-doc-links.sh

.PHONY: fern-freeze-versions
fern-freeze-versions: ## Rebuild the frozen per-version docs content that fern/docs.yml points at.
	hack/fern-freeze-versions.sh

.PHONY: verify
verify: verify-codegen verify-mod verify-license-headers verify-doc-links ## Run all verification checks.

.PHONY: ci
ci: verify lint build test ## Run the full CI gate locally: verify, lint, build, and test.

##@ Build

.PHONY: check-clean-version
check-clean-version: ## Verify VERSION is a clean release tag (vX.Y.Z with optional -prerelease; no -dirty, -N-gXXX, dev, or bare SHA).
	@case "$(VERSION)" in \
	  dev|*-dirty|*-*-g*) \
	    echo "ERROR: VERSION=$(VERSION) is not a clean release tag."; \
	    echo "Commit and tag your changes first (e.g., git tag v1.14.0)."; \
	    exit 1 ;; \
	  v[0-9]*.[0-9]*.[0-9]*) ;; \
	  *) \
	    echo "ERROR: VERSION=$(VERSION) does not look like a release tag (vX.Y.Z or vX.Y.Z-rc.N)."; \
	    echo "git describe falls back to a bare commit SHA when no tag is reachable;"; \
	    echo "fetch tags (git fetch --tags) or pass VERSION=<tag> explicitly."; \
	    exit 1 ;; \
	esac


.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager/

.PHONY: build-nvcrectl
build-nvcrectl: $(LOCALBIN) ## Build nvcrectl CLI tool.
	go build -ldflags "$(LDFLAGS)" -o bin/nvcrectl ./cmd/nvcrectl/
	ln -sf nvcrectl bin/kubectl-nvcrectl

.PHONY: build-nvcrectl-cross
build-nvcrectl-cross: $(LOCALBIN) ## Cross-compile nvcrectl for all NVCRECTL_PLATFORMS (linux, macOS).
	@for platform in $(subst $(comma), ,$(NVCRECTL_PLATFORMS)); do \
		os=$${platform%/*}; \
		arch=$${platform#*/}; \
		ext=""; if [ "$${os}" = "windows" ]; then ext=".exe"; fi; \
		echo "Building nvcrectl for $${os}/$${arch}..."; \
		CGO_ENABLED=0 GOOS=$${os} GOARCH=$${arch} \
			go build -ldflags "$(LDFLAGS)" -o bin/nvcrectl-$${os}-$${arch}$${ext} ./cmd/nvcrectl/ || exit 1; \
	done

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/manager/

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t "${IMG}" .

.PHONY: docker-push
docker-push: check-clean-version ## Push docker image with the manager.
	$(CONTAINER_TOOL) push "${IMG}"

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64

# Platforms for nvcrectl CLI cross-compilation (includes macOS and Windows for end-user workstations).
NVCRECTL_PLATFORMS ?= linux/amd64,linux/arm64,darwin/amd64,darwin/arm64

# BUILDX_PUSH controls whether docker-buildx pushes the image.
# Default pushes. Set BUILDX_PUSH= (empty) to build without pushing,
# which validates both platforms and discards the result.
BUILDX_PUSH ?= --push

.PHONY: docker-buildx
docker-buildx: #check-clean-version ## Build and push docker image for the manager for cross-platform support
	# Run as one shell, so the trap removes the builder and the temporary
	# Dockerfile even when the build fails.
	trap '$(CONTAINER_TOOL) buildx rm nvcre-builder >/dev/null 2>&1 || true; rm -f Dockerfile.cross' EXIT; \
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross; \
	$(CONTAINER_TOOL) buildx create --name nvcre-builder >/dev/null 2>&1 || true; \
	$(CONTAINER_TOOL) buildx use nvcre-builder; \
	$(CONTAINER_TOOL) buildx build --build-arg VERSION=$(VERSION) $(BUILDX_PUSH) --platform=$(PLATFORMS) --tag "${IMG}" -f Dockerfile.cross .

##@ Deployment

##@ Helm

HELM ?= helm
HELM_PACKAGE_VERSION ?= $(VERSION)
# OCI registry base for Helm chart publishing.
# helm push pushes to $(HELM_OCI_REGISTRY)/<chart-name>:<version>.
HELM_OCI_REGISTRY ?= oci://ghcr.io/nvidia
# When set, helm-push writes the pushed chart's digest here. CI reads it to hand
# the digest to the signing workflow without scraping the log.
CHART_DIGEST_FILE ?=

.PHONY: helm-lint
helm-lint: ## Lint the cluster-readiness-engine Helm chart.
	"$(HELM)" dependency build $(HELM_CHART_DIR) --skip-refresh
	"$(HELM)" lint $(HELM_CHART_DIR)

.PHONY: helm-package
helm-package: helm-lint ## Package the cluster-readiness-engine Helm chart.
	"$(HELM)" package $(HELM_CHART_DIR) \
		--version "$(HELM_PACKAGE_VERSION)" \
		--app-version "$(VERSION)"

.PHONY: helm-push
# The digest is what gets signed, so print it rather than leaving the operator
# to look it up. Both streams are captured because helm's output stream for the
# digest line is not a documented contract, and CHART_DIGEST_FILE lets CI take
# the value without re-parsing a log.
helm-push: check-clean-version helm-package ## Push the Helm chart to the OCI registry and print its digest.
	@set -eu; \
	log="$$(mktemp)"; \
	trap 'rm -f "$$log"' EXIT; \
	if ! "$(HELM)" push \
		cluster-readiness-engine-$(HELM_PACKAGE_VERSION).tgz \
		$(HELM_OCI_REGISTRY) >"$$log" 2>&1; then \
		cat "$$log" >&2; \
		exit 1; \
	fi; \
	cat "$$log"; \
	digest="$$(sed -n 's/.*[Dd]igest: \(sha256:[0-9a-f]\{64\}\).*/\1/p' "$$log" | tail -1)"; \
	if [ -z "$$digest" ]; then \
		echo "ERROR: helm push reported no sha256 digest; refusing to continue because the digest is what gets signed" >&2; \
		exit 1; \
	fi; \
	echo "chart digest: $$digest"; \
	if [ -n "$(CHART_DIGEST_FILE)" ]; then printf '%s' "$$digest" > "$(CHART_DIGEST_FILE)"; fi

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
ADDLICENSE ?= $(LOCALBIN)/addlicense
GOVULNCHECK ?= $(LOCALBIN)/govulncheck
GOTESTSUM ?= $(LOCALBIN)/gotestsum
GOCOVER_COBERTURA ?= $(LOCALBIN)/gocover-cobertura

## Tool Versions
# Go CLI tools are versioned in the nested tools/go.mod, via its `tool`
# directives. That module is not imported by anything here, so their
# dependency trees stay out of this module's graph and out of the graph any
# consumer of api/v1alpha1 inherits. Dependabot raises those pins under its
# own `gomod` entry for /tools, on the same weekly schedule and cooldown as
# the root module. The Makefile only reads the versions, so a local
# `go install` matches what CI resolved. Override any of these with an
# exported env var when you need a one-off local bump (`?=`).
#
# Non-Go pins such as KWOK_VERSION above stay manual — see .github/dependabot.yml.
CONTROLLER_TOOLS_VERSION ?= $(call gomodver,sigs.k8s.io/controller-tools,tools)
ADDLICENSE_VERSION ?= $(call gomodver,github.com/google/addlicense,tools)
# Pinned rather than installed with @latest so a CI run is reproducible and a
# local run resolves the same tool. An @latest scanner also means a new release
# can turn a green pipeline red with no commit here to explain it.
GOVULNCHECK_VERSION ?= $(call gomodver,golang.org/x/vuln,tools)
GOTESTSUM_VERSION ?= $(call gomodver,gotest.tools/gotestsum,tools)
GOCOVER_COBERTURA_VERSION ?= $(call gomodver,github.com/boumenot/gocover-cobertura,tools)
GOLANGCI_LINT_VERSION ?= $(call gomodver,github.com/golangci/golangci-lint/v2,tools)

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: addlicense
addlicense: $(ADDLICENSE) ## Download addlicense locally if necessary.
$(ADDLICENSE): $(LOCALBIN)
	$(call go-install-tool,$(ADDLICENSE),github.com/google/addlicense,$(ADDLICENSE_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: gotestsum
gotestsum: $(GOTESTSUM) ## Download gotestsum locally if necessary.
$(GOTESTSUM): $(LOCALBIN)
	$(call go-install-tool,$(GOTESTSUM),gotest.tools/gotestsum,$(GOTESTSUM_VERSION))

.PHONY: gocover-cobertura
gocover-cobertura: $(GOCOVER_COBERTURA) ## Download gocover-cobertura locally if necessary.
$(GOCOVER_COBERTURA): $(LOCALBIN)
	$(call go-install-tool,$(GOCOVER_COBERTURA),github.com/boumenot/gocover-cobertura,$(GOCOVER_COBERTURA_VERSION))

.PHONY: scan-vuln
scan-vuln: govulncheck ## Scan Go dependencies for known vulnerabilities.
	"$(GOVULNCHECK)" ./...

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
# Installed with `go install` like every other tool here rather than upstream's
# install.sh. That script was fetched from `master` — an unpinned reference that
# can change with no commit here — and its checksum check greps the tarball name
# unanchored against checksums.txt. Since v2.13.x ships a
# `<tarball>.tar.gz.sbom.json` asset, that grep matches two lines, so the
# comparison sees two hashes and fails on a tarball that downloaded correctly.
# `go install` pins the version and verifies it through the Go checksum database.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
#
# GOTOOLCHAIN is pinned to this module's Go version (go env GOVERSION). Without
# that, `go install pkg@version` follows the *tool* module's minimum Go and may
# build a binary too old to lint/build this tree (e.g. golangci-lint on go1.26
# refusing go 1.27.0 in go.mod).
define go-install-tool
@[ -n "$(3)" ] || { echo "Error: empty version for $(2); is it a tool in tools/go.mod?" >&2; exit 1; }; \
[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOTOOLCHAIN="$$(go env GOVERSION)" GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

# gomodver reads a module's resolved version out of a go.mod.
# $1 - module path to look up
# $2 - optional directory of the go.mod to read; defaults to this module's
#      root. Pass `tools` for the nested tool module.
define gomodver
$(shell go list $(if $(2),-C $(2),) -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
