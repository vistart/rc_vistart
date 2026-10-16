# rc_vistart - local development tasks.
#
# Requires Go 1.27+ on PATH. There is no remote CI for this project, so these
# targets are the whole verification story: run `make check` before pushing.

BINARY      := bin/rc_vistart
PKG         := ./...
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X github.com/vistart/rc_vistart/internal/version.Version=$(VERSION) \
	-X github.com/vistart/rc_vistart/internal/version.Commit=$(COMMIT) \
	-X github.com/vistart/rc_vistart/internal/version.Date=$(DATE)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the binary into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/rc_vistart

.PHONY: install
install: ## Install the binary into GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/rc_vistart

.PHONY: test
test: ## Run the test suite
	go test $(PKG)

.PHONY: test-race
test-race: ## Run the test suite under the race detector
	go test -race $(PKG)

.PHONY: cover
cover: ## Run the tests and write coverage.out plus an HTML report
	go test -coverprofile=coverage.out -covermode=atomic $(PKG)
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -n 1

.PHONY: bench
bench: ## Run benchmarks
	go test -run '^$$' -bench . -benchmem $(PKG)

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: fmt
fmt: ## Format the source tree
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if the source tree is not formatted
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then \
		echo "these files need gofmt:"; echo "$$out"; exit 1; \
	fi

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: check
check: fmt-check vet test ## Everything that must pass before pushing

.PHONY: run
run: ## Run the service locally on :8080
	go run ./cmd/rc_vistart

.PHONY: clean
clean: ## Remove build and coverage output
	rm -rf bin coverage.out coverage.html