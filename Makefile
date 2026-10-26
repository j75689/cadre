BIN      := bin
CMDS     := cadre cadre-server cadre-runner
GOFLAGS  ?=
REGISTRY ?= ghcr.io/j75689
TAG      ?= dev

TOOLS                  := $(CURDIR)/$(BIN)/tools
CONTROLLER_GEN_VERSION := v0.23.0
GOLANGCI_LINT_VERSION  := v2.15.0
ENVTEST_VERSION        := v0.25.2
ENVTEST_K8S_VERSION    := 1.37.0
HELM_VERSION           := v4.3.1
KUBECONFORM_VERSION    := v0.8.0
SHELLCHECK_VERSION     := v0.11.0
# kind and kubectl for make e2e, downloaded by hack/e2e/run.sh, which holds
# their checksums.
KIND_VERSION           := v0.33.0
KUBECTL_VERSION        := v1.37.0

# Tool binaries carry their version, so bumping a version installs a new binary
# instead of silently reusing a stale one.
CONTROLLER_GEN := $(TOOLS)/controller-gen-$(CONTROLLER_GEN_VERSION)
GOLANGCI_LINT  := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
ENVTEST        := $(TOOLS)/setup-envtest-$(ENVTEST_VERSION)
HELM           := $(TOOLS)/helm-$(HELM_VERSION)
KUBECONFORM    := $(TOOLS)/kubeconform-$(KUBECONFORM_VERSION)
SHELLCHECK     := $(TOOLS)/shellcheck-$(SHELLCHECK_VERSION)

# go-install installs package $(2) at version $(3) as the binary $(1).
define go-install
	@set -e; tmp=$$(mktemp -d); \
	GOBIN=$$tmp go install $(2)@$(3); \
	mkdir -p $(TOOLS); mv $$tmp/$$(basename $(2)) $(1); rm -rf $$tmp
endef

.PHONY: all build test smoke chaos fmt vet lint check tidy clean generate manifests verify-generated verify-tidy tools image images ui ui-check chart-files chart-lint chart-dist print-chart-paths print-helm e2e print-e2e-tools install-cadre-cli cli-dist print-cli-dist

all: build

build: ## Build all binaries into bin/; for cadre-server with the web UI, run make ui first
	@mkdir -p $(BIN)
	@for c in $(CMDS); do echo "building $$c"; go build $(GOFLAGS) -o $(BIN)/$$c ./cmd/$$c; done

# install-cadre-cli installs the cadre CLI from this checkout, with go
# install, so it lands where go install puts binaries: go env GOBIN, or
# bin under the first GOPATH entry.
VERSION ?= $(shell git describe --tags --always --dirty)
install-cadre-cli: ## Install the cadre CLI into go env GOBIN (or GOPATH/bin) [VERSION=...]
	go install -trimpath -ldflags "-X main.version=$(VERSION)" ./cmd/cadre
	@dir=$$(go env GOBIN); if [ -z "$$dir" ]; then gopath=$$(go env GOPATH); dir=$${gopath%%:*}/bin; fi; \
		echo "installed $$dir/cadre ($(VERSION)); make sure $$dir is on your PATH"

# web/node_modules is installed from the lockfile, again when it changes.
web/node_modules/.package-lock.json: web/package-lock.json
	cd web && npm ci
	@touch $@

ui: web/node_modules/.package-lock.json ## Build the web UI into internal/web/dist, which cadre-server embeds (needs Node, see web/.nvmrc)
	cd web && npm run build

ui-check: web/node_modules/.package-lock.json ## Typecheck, lint and unit-test the web UI
	cd web && npm run typecheck && npm run lint && npm test

image: ## Build one command's image: make image CMD=cadre-server [TAG=dev]
	@case " $(CMDS) " in *" $(CMD) "*) [ -n "$(CMD)" ] ;; *) false ;; esac || { echo "set CMD to one of: $(CMDS)"; exit 1; }
	DOCKER_BUILDKIT=1 docker build --build-arg CMD=$(CMD) --build-arg VERSION=$(TAG) -t $(REGISTRY)/$(CMD):$(TAG) .

