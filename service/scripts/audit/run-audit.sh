#!/usr/bin/env bash
# run-audit.sh — executes the full OFFLINE audit battery for the
# SuroMesh Service (Phase 26). Never contacts the live Soroush Plus
# service; never requires credentials.
#
# Usage (from service/): bash scripts/audit/run-audit.sh
set -uo pipefail

PASS=0; FAIL=0; SKIP=0
record() { # name, status
  case "$2" in
    PASS) PASS=$((PASS+1)) ;;
    SKIPPED*) SKIP=$((SKIP+1)) ;;
    *) FAIL=$((FAIL+1)) ;;
  esac
  printf '%-60s %s\n' "$1" "$2"
}

echo "=== SuroMesh Service Audit Battery ==="
date

# Layer G — static analysis
gofmt -l . | grep -q . && record "gofmt" "FAIL" || record "gofmt" "PASS"
go vet ./... && record "go vet" "PASS" || record "go vet" "FAIL"
go mod tidy && git diff --exit-code -- go.mod go.sum >/dev/null \
  && record "go mod tidy (no drift)" "PASS" || record "go mod tidy (no drift)" "FAIL"

# Layer D — tests
go test -count=1 ./... >/dev/null 2>&1 && record "go test ./..." "PASS" || record "go test ./..." "FAIL"
go test -race -count=1 ./... >/dev/null 2>&1 && record "go test -race ./..." "PASS" || record "go test -race ./..." "FAIL"

# Layer B — conformance
go test -count=1 -run "TestSessionBlob|TestTranslate|TestConvertUpdate|TestConvertMessage|TestPeerConversion" \
  ./internal/infrastructure/upstream/ >/dev/null 2>&1 \
  && record "adapter conformance" "PASS" || record "adapter conformance" "FAIL"
go test -count=1 -run TestDomainDependencyBoundary ./internal/domain/ >/dev/null 2>&1 \
  && record "domain boundary" "PASS" || record "domain boundary" "FAIL"

# Layer F — security
go test -count=1 -run "TestAuthN|TestAuthZ|TestHealth|TestDeadline" ./internal/grpcapi/server/ >/dev/null 2>&1 \
  && record "gRPC authn/authz/health/deadline" "PASS" || record "gRPC authn/authz/health/deadline" "FAIL"

# Protobuf
command -v buf >/dev/null 2>&1 && { buf lint && record "buf lint" "PASS" || record "buf lint" "FAIL"; } \
  || record "buf lint" "SKIPPED (buf not on PATH)"

# Release policy (dev mode tolerated; strict mode must reject)
bash scripts/check-release.sh >/dev/null 2>&1 && record "release policy (dev)" "PASS" || record "release policy (dev)" "FAIL"
if RELEASE_BUILD=1 bash scripts/check-release.sh >/dev/null 2>&1; then
  record "release policy (strict rejects replace)" "FAIL"
else
  record "release policy (strict rejects replace)" "PASS"
fi

# Lifecycle smoke (offline fail-closed at storage)
go build -o /tmp/suromesh-service-audit ./cmd/service
out=$(SUROMESH_LISTEN_PLAINTEXT_DEV=true \
  SUROMESH_CRYPTO_FILE_KEY=$(mktemp) \
  SUROMESH_STORAGE_POSTGRES_DSN="postgres://offline-audit/none" \
  SUROMESH_LOG_FORMAT=text \
  /tmp/suromesh-service-audit 2>&1); code=$?
if [ "$code" = "1" ] && echo "$out" | grep -q "persistence failure"; then
  record "lifecycle smoke (boot + fail-closed storage)" "PASS"
else
  record "lifecycle smoke (boot + fail-closed storage)" "FAIL"
fi

# SDK regression (from repo root)
( cd .. && go test -count=1 ./... >/dev/null 2>&1 ) \
  && record "SDK regression (17 pkgs)" "PASS" || record "SDK regression (17 pkgs)" "FAIL"
( cd .. && go run ./cmd/tlgen -verify >/dev/null 2>&1 ) \
  && record "SDK generator verification" "PASS" || record "SDK generator verification" "FAIL"

# Integration tests (only with TEST_DATABASE_DSN)
if [ -n "${TEST_DATABASE_DSN:-}" ]; then
  TEST_DATABASE_DSN="$TEST_DATABASE_DSN" go test -count=1 -run TestPersistence \
    ./internal/infrastructure/persistence/ >/dev/null 2>&1 \
    && record "persistence integration (postgres)" "PASS" || record "persistence integration (postgres)" "FAIL"
else
  record "persistence integration" "SKIPPED (TEST_DATABASE_DSN not set)"
fi

echo "=== RESULT: PASS=$PASS FAIL=$FAIL SKIP=$SKIP ==="
[ "$FAIL" = "0" ]
