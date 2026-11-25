#!/usr/bin/env bash
# e2e.sh — end-to-end tests: the e2eclient talks real gRPC to the real
# service container (Phase 27 §4.2 item 13, §10). The service container
# is started by the compose e2e profile; this script builds the client
# and drives the full contract battery over the wire. No live Soroush
# Plus account is required or contacted.
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

: "${E2E_TARGET:?compose provides E2E_TARGET=service:50051}"
: "${E2E_BOOTSTRAP_KEY:?compose injects the per-run bootstrap key}"

cd "${SERVICE_ROOT}"
step_begin "build-e2e-client"
go build -o /tmp/suromesh-e2eclient ./cmd/e2eclient
step_end "build-e2e-client" 0

step_begin "e2e-wire-tests"
E2E_TARGET="${E2E_TARGET}" E2E_BOOTSTRAP_KEY="${E2E_BOOTSTRAP_KEY}" \
  timeout 180 /tmp/suromesh-e2eclient | tee "${PHASE27_LOGS}/e2e-results.txt"
step_end "e2e-wire-tests" 0

echo "E2E: ALL PASSED"
