#!/usr/bin/env bash
# migration-cli.sh — validates the migrate CLI as an operator would use
# it (Phase 27 §5.4): fresh up, status output, schema verification, and
# TRUE multi-process concurrent migration (four independent processes
# race; the advisory lock must serialize them).
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

: "${TEST_DATABASE_ADMIN_DSN:?compose provides TEST_DATABASE_ADMIN_DSN}"

cd "${SERVICE_ROOT}"
step_begin "build-migrate-cli"
go build -o /tmp/suromesh-migrate ./cmd/migrate
step_end "build-migrate-cli" 0

LOG="${PHASE27_LOGS}/migration-cli.log"
: > "${LOG}"

# Fresh database for the CLI scenario (via psql on the admin DSN).
DB="mig_cli_$(date +%s)"
psql "${TEST_DATABASE_ADMIN_DSN}" -c "CREATE DATABASE ${DB}" | tee -a "${LOG}"
CLI_DSN="$(echo "${TEST_DATABASE_DSN}" | sed "s|/[^/?]*?|/${DB}?|")"
trap 'psql "${TEST_DATABASE_ADMIN_DSN}" -c "DROP DATABASE IF EXISTS ${DB} WITH (FORCE)" >/dev/null 2>&1 || true' EXIT

step_begin "migrate-cli-fresh-up"
SUROMESH_STORAGE_POSTGRES_DSN="${CLI_DSN}" /tmp/suromesh-migrate up | tee -a "${LOG}"
step_end "migrate-cli-fresh-up" 0

step_begin "migrate-cli-status"
SUROMESH_STORAGE_POSTGRES_DSN="${CLI_DSN}" /tmp/suromesh-migrate status | tee -a "${LOG}"
grep -q "0001_init" <(SUROMESH_STORAGE_POSTGRES_DSN="${CLI_DSN}" /tmp/suromesh-migrate status) || { echo "status output missing 0001_init"; exit 1; }
step_end "migrate-cli-status" 0

step_begin "migrate-cli-idempotent-rerun"
BEFORE="$(psql "${CLI_DSN}" -tAc "SELECT count(*) FROM schema_migrations")"
SUROMESH_STORAGE_POSTGRES_DSN="${CLI_DSN}" /tmp/suromesh-migrate up | tee -a "${LOG}"
AFTER="$(psql "${CLI_DSN}" -tAc "SELECT count(*) FROM schema_migrations")"
if [ "${BEFORE}" != "${AFTER}" ]; then
  echo "rerun changed ledger: ${BEFORE} -> ${AFTER} (reapplication!)"; exit 1
fi
step_end "migrate-cli-idempotent-rerun" 0

step_begin "migrate-cli-schema-validation"
# Structural validation of the resulting schema via psql.
psql "${CLI_DSN}" -c "\dt" | tee -a "${LOG}"
for table in schema_migrations tenants credentials accounts sessions operations audit_events; do
  psql "${CLI_DSN}" -tAc "SELECT to_regclass('public.${table}') IS NOT NULL" | grep -q t \
    || { echo "missing table ${table}"; exit 1; }
done
# Foreign-key integrity spot check.
FKS="$(psql "${CLI_DSN}" -tAc "SELECT count(*) FROM information_schema.table_constraints WHERE constraint_type='FOREIGN KEY' AND table_schema='public'")"
echo "foreign keys present: ${FKS}" | tee -a "${LOG}"
[ "${FKS}" -ge 5 ] || { echo "expected >= 5 foreign keys, got ${FKS}"; exit 1; }
step_end "migrate-cli-schema-validation" 0

step_begin "migrate-cli-concurrent-four-processes"
# TRUE multi-process race: four `migrate up` processes start together
# against the same database (the same situation multiple replicas or a
# double-invoked operator job would create). All must succeed; exactly
# one application; consistent ledger.
DB2="mig_cli_race_$(date +%s)"
psql "${TEST_DATABASE_ADMIN_DSN}" -c "CREATE DATABASE ${DB2}" | tee -a "${LOG}"
RACE_DSN="$(echo "${TEST_DATABASE_DSN}" | sed "s|/[^/?]*?|/${DB2}?|")"
pids=()
for i in 1 2 3 4; do
  SUROMESH_STORAGE_POSTGRES_DSN="${RACE_DSN}" /tmp/suromesh-migrate up >"/tmp/migrate-race-${i}.log" 2>&1 &
  pids+=($!)
done
rc=0
for pid in "${pids[@]}"; do
  wait "${pid}" || { echo "concurrent migrate process ${pid} FAILED:"; cat "/tmp/migrate-race-${pid}.log"; rc=1; }
done
grep -h "" /tmp/migrate-race-*.log | tee -a "${LOG}"
COUNT="$(psql "${RACE_DSN}" -tAc "SELECT count(*) FROM schema_migrations")"
[ "${COUNT}" = "1" ] || { echo "ledger rows = ${COUNT}, want exactly 1 (exactly-once)"; rc=1; }
psql "${TEST_DATABASE_ADMIN_DSN}" -c "DROP DATABASE IF EXISTS ${DB2} WITH (FORCE)" >/dev/null
step_end "migrate-cli-concurrent-four-processes" "${rc}"
[ "${rc}" -eq 0 ] || exit 1

echo "MIGRATION-CLI: ALL PASSED"