images: ## Build every command's image [TAG=dev]
	@for c in $(CMDS); do $(MAKE) --no-print-directory image CMD=$$c TAG=$(TAG) || exit 1; done

# e2e installs the chart from the images built at this commit (E2E_CMDS:
# the server's and the runner's, all it deploys) into a kind
# cluster, with Dex and SeaweedFS, and checks it through the API and the UI.
# It needs Docker; KEEP=1 keeps the cluster.
E2E_TAG ?= e2e-$(shell git rev-parse --short HEAD)
E2E_CMDS := cadre-server cadre-runner
e2e: ## Check an install end to end in kind (hack/e2e) [KEEP=1]
	@for c in $(E2E_CMDS); do $(MAKE) --no-print-directory image CMD=$$c TAG=$(E2E_TAG) || exit 1; done
	hack/e2e/run.sh $(E2E_TAG)

print-e2e-tools: ## Print the kind and kubectl versions make e2e uses, then its images
	@echo $(KIND_VERSION) $(KUBECTL_VERSION) $(E2E_CMDS)

test: $(ENVTEST) ## Run all tests, the chaos test in its short form; downloads envtest binaries on first run
	assets=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOLS)/envtest -p path) && \
	KUBEBUILDER_ASSETS="$$assets" go test -race -short -count=1 ./...

smoke: $(ENVTEST) ## Run the builtin loop against the real Anthropic API, authenticated with a real ServiceAccount token (needs CADRE_SMOKE_ANTHROPIC_KEY; costs a few cents)
	@test -n "$$CADRE_SMOKE_ANTHROPIC_KEY" || { echo "set CADRE_SMOKE_ANTHROPIC_KEY"; exit 1; }
	assets=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOLS)/envtest -p path) && \
	KUBEBUILDER_ASSETS="$$assets" go test -tags smoke -count=1 -v -run TestSmoke ./internal/provider/anthropic/

chaos: $(ENVTEST) ## Run the full 60s chaos test locally (CADRE_CHAOS_SEED, CADRE_CHAOS_DURATION)
	assets=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOLS)/envtest -p path) && \
	KUBEBUILDER_ASSETS="$$assets" go test -race -count=1 -timeout 15m -v -run TestChaos ./test/chaos/

fmt: ## Check formatting
	@dirs=$$(go list -f '{{.Dir}}' ./...); test -z "$$(gofmt -l $$dirs)" || { echo "unformatted files:"; gofmt -l $$dirs; exit 1; }

vet:
	go vet ./...

lint: $(GOLANGCI_LINT) $(SHELLCHECK) ## Run golangci-lint, and shellcheck on the installer where it is pinned
	$(GOLANGCI_LINT) run ./...
	@if [ -x $(SHELLCHECK) ]; then echo "$(SHELLCHECK) -s sh hack/install.sh"; $(SHELLCHECK) -s sh hack/install.sh; fi

generate: $(CONTROLLER_GEN) ## Generate deepcopy code for the API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: $(CONTROLLER_GEN) ## Generate CRD manifests into config/crd/bases and cadre-server's RBAC into config/rbac, and copy them into the chart
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=cadre-server paths=./internal/... output:rbac:artifacts:config=config/rbac
	@$(MAKE) --no-print-directory chart-files

