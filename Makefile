BIN      := bin
CMDS     := cadre cadre-server cadre-runner
GOFLAGS  ?=

TOOLS                  := $(CURDIR)/$(BIN)/tools
CONTROLLER_GEN         := $(TOOLS)/controller-gen
CONTROLLER_GEN_VERSION := v0.23.0
GOLANGCI_LINT          := $(TOOLS)/golangci-lint
GOLANGCI_LINT_VERSION  := v2.15.0
ENVTEST                := $(TOOLS)/setup-envtest
ENVTEST_VERSION        := v0.25.2
ENVTEST_K8S_VERSION    := 1.37.0

.PHONY: all build test fmt vet lint check tidy clean generate manifests verify-generated tools

all: build

build: ## Build all binaries into bin/
	@mkdir -p $(BIN)
	@for c in $(CMDS); do echo "building $$c"; go build $(GOFLAGS) -o $(BIN)/$$c ./cmd/$$c; done

test: $(ENVTEST) ## Run unit tests, with envtest binaries for API tests
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(TOOLS)/envtest -p path)" go test -race -count=1 ./...

fmt: ## Check formatting
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

vet:
	go vet ./...

lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

generate: $(CONTROLLER_GEN) ## Generate deepcopy code for the API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: $(CONTROLLER_GEN) ## Generate CRD manifests into config/crd/bases
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

verify-generated: generate manifests ## Fail if generated files are out of date
	@test -z "$$(git status --porcelain -- api/*/zz_generated.deepcopy.go config/crd)" || { git status --short -- api/*/zz_generated.deepcopy.go config/crd; echo "generated files are stale: run make generate manifests and commit"; exit 1; }

tidy:
	go mod tidy

check: fmt vet lint verify-generated test ## Everything CI runs

tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) $(ENVTEST) ## Install pinned tools into bin/tools

$(CONTROLLER_GEN):
	GOBIN=$(TOOLS) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(GOLANGCI_LINT):
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(ENVTEST):
	GOBIN=$(TOOLS) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

clean:
	rm -rf $(BIN)
