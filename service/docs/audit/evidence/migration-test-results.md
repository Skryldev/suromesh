# Migration Test Results — Phase 27

Target system: the service's embedded migration runner
(`internal/infrastructure/persistence.Migrate`) + the new operator CLI
(`service/cmd/migrate up|status`), executed against real
`postgres:17-alpine` containers (tmpfs, per-run disposable).
Server identity recorded per run in `evidence/phase27/postgres-version.txt`.

## Runner changes made in Phase 27 (evidence-driven)

1. **Cross-migrator serialization** — the whole run now takes a
   session-level `pg_advisory_lock` on a dedicated connection (released
   even on cancellation; a crashed migrator cannot wedge the lock).
   Justification: Scenario E showed nothing prevented two migrators from
   racing the version check → double-apply.
2. **`VerifySchema` startup gate** — required tables + critical columns
   are checked after migration; the application fails closed on an
   incompatible schema (tampered/partial-restore databases are refused).
3. **Operator CLI** (`cmd/migrate`) — `up` (migrate + verify) and
   `status` (known-vs-applied, UNKNOWN-TO-BINARY detection for
   downgraded binaries). Deterministic exit codes 0/1/2.
4. **Schema corrections exposed by real-PostgreSQL testing**
   (all three were invisible to the in-memory harness):
   - removed the GLOBAL unique index on `accounts.phone_hash` that
     contradicted tenant-scoped uniqueness (multi-tenant blocker);
   - `operations.result_ref` NULL-safe scan (`*string`);
   - `CHAR(n)` columns → `TEXT` (blank-padding made replay comparisons
     fail after round-trip).

## Scenario results (all executed in this phase's pipeline)

| Scenario | Test / step | Result | Notes |
|---|---|---|---|
| A — Fresh installation | `TestMigrationScenario_A_FreshInstall` | PASS | empty DB → 0001 applied once → tables/constraints verified → tenant round trip on the migrated DB |
| B — Idempotency | `TestMigrationScenario_B_IdempotentRerun` | PASS | second run: ledger + table set byte-identical; VerifySchema passes |
| C — Interrupted migration | `TestMigrationScenario_C_InterruptedMigrationRollback` | PASS | controlled mid-migration failure: full statement rollback inside the migration's transaction, ledger has no failed version, roll-forward recovery re-run succeeds |
| D — Existing DB upgrade | `TestMigrationScenario_D_UpgradePreservesData` | PASS | representative rows at 0001 + later migration (add column + backfill): data preserved, backfill correct, both versions in ledger |
| E — Concurrent migrators | `TestMigrationScenario_E_ConcurrentMigrationRuns` | PASS | 8 concurrent Migrate calls (independent pgx connections): advisory lock serializes, all succeed, exactly-once application, consistent ledger |
| E′ — Multi-PROCESS race | `migration-cli.sh` (4 simultaneous `migrate up` processes) | PASS | the true inter-process case: all exit 0, ledger has exactly 1 row |
| B′ — CLI idempotency | `migration-cli.sh` | PASS | ledger row count unchanged across reruns |
| F — Application compatibility | e2e profile: service boots on migrated DB (VerifySchema gate) + 11 persistence tests | PASS | Scenario F coverage |

## Recovery semantics (documented, verified)

- Roll-forward ONLY. There are no down-migrations and none are claimed.
  A failed migration = fix the migration + re-run (Scenario C proves the
  failed attempt leaves NO schema residue and NO ledger entry).
- Concurrent operators/replicas are SAFE (advisory lock); the loser
  waits, then finds nothing to do.
- Startup behavior on incompatible schema: hard fail (`VerifySchema`),
  never silent service against a partial/tampered schema.

## Operator commands (Docker-native; full doc in docker-ci.md)

```bash
# isolated PG (dev/test; NEVER production)
docker compose -f service/deploy/compose/ci.compose.yml --profile migration up -d postgres-test

# apply / status / inspect (inside the CI image)
docker compose -f service/deploy/compose/ci.compose.yml --profile migration run --rm ci bash -c \
  'go build -o /tmp/migrate ./cmd/migrate && SUROMESH_STORAGE_POSTGRES_DSN="$TEST_DATABASE_DSN" /tmp/migrate up'
docker compose -f service/deploy/compose/ci.compose.yml --profile migration run --rm ci bash -c \
  'SUROMESH_STORAGE_POSTGRES_DSN="$TEST_DATABASE_DSN" /tmp/migrate status'
docker compose -f service/deploy/compose/ci.compose.yml --profile migration run --rm ci psql "$TEST_DATABASE_DSN" -c '\dt'

# reset the disposable test database (safe — ephemeral by construction)
docker compose -f service/deploy/compose/ci.compose.yml --profile migration down -v
```

Production reset/destructive operations are absent from every default
command by design.

Raw outputs: `evidence/phase27/migration-scenarios.log`,
`migration-cli.log`, `postgres-version.txt`, JUnit
`migration-scenarios.xml`.
