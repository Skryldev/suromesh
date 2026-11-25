# SuroMesh — Docker-native CI/CD entry points (Phase 27).
#
# The host needs ONLY Docker Engine + Docker Compose + GNU make. Every
# check executes inside containers; see scripts/ci.sh and
# service/docs/operations/docker-ci.md for the full architecture and
# operator procedures.
#
# NOTE: the canonical entry point works WITHOUT make too:
#   ./scripts/ci.sh ci          (bash + docker only)

SHELL := /bin/bash
CI := ./scripts/ci.sh

.PHONY: help ci test race integration migration e2e security build smoke release clean

help:
	@echo "SuroMesh Docker-native CI/CD (host needs only Docker + make):"
	@echo "  make ci          full qualification pipeline (fail-fast)"
	@echo "  make test        static gates + unit tests + bounded fuzz"
	@echo "  make race        race-detector suites (SDK + service)"
	@echo "  make integration PostgreSQL integration tests (Dockerized PG)"
	@echo "  make migration   migration scenarios + multi-process CLI race"
	@echo "  make e2e         E2E gRPC against the real service container"
	@echo "  make security    govulncheck + crypto battery + secret scan"
	@echo "  make build       production image build"
	@echo "  make smoke       container runtime qualification"
	@echo "  make release     release policy + artifact validation"
	@echo "  make clean       remove run containers/networks/ephemeral state"

ci:          ; $(CI) ci
test:        ; $(CI) test
race:        ; $(CI) race
integration: ; $(CI) integration
migration:   ; $(CI) migration
e2e:         ; $(CI) e2e
security:    ; $(CI) security
build:       ; $(CI) build
smoke:       ; $(CI) smoke
release:     ; $(CI) release
clean:       ; $(CI) clean
