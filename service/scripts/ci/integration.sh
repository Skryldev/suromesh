#!/usr/bin/env bash
# integration.sh — PostgreSQL integration tests + migration scenarios
# against the real Dockerized PostgreSQL (Phase 27 §4.2 item 12, §5).
# TEST_DATABASE_DSN / TEST_DATABASE_ADMIN_DSN are provided by the compose
# environment (ephemeral per-run database; never production).
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

: "${TEST_DATABASE_DSN:?compose provides TEST_DATABASE_DSN}"
: "${TEST_DATABASE_ADMIN_DSN:?compose provides TEST_DATABASE_ADMIN_DSN}"

step_begin "postgres-readiness"
for i in $(seq 1 30); do
  if psql "${TEST_DATABASE_DSN}" -c "SELECT 1" >/dev/null 2>&1; then
    echo "postgres ready (attempt ${i})"
    break
  fi
  if [ "${i}" -eq 30 ]; then
    echo "postgres never became ready"; step_end "postgres-readiness" 1; exit 1
  fi
  sleep 1
done
psql "${TEST_DATABASE_DSN}" -c "SELECT version();" | tee "${PHASE27_LOGS}/postgres-version.txt"
step_end "postgres-readiness" 0

cd "${SERVICE_ROOT}"
step_begin "integration-tests"
timeout 600 gotestsum --junitfile "${JUNIT_DIR}/integration.xml" -- \
   -race -count=1 -timeout 8m \
    -run "TestPersistence" ./internal/infrastructure/persistence/
step_end "integration-tests" 0

step_begin "migration-scenarios"
timeout 600 gotestsum --junitfile "${JUNIT_DIR}/migration-scenarios.xml" -- \
   -count=1 -timeout 8m \
    -run "TestMigrationScenario" -v ./internal/infrastructure/persistence/ \
    | tee "${PHASE27_LOGS}/migration-scenarios.log"
step_end "migration-scenarios" 0

echo "INTEGRATION: ALL PASSED"
