# SuroMesh Service — developer commands.
# All targets run from service/ (or via make -C service).

GO ?= go
BUF ?= buf

.PHONY: help build vet test race fmt tidy proto proto-lint proto-breaking proto-drift release-policy integration smoke all

help:
	@echo "build           compile all packages"
	@echo "vet             go vet"
	@echo "test            unit tests"
	@echo "race            race detector"
	@echo "fmt             gofmt -l (report) + -w (fix)"
	@echo "tidy            go mod tidy"
	@echo "proto           buf generate (protoc-gen-go + protoc-gen-go-grpc on PATH)"
	@echo "proto-lint      buf lint"
	@echo "proto-breaking  buf breaking against the last generated snapshot"
	@echo "proto-drift     regenerate + git diff (fails on drift)"
	@echo "release-policy  reject replace directives (release builds)"
	@echo "integration     persistence integration tests (TEST_DATABASE_DSN required)"
	@echo "smoke           boot the binary for 3s (liveness check)"
	@echo "all             fmt tidy build vet test race proto-lint proto-drift"

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

fmt:
	gofmt -w .
	gofmt -l .

tidy:
	$(GO) mod tidy

proto:
	$(BUF) generate

proto-lint:
	$(BUF) lint

proto-breaking:
	$(BUF) breaking --against $(shell pwd)/../.proto-baseline

proto-drift:
	$(BUF) generate
	git diff --exit-code -- gen/ || { echo "generated code drifted; run: buf generate"; exit 1; }

release-policy:
	bash scripts/check-release.sh

integration:
	TEST_DATABASE_DSN=$${TEST_DATABASE_DSN:=$(TEST_DSN)} $(GO) test -count=1 -run TestPersistence ./internal/infrastructure/persistence/ -v

smoke:
	$(GO) build -o /tmp/suromesh-service ./cmd/service
	SUROMESH_LISTEN_PLAINTEXT_DEV=true \
	SUROMESH_CRYPTO_FILE_KEY=$$(mktemp) \
	SUROMESH_STORAGE_POSTGRES_DSN=postgres://invalid/ignore \
	SUROMESH_LOG_FORMAT=text \
	timeout 3 /tmp/suromesh-service; code=$$?; \
	if [ "$$code" = "124" ]; then echo "smoke OK (running at kill)"; else echo "smoke exit $$code"; exit 1; fi

all: fmt tidy build vet test race proto-lint proto-drift release-policy
