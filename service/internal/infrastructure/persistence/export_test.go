package persistence

// Exported-for-test hooks: migration scenario tests (external test
// package) need to drive the migration runner with synthetic migration
// sets (failure injection, upgrade paths). Not compiled into production
// binaries.

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigration is the exported test view of one migration. It converts
// to the internal representation in MigrateWith.
type TestMigration struct {
	Version string
	SQL     string
}

// MigrateWith runs the migration runner over a caller-supplied migration
// set (same locking/transaction/version-tracking semantics as Migrate).
func MigrateWith(ctx context.Context, pool *pgxpool.Pool, ms []TestMigration) error {
	internal := make([]migration, 0, len(ms))
	for _, m := range ms {
		internal = append(internal, migration{version: m.Version, sql: m.SQL})
	}
	return migrate(ctx, pool, internal)
}
