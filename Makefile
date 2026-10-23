BIN      := bin
CMDS     := cadre cadre-server cadre-runner
GOFLAGS  ?=

.PHONY: all build test fmt vet lint check tidy clean

all: build

build: ## Build all binaries into bin/
	@mkdir -p $(BIN)
	@for c in $(CMDS); do echo "building $$c"; go build $(GOFLAGS) -o $(BIN)/$$c ./cmd/$$c; done

test: ## Run unit tests
	go test -race -count=1 ./...

fmt: ## Check formatting
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

vet:
	go vet ./...

lint: ## Run golangci-lint (must be installed)
	golangci-lint run ./...

tidy:
	go mod tidy

check: fmt vet lint test ## Everything CI runs

clean:
	rm -rf $(BIN)
