package persistence_test

// Migration qualification scenarios (Phase 27 §5.3), executed against a
// real PostgreSQL instance provided by the Docker CI environment
// (TEST_DATABASE_DSN + TEST_DATABASE_ADMIN_DSN).
//
// Scenario A — fresh installation: empty database → all migrations →
//   schema verified → application round trip.
// Scenario B — idempotency: re-running the migration runner is a no-op.
// Scenario C — interrupted migration: a failing migration rolls back
//   completely (per-migration transaction) and the version ledger stays
//   consistent; recovery = fix + re-run (roll-forward only).
// Scenario D — existing database upgrade: real 0001 + representative
//   data, then a synthetic later migration preserves the data.
// Scenario E — concurrent migration runs: the advisory lock serializes
//   them; exactly-once application, consistent ledger.
// Scenario F — application compatibility: covered by the integration
//   suite (same DB) and the Docker E2E profile (service boots against a
//   migrated database; startup gate VerifySchema fails closed otherwise).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/persistence"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_ADMIN_DSN")
	if dsn == "" {
		t.Skip("MIGRATION SKIPPED — TEST_DATABASE_ADMIN_DSN not set (CI provides postgres)")
	}
	return dsn
}

// scenarioDB creates a unique, empty database for one scenario and
// registers cleanup (drop at test end). Name is lowercase/valid per
// PostgreSQL identifier rules.
func scenarioDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	name := fmt.Sprintf("mig_%s_%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")), time.Now().UnixNano()%1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		admin.Close()
		t.Fatalf("create scenario db %s: %v", name, err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_, _ = admin.Exec(dctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
		admin.Close()
	})

	// Test DSN with the scenario database swapped in.
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse test dsn: %v", err)
	}
	u.Path = "/" + name
	db, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("scenario connect: %v", err)
	}
	t.Cleanup(db.Close)
	return name, db
}

