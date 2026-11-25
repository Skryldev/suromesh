#!/usr/bin/env bash
# unit.sh — service unit tests (Phase 27 §4.2 item 3/4) with JUnit XML
# evidence. Explicit 10-minute hard timeout; no silent retries.
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

cd "${SERVICE_ROOT}"
step_begin "unit-tests"
timeout 600 gotestsum --junitfile "${JUNIT_DIR}/service-unit.xml" -- \
   -count=1 -timeout 8m ./...
step_end "unit-tests" 0

step_begin "sdk-unit-tests"
# SDK module: unit tests outside the race run (race.sh covers the full
# matrix). Must run from the SDK ROOT (its own module).
cd "${SRC_ROOT}"
timeout 900 gotestsum --junitfile "${JUNIT_DIR}/sdk-unit.xml" -- \
   -count=1 -timeout 12m ./...
step_end "sdk-unit-tests" 0

echo "UNIT: ALL PASSED"