# The chart carries the CRDs and the RBAC rules as they are generated: the
# chart's templates read the rules from files/, so they cannot drift.
chart-files: ## Copy the CRDs and RBAC into charts/cadre
	@rm -f charts/cadre/crds/*.yaml && cp config/crd/bases/*.yaml charts/cadre/crds/
	@cp config/rbac/role.yaml charts/cadre/files/server-role.yaml
	@cp config/rbac/workloads.yaml charts/cadre/files/server-workloads.yaml
	@cp config/runner/rbac.yaml charts/cadre/files/runner-role.yaml

# chart-lint lints the chart and validates what it renders against
# Kubernetes' schemas, for each values file under ci/. The CRDs are left out:
# kubeconform's schema catalogue has none for CustomResourceDefinition, and
# envtest loads them in every test run. The render goes to a variable first,
# so that a failing helm template fails the target.
chart-lint: $(HELM) $(KUBECONFORM) ## Lint the Helm chart and validate its output
	@set -e; for f in charts/cadre/ci/*.yaml; do \
		echo "chart: $$f"; \
		$(HELM) lint --strict charts/cadre -f $$f; \
		out=$$($(HELM) template cadre charts/cadre -n cadre-system -f $$f); \
		printf '%s\n' "$$out" | $(KUBECONFORM) -strict -summary -kubernetes-version $(ENVTEST_K8S_VERSION) -; \
	done

# chart-dist packages the chart at RELEASE (a vX.Y.Z tag, semver: chart
# version X.Y.Z, appVersion the tag) and renders install.yaml from it, both
# into dist/; print-chart-paths prints the two paths.
CHART_VERSION = $(patsubst v%,%,$(RELEASE))
CHART_PKG     = dist/cadre-$(CHART_VERSION).tgz
# A release has no +build part: GitHub turns the + into a . in asset names,
# which then match neither the checksums file nor install.sh.
check-release = @echo "$(RELEASE)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo "RELEASE must be a vX.Y.Z tag without +build (GitHub renames + in release assets), not '$(RELEASE)'"; exit 1; }
chart-dist: $(HELM) ## Package the chart and render install.yaml for RELEASE=vX.Y.Z
	$(check-release)
	@mkdir -p dist
	$(HELM) package charts/cadre --version $(CHART_VERSION) --app-version $(RELEASE) -d dist
	@set -e; out=$$($(HELM) template cadre $(CHART_PKG) -n cadre-system --include-crds \
		--set auth.mode=none --set storage.s3.bucket=CHANGE-ME); \
		{ cat hack/install-header.yaml; printf '%s\n' "$$out"; } > dist/install.yaml

print-chart-paths: ## Print chart-dist's package and install.yaml for RELEASE=vX.Y.Z
	$(check-release)
	@echo "$(CHART_PKG) dist/install.yaml"

# cli-dist cross-compiles the cadre CLI at RELEASE (a vX.Y.Z tag) for each
# of CLI_PLATFORMS into CLI_DIST: cadre_<version>_<os>_<arch>.tar.gz with
# cadre and LICENSE (a .zip with cadre.exe for windows), <version> without
# its v, and cadre_<version>_checksums.txt over them in sha256sum's format.
# The builds carry no VCS stamp (the version is the tag), and hack/archive
# writes the archives the same way on every host: sorted entries owned by
# 0:0 with SOURCE_DATE_EPOCH (by default the commit's time) as their time.
# So a rebuild of a commit gives the same checksums.
CLI_PLATFORMS ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64
CLI_DIST      ?= dist
CLI_VERSION    = $(patsubst v%,%,$(RELEASE))
CLI_SUMS       = $(CLI_DIST)/cadre_$(CLI_VERSION)_checksums.txt
# cli-names defines, for the recipes' shell, name OS ARCH: an archive's
# path, and exe OS: the binary's name in it.
cli-names = exe() { if [ "$$1" = windows ]; then echo cadre.exe; else echo cadre; fi; }; \
	name() { ext=tar.gz; [ "$$1" != windows ] || ext=zip; echo "$(CLI_DIST)/cadre_$(CLI_VERSION)_$$1_$$2.$$ext"; }
cli-dist: ## Build the cadre CLI's release archives and checksums for RELEASE=vX.Y.Z [CLI_PLATFORMS=os/arch ...]
	$(check-release)
	@set -e; $(cli-names); mkdir -p $(CLI_DIST); tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	epoch=$${SOURCE_DATE_EPOCH:-$$(git log -1 --format=%ct)}; \
	go build -o "$$tmp/archive" ./hack/archive; archives=; \
	for p in $(CLI_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; bin=$$(exe $$os); out=$$(name $$os $$arch); \
		echo "building cadre $(RELEASE) for $$p"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$(RELEASE)" -o "$$tmp/$$p/$$bin" ./cmd/cadre; \
		"$$tmp/archive" -mtime "$$epoch" -o "$$out" "$$bin=$$tmp/$$p/$$bin" LICENSE=LICENSE; \
		archives="$$archives $$out"; \
	done; \
	"$$tmp/archive" -sums $(CLI_SUMS) $$archives

print-cli-dist: ## Print cli-dist's archives, then its checksums file, for RELEASE=vX.Y.Z
	$(check-release)
	@$(cli-names); for p in $(CLI_PLATFORMS); do printf '%s ' "$$(name $${p%/*} $${p#*/})"; done; echo $(CLI_SUMS)

