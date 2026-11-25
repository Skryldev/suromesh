#!/usr/bin/env bash
# common.sh — shared helpers for the in-container CI scripts.
# Sourced by every script in this directory. Enforces:
#   - errexit, nounset, pipefail (a failing command can never hide);
#   - a single evidence root for logs and machine-readable artifacts;
#   - step markers that scripts/ci.sh parses into the evidence bundle.

set -euo pipefail

SRC_ROOT="${SRC_ROOT:-/src}"
SERVICE_ROOT="${SRC_ROOT}/service"
EVIDENCE="${SERVICE_ROOT}/docs/audit/evidence"
PHASE27_LOGS="${EVIDENCE}/phase27"
JUNIT_DIR="${PHASE27_LOGS}/junit"
mkdir -p "${PHASE27_LOGS}" "${JUNIT_DIR}"

step_begin() { echo "STEP $1 BEGIN  $(date -u +%Y-%m-%dT%H:%M:%SZ)"; }
step_end()   { echo "STEP $1 END    $(date -u +%Y-%m-%dT%H:%M:%SZ) rc=$2"; }

tool_versions() {
  # Print the exact toolchain identity used for this run (evidence).
  echo "go: $(go version)"
  echo "buf: $(buf --version 2>/dev/null || echo missing)"
  echo "protoc-gen-go: $(protoc-gen-go --version 2>/dev/null || echo missing)"
  echo "protoc-gen-go-grpc: $(protoc-gen-go-grpc --version 2>/dev/null || echo missing)"
  echo "staticcheck: $(staticcheck -version 2>/dev/null || echo missing)"
  echo "govulncheck: $(govulncheck -version 2>/dev/null | head -1 || echo missing)"
  echo "gotestsum: $(gotestsum --version 2>/dev/null || echo missing)"
  echo "psql: $(psql --version 2>/dev/null || echo missing)"
}

die() { echo "FATAL: $*" >&2; exit 1; }
