#!/usr/bin/env bash
# gates.sh — static quality gates (Phase 27 §4.2 items 1,2,6,7,9,10):
# formatting, module integrity, vet, staticcheck, code generation
# verification, protobuf/buf validation (lint + breaking vs baseline).
# Runs inside the CI runner container; exits non-zero on any failure.
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

cd "${SRC_ROOT}"
tool_versions | tee "${PHASE27_LOGS}/toolchain.txt"

step_begin "gofmt"
UNFORMATTED="$(gofmt -l . 2>/dev/null | grep -v '^service/gen/' || true)"
if [ -n "${UNFORMATTED}" ]; then
  echo "gofmt violations:"; echo "${UNFORMATTED}"
  step_end "gofmt" 1; exit 1
fi
step_end "gofmt" 0

step_begin "go-mod-integrity"
# GOFLAGS is -mod=readonly in the compose environment (correct for
# builds/tests); tidy needs write access, so it clears GOFLAGS.
( cd service && GOFLAGS= go mod tidy )
if [ -n "$(git -C "${SRC_ROOT}" diff --name-only -- service/go.mod service/go.sum)" ]; then
  echo "go.mod/go.sum drifted after 'go mod tidy' (not tidy or missing go.sum entries)"
  git -C "${SRC_ROOT}" diff -- service/go.mod service/go.sum
  step_end "go-mod-integrity" 1; exit 1
fi
( cd service && go mod verify )
step_end "go-mod-integrity" 0

step_begin "go-build"
( cd service && go build ./... )
step_end "go-build" 0

step_begin "go-vet"
( cd service && go vet ./... )
step_end "go-vet" 0

step_begin "staticcheck"
( cd service && staticcheck ./... )
step_end "staticcheck" 0

step_begin "sdk-build-vet"
# The SDK module must build and vet standalone (it never imports the service).
( go build ./... && go vet ./... )
step_end "sdk-build-vet" 0

step_begin "domain-boundary"
( cd service && go test -count=1 -run TestDomainDependencyBoundary ./internal/domain/ )
step_end "domain-boundary" 0

step_begin "buf-lint"
( cd service && buf lint )
step_end "buf-lint" 0

step_begin "buf-generate-verify"
( cd service && buf generate )
if [ -n "$(git -C "${SRC_ROOT}" status --porcelain -- service/gen/)" ]; then
  echo "generated code drifted; regenerate with: cd service && buf generate"
  git -C "${SRC_ROOT}" status --porcelain -- service/gen/
  step_end "buf-generate-verify" 1; exit 1
fi
step_end "buf-generate-verify" 0

step_begin "buf-breaking"
BASELINE="${SERVICE_ROOT}/.proto-baseline/baseline.binpb"
if [ ! -f "${BASELINE}" ]; then
  # First run on this checkout: establish the baseline (documented as the
  # Phase 27 baseline — no released API baseline exists before it).
  mkdir -p "$(dirname "${BASELINE}")"
  ( cd service && buf build api -o "${BASELINE}" )
  echo "BASELINE ESTABLISHED (Phase 27): ${BASELINE}"
  echo "The owner must commit it; subsequent runs fail on breaking changes."
else
  ( cd service && buf breaking api --against "${BASELINE}" )
  echo "breaking-change check PASSED against committed baseline"
fi
step_end "buf-breaking" 0

echo "GATES: ALL PASSED"