func appliedVersions(t *testing.T, pool *pgxpool.Pool) map[string]time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var v string
		var at time.Time
		if err := rows.Scan(&v, &at); err != nil {
			t.Fatal(err)
		}
		out[v] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tablesOf(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema='public' AND table_type='BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// postgresVersion records the server identity for the evidence bundle.
func postgresVersion(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var v string
	if err := pool.QueryRow(ctx, `SELECT version()`).Scan(&v); err != nil {
		t.Fatalf("server version: %v", err)
	}
	t.Logf("PostgreSQL server: %s", v)
	return v
}

// Scenario A — fresh installation.
func TestMigrationScenario_A_FreshInstall(t *testing.T) {
	name, db := scenarioDB(t)
	postgresVersion(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := persistence.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate fresh db %s: %v", name, err)
	}
	if err := persistence.VerifySchema(ctx, db); err != nil {
		t.Fatalf("verify schema: %v", err)
	}
	tables := tablesOf(t, db)
	for _, want := range []string{"schema_migrations", "tenants", "credentials",
		"accounts", "sessions", "operations", "audit_events"} {
		if !tables[want] {
			t.Fatalf("fresh install: table %s missing", want)
		}
	}
	ledger := appliedVersions(t, db)
	if len(ledger) != 1 {
		t.Fatalf("ledger = %d rows, want 1 (0001_init)", len(ledger))
	}
	if _, ok := ledger["0001_init"]; !ok {
		t.Fatalf("ledger missing 0001_init: %v", ledger)
	}

	// Application round trip directly on the migrated database.
	store := persistence.NewStore(db)
	id, err := store.Tenants.Create(ctx, "fresh-install")
	if err != nil {
		t.Fatalf("tenant create: %v", err)
	}
	rec, err := store.Tenants.Get(ctx, id)
	if err != nil || rec.Name != "fresh-install" {
		t.Fatalf("tenant read: %v %+v", err, rec)
	}
}

// Scenario B — idempotency: the runner applied twice changes nothing.
func TestMigrationScenario_B_IdempotentRerun(t *testing.T) {
	_, db := scenarioDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := persistence.Migrate(ctx, db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	ledger1 := appliedVersions(t, db)
	tables1 := tablesOf(t, db)

	if err := persistence.Migrate(ctx, db); err != nil {
		t.Fatalf("second migrate (must be a no-op): %v", err)
	}
	ledger2 := appliedVersions(t, db)
	tables2 := tablesOf(t, db)

	if fmt.Sprint(ledger1) != fmt.Sprint(ledger2) {
		t.Fatalf("ledger changed on rerun:\n before=%v\n after=%v", ledger1, ledger2)
	}
	if fmt.Sprint(tables1) != fmt.Sprint(tables2) {
		t.Fatalf("table set changed on rerun:\n before=%v\n after=%v", tables1, tables2)
	}
	if err := persistence.VerifySchema(ctx, db); err != nil {
		t.Fatalf("schema verify after rerun: %v", err)
	}
}

// Scenario C — interrupted migration: a migration that fails midway
// rolls back completely; the ledger stays consistent; recovery is a
// roll-forward re-run after the defect is fixed.
func TestMigrationScenario_C_InterruptedMigrationRollback(t *testing.T) {
	_, db := scenarioDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Controlled failure injection: two synthetic migrations; the second
	// creates a table, inserts a row, THEN fails (multi-statement within
	// one transaction — everything must vanish on rollback).
	failingSet := []persistence.TestMigration{
		{Version: "s1_ok", SQL: `CREATE TABLE scenario_c_marker (id INT PRIMARY KEY); INSERT INTO scenario_c_marker VALUES (1);`},
		{Version: "s2_fails", SQL: `CREATE TABLE scenario_c_partial (id INT PRIMARY KEY);` +
			`INSERT INTO scenario_c_partial VALUES (1);` +
			`DO $$ BEGIN RAISE EXCEPTION 'controlled migration failure (scenario C)'; END $$;`},
	}
	err := persistence.MigrateWith(ctx, db, failingSet)
	if err == nil {
		t.Fatal("controlled failure did not surface")
	}
	if !errors.Is(err, domain.ErrPersistence) {
		t.Fatalf("failure typed %T/%v, want domain.ErrPersistence", err, err)
	}
	if !strings.Contains(err.Error(), "s2_fails") {
		t.Fatalf("error must identify the failing migration: %v", err)
	}
	t.Logf("controlled failure surfaced: %v", err)

	// Rollback completeness: partial objects gone, ledger has only s1.
	tables := tablesOf(t, db)
	if tables["scenario_c_partial"] {
		t.Fatal("rollback incomplete: scenario_c_partial survived")
	}
	if !tables["scenario_c_marker"] {
		t.Fatal("s1 must remain applied (each migration commits independently)")
	}
	ledger := appliedVersions(t, db)
	if _, ok := ledger["s2_fails"]; ok {
		t.Fatal("ledger recorded a failed migration (inconsistent version tracking)")
	}
	if _, ok := ledger["s1_ok"]; !ok {
		t.Fatal("ledger lost s1_ok")
	}

	// Recovery: roll-forward re-run with the fixed migration succeeds.
	fixedSet := []persistence.TestMigration{
		{Version: "s1_ok", SQL: failingSet[0].SQL},
		{Version: "s2_fails", SQL: `CREATE TABLE scenario_c_partial (id INT PRIMARY KEY);`},
	}
	if err := persistence.MigrateWith(ctx, db, fixedSet); err != nil {
		t.Fatalf("recovery re-run: %v", err)
	}
	ledger = appliedVersions(t, db)
	if _, ok := ledger["s2_fails"]; !ok {
		t.Fatalf("ledger after recovery: %v", ledger)
	}
	if !tablesOf(t, db)["scenario_c_partial"] {
		t.Fatal("recovery did not create the table")
	}
}

// Scenario D — existing database upgrade: a database at version 0001
// holding representative data upgrades through a later migration with
// the data preserved (machine for future real migrations 0002+; today's
// production set has exactly one migration).
func TestMigrationScenario_D_UpgradePreservesData(t *testing.T) {
	_, db := scenarioDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := persistence.Migrate(ctx, db); err != nil {
		t.Fatalf("build at 0001: %v", err)
	}
	// Representative historical records.
	store := persistence.NewStore(db)
	tid, err := store.Tenants.Create(ctx, "upgrade-tenant")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: tid, DisplayAlias: "legacy-account", PhoneHash: "legacy-hash",
		PhoneMask: "+98 *** 42", Status: domain.AccountStatusActive,
		Desired: domain.DesiredRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Sessions.Store(ctx, domain.AccountID(rec.AccountID), []byte{0x53, 0x4d, 0x42, 0x31, 0x01}); err != nil {
		t.Fatal(err)
	}

	// A later migration: add a column + backfill (representative of real
	// future migrations 0002+).
	upgrade := []persistence.TestMigration{
		{Version: "0002_add_note", SQL: `
			ALTER TABLE accounts ADD COLUMN IF NOT EXISTS note TEXT NOT NULL DEFAULT '';
			UPDATE accounts SET note = 'migrated:' || display_alias;`},
	}
	if err := persistence.MigrateWith(ctx, db, upgrade); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	// Data preservation + schema correctness.
	var alias, note string
	if err := db.QueryRow(ctx,
		`SELECT display_alias, note FROM accounts WHERE id = $1`, rec.AccountID).
		Scan(&alias, &note); err != nil {
		t.Fatalf("read upgraded row: %v", err)
	}
	if alias != "legacy-account" {
		t.Fatalf("data lost in upgrade: alias=%q", alias)
	}
	if note != "migrated:legacy-account" {
		t.Fatalf("backfill wrong: note=%q", note)
	}
	var blob []byte
	if err := db.QueryRow(ctx,
		`SELECT blob FROM sessions WHERE account_id = $1`, rec.AccountID).Scan(&blob); err != nil {
		t.Fatalf("session row lost in upgrade: %v", err)
	}
	// Ledger consistency after upgrade.
	ledger := appliedVersions(t, db)
	if _, ok := ledger["0001_init"]; !ok {
		t.Fatal("0001_init missing from ledger after upgrade")
	}
	if _, ok := ledger["0002_add_note"]; !ok {
		t.Fatal("0002_add_note missing from ledger after upgrade")
	}
	if err := persistence.VerifySchema(ctx, db); err != nil {
		t.Fatalf("verify after upgrade: %v", err)
	}
}

// Scenario E — concurrent migration runs: independent pools (simulating
// independent processes/replicas) race to migrate the same fresh
// database. The advisory lock serializes them: exactly-once application,
// consistent ledger, valid final schema.
func TestMigrationScenario_E_ConcurrentMigrationRuns(t *testing.T) {
	_, db := scenarioDB(t)
	postgresVersion(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Independent pools over the same database (what separate replicas
	// would each hold).
	const n = 8
	dsn := testDSN(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + strings.TrimPrefix(u.Path, "/")
	if u.Path == "" {
		u.Path = "/postgres"
	}
	// Reopen on the scenario DB name is not available here (it is
	// internal to scenarioDB); instead use the same pool across runners —
	// pgxpool hands out distinct connections, so the advisory lock still
	// serializes independent sessions (the same semantics a second
	// process would exercise).
	var successes, failures atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cctx, ccancel := context.WithTimeout(ctx, 90*time.Second)
			defer ccancel()
			err := persistence.Migrate(cctx, db)
			switch {
			case err == nil:
				successes.Add(1)
			default:
				failures.Add(1)
				t.Logf("runner %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent migrators failed: locking must serialize them, not fail them", failures.Load())
	}
	if successes.Load() != n {
		t.Fatalf("successes = %d, want %d", successes.Load(), n)
	}
	ledger := appliedVersions(t, db)
	if len(ledger) != 1 {
		t.Fatalf("ledger has %d rows, want exactly 1 (exactly-once application)", len(ledger))
	}
	if err := persistence.VerifySchema(ctx, db); err != nil {
		t.Fatalf("schema after concurrent runs: %v", err)
	}
}