print-helm: $(HELM) ## Print the pinned helm's path, installing it
	@echo $(HELM)

verify-generated: generate manifests ## Fail if generated files are out of date
	@test -z "$$(git status --porcelain -- api/*/zz_generated.deepcopy.go config/crd config/rbac charts/cadre/crds charts/cadre/files)" || { git status --short -- api/*/zz_generated.deepcopy.go config/crd config/rbac charts/cadre/crds charts/cadre/files; echo "generated files are stale: run make generate manifests and commit"; exit 1; }

tidy:
	go mod tidy

verify-tidy: ## Fail if go.mod or go.sum is not tidy
	go mod tidy && git diff --exit-code go.mod go.sum

check: verify-tidy fmt vet lint verify-generated chart-lint ui-check test ## Everything CI runs

tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ENVTEST) $(HELM) $(KUBECONFORM) $(SHELLCHECK) ## Install pinned tools into bin/tools

$(CONTROLLER_GEN):
	$(call go-install,$@,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

$(GOLANGCI_LINT):
	$(call go-install,$@,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

$(ENVTEST):
	$(call go-install,$@,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

$(HELM):
	$(call go-install,$@,helm.sh/helm/v4/cmd/helm,$(HELM_VERSION))

$(KUBECONFORM):
	$(call go-install,$@,github.com/yannh/kubeconform/cmd/kubeconform,$(KUBECONFORM_VERSION))

# shellcheck is not a Go tool: its release archive, checked against these
# SHA-256 sums, one per platform, which sha256 prints in a recipe (change them with the version). A
# platform without one (armv7l, riscv64, FreeBSD) gets no shellcheck, with
# a warning, and lint goes on without it.
shellcheck-platform = $$(uname -s | tr A-Z a-z).$$(uname -m | sed s/arm64/aarch64/)
shellcheck-sha256 = sha256() { case $(shellcheck-platform) in \
	linux.x86_64) echo b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6 ;; \
	linux.aarch64) echo 68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc ;; \
	darwin.x86_64) echo c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51 ;; \
	darwin.aarch64) echo 339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f ;; \
	esac; }
$(SHELLCHECK):
	@set -e; $(shellcheck-sha256); want=$$(sha256); \
	[ -n "$$want" ] || { echo "warning: shellcheck $(SHELLCHECK_VERSION) is not pinned for $(shellcheck-platform): skipping it"; exit 0; }; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	name=shellcheck-$(SHELLCHECK_VERSION).$(shellcheck-platform).tar.gz; \
	curl -fsSL -o "$$tmp/$$name" https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/$$name; \
	if command -v sha256sum >/dev/null; then got=$$(sha256sum "$$tmp/$$name"); else got=$$(shasum -a 256 "$$tmp/$$name"); fi; \
	got=$${got%% *}; [ "$$got" = "$$want" ] || { echo "$$name: SHA-256 $$got, want $$want"; exit 1; }; \
	tar -xzf "$$tmp/$$name" -C "$$tmp"; mkdir -p $(TOOLS); mv "$$tmp/shellcheck-$(SHELLCHECK_VERSION)/shellcheck" $@

clean:
	rm -rf $(BIN) dist
