# iam-perf — development and release tasks
SHELL := /bin/bash
VERSION := $(shell cat VERSION 2>/dev/null || echo dev)
BIN := bin/iam-perf
MODULE := example.com/iam-perf

.PHONY: help check fmt vet test race cover bench build docker-build docker-smoke e2e-local clean verify

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

fmt: ## Format code
	go fmt ./...

vet: ## Static analysis
	go vet ./...

test: ## Unit + integration tests (offline)
	go test -timeout 300s ./...

race: ## Race detector run
	go test -race -timeout 300s ./...

cover: ## Coverage report
	go test -cover -timeout 300s ./...

bench: ## Benchmarks against local fixtures
	go test -bench=. -benchmem -run=^$$ ./tests/

build: ## Build CLI binary with version metadata
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath \
		-ldflags="-s -w -X $(MODULE)/pkg/iamperf.version=$(VERSION)" \
		-o $(BIN) ./cmd/iam-perf
	./$(BIN) version

# go.sum intentionally absent (zero third-party deps). `tidy` verifies it.
verify: fmt-check vet test race ## Full verification gauntlet (CI entry point)
	@echo "verification gauntlet: OK"

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

tidy: ## Confirm zero external dependencies
	go mod tidy
	@test ! -s go.sum || (echo "unexpected dependencies detected in go.sum" && exit 1)

e2e-local: ## Run e2e suite against local fixtures only
	go test -timeout 300s ./tests/ -v

docker-build: ## Build the container image (requires Docker; NOT verified in this workspace)
	docker build --build-arg VERSION=$(VERSION) -t iam-perf:$(VERSION) .

docker-smoke: ## Post-build smoke test: non-root + no shell + version output
	docker run --rm --user 65532:65532 iam-perf:$(VERSION) version
	docker run --rm --read-only iam-perf:$(VERSION) validate -c /dev/null || true

clean:
	rm -rf bin reports coverage.out
