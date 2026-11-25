// Package persistence implements the durable ports against PostgreSQL
// (pgx/v5): tenants, credentials, accounts, encrypted sessions,
// idempotent operations, and audit events.
//
// Rules enforced here:
//   - every query is context-aware (cancellation/deadlines propagate);
//   - no transaction is held open across upstream network calls;
//   - tenant scoping is part of every query (no cross-tenant reads);
//   - migrations are versioned SQL applied in order, recorded in
//     schema_migrations, and idempotent.
package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// Connect builds the connection pool. The DSN comes from configuration
// (secret store); callers own pool sizing.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: parse dsn: %v", domain.ErrPersistence, err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %v", domain.ErrPersistence, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%w: ping: %v", domain.ErrPersistence, err)
	}
	return pool, nil
}

// migrationLockID is the fixed key for the PostgreSQL advisory lock that
// serializes concurrent migration runs (Scenario E). A dedicated value in
// the global advisory-lock namespace ("SURO" as int32 pair); unrelated
// software must not use this key.
const migrationLockID int64 = 0x5355524F_4D494752 // "SUROMIGR"

// Migrate applies embedded migrations in order. Each migration runs in
// its own transaction and is recorded in schema_migrations; already-
// applied files are skipped (idempotent).
//
// Concurrency: the whole run is serialized across ALL migrators (this
// process, other processes, other replicas) by a session-level advisory
// lock taken on a dedicated connection and held until the run finishes
// (released on context cancellation or connection close — a crashed
// migrator can never leave the lock stuck). Only roll-forward is
// supported; there is no down-migration.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate(ctx, pool, migrations)
}

func migrate(ctx context.Context, pool *pgxpool.Pool, ms []migration) error {
	// Dedicated connection owns the advisory lock for the whole run.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("%w: migration lock connection: %v", domain.ErrPersistence, err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("%w: migration lock (another migrator may be running): %v",
			domain.ErrPersistence, err)
	}
	// Release even when ctx was canceled mid-run (unlock must not be
	// skipped because the surrounding context died).
	defer func() {
		_, _ = lockConn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("%w: migrations table: %v", domain.ErrPersistence, err)
	}
	for _, m := range ms {
		var applied bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`,
			m.version).Scan(&applied); err != nil {
			return fmt.Errorf("%w: check %s: %v", domain.ErrPersistence, m.version, err)
		}
		if applied {
			continue
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("%w: begin %s: %v", domain.ErrPersistence, m.version, err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%w: apply %s: %v", domain.ErrPersistence, m.version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, m.version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%w: record %s: %v", domain.ErrPersistence, m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("%w: commit %s: %v", domain.ErrPersistence, m.version, err)
		}
	}
	return nil
}

// KnownMigrationVersions returns the ordered list of migration versions
// known to this binary (for the migrate CLI's status command and for
// drift reporting: a version recorded in schema_migrations but absent
// here means the database is newer than the binary).
func KnownMigrationVersions() []string {
	out := make([]string, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, m.version)
	}
	return out
}

// requiredTables names every table the service cannot run without.
// VerifySchema gates application startup against a database whose schema
// does not match the migration set (e.g. manual tampering, a partial
// restore): fail closed, never continue silently.
var requiredTables = []string{
	"schema_migrations", "tenants", "credentials", "accounts",
	"sessions", "operations", "audit_events",
}

// VerifySchema checks that every required table (and a minimal set of
// critical columns) exists in the connected database. It runs AFTER
// Migrate in application startup and in the migrate CLI; a mismatch is a
// hard error — the service must not serve against an incompatible schema.
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range requiredTables {
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&ok); err != nil {
			return fmt.Errorf("%w: verify %s: %v", domain.ErrPersistence, table, err)
		}
		if !ok {
			return fmt.Errorf("%w: required table %s missing after migration "+
				"(incompatible or tampered schema — do not continue)", domain.ErrPersistence, table)
		}
	}
	// Spot-check the columns most load-bearing for correctness.
	criticalColumns := []struct{ table, column string }{
		{"credentials", "key_hash"}, {"credentials", "tenant_id"},
		{"accounts", "tenant_id"}, {"accounts", "phone_hash"},
		{"sessions", "blob"}, {"sessions", "kek_version"},
		{"operations", "fingerprint"}, {"operations", "op_key"},
	}
	for _, cc := range criticalColumns {
		var ok bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM information_schema.columns
				WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`,
			cc.table, cc.column).Scan(&ok); err != nil {
			return fmt.Errorf("%w: verify %s.%s: %v", domain.ErrPersistence, cc.table, cc.column, err)
		}
		if !ok {
			return fmt.Errorf("%w: required column %s.%s missing after migration",
				domain.ErrPersistence, cc.table, cc.column)
		}
	}
	return nil
}

type migration struct {
	version string
	sql     string
}

var migrations = []migration{
	{version: "0001_init", sql: migration0001},
}

// mapPgxErr converts driver errors into the domain taxonomy. Unique
// violations surface as ErrAlreadyExists; connection failures as
// ErrPersistence.
func mapPgxErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, what)
	}
	msg := err.Error()
	if strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint") {
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, what)
	}
	return fmt.Errorf("%w: %s: %v", domain.ErrPersistence, what, err)
}
