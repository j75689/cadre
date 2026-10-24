BIN      := bin
CMDS     := cadre cadre-server cadre-runner
GOFLAGS  ?=

TOOLS                  := $(CURDIR)/$(BIN)/tools
CONTROLLER_GEN_VERSION := v0.23.0
GOLANGCI_LINT_VERSION  := v2.15.0
ENVTEST_VERSION        := v0.25.2
ENVTEST_K8S_VERSION    := 1.37.0

# Tool binaries carry their version, so bumping a version installs a new binary
# instead of silently reusing a stale one.
CONTROLLER_GEN := $(TOOLS)/controller-gen-$(CONTROLLER_GEN_VERSION)
GOLANGCI_LINT  := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
ENVTEST        := $(TOOLS)/setup-envtest-$(ENVTEST_VERSION)

# go-install installs package $(2) at version $(3) as the binary $(1).
define go-install
	@set -e; tmp=$$(mktemp -d); \
	GOBIN=$$tmp go install $(2)@$(3); \
	mkdir -p $(TOOLS); mv $$tmp/$$(basename $(2)) $(1); rm -rf $$tmp
endef

.PHONY: all build test smoke chaos fmt vet lint check tidy clean generate manifests verify-generated tools

all: build

build: ## Build all binaries into bin/
	@mkdir -p $(BIN)
	@for c in $(CMDS); do echo "building $$c"; go build $(GOFLAGS) -o $(BIN)/$$c ./cmd/$$c; done

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
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

vet:
	go vet ./...

lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

generate: $(CONTROLLER_GEN) ## Generate deepcopy code for the API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: $(CONTROLLER_GEN) ## Generate CRD manifests into config/crd/bases and cadre-server's RBAC into config/rbac
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=cadre-server paths=./internal/... output:rbac:artifacts:config=config/rbac

verify-generated: generate manifests ## Fail if generated files are out of date
	@test -z "$$(git status --porcelain -- api/*/zz_generated.deepcopy.go config/crd config/rbac)" || { git status --short -- api/*/zz_generated.deepcopy.go config/crd config/rbac; echo "generated files are stale: run make generate manifests and commit"; exit 1; }

tidy:
	go mod tidy

check: fmt vet lint verify-generated test ## Everything CI runs

tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ENVTEST) ## Install pinned tools into bin/tools

$(CONTROLLER_GEN):
	$(call go-install,$@,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

$(GOLANGCI_LINT):
	$(call go-install,$@,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

$(ENVTEST):
	$(call go-install,$@,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

clean:
	rm -rf $(BIN)
