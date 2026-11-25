#!/usr/bin/env bash
# race.sh — race-detector suites: SDK module + service module
# (Phase 27 §4.2 item 5, §15). Hard timeouts; JUnit evidence.
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

cd "${SRC_ROOT}"
step_begin "sdk-race"
timeout 1200 gotestsum --junitfile "${JUNIT_DIR}/sdk-race.xml" -- \
   -race -count=1 -timeout 15m ./...
step_end "sdk-race" 0

cd "${SERVICE_ROOT}"
step_begin "service-race"
timeout 900 gotestsum --junitfile "${JUNIT_DIR}/service-race.xml" -- \
   -race -count=1 -timeout 10m ./...
step_end "service-race" 0

echo "RACE: ALL PASSED"
