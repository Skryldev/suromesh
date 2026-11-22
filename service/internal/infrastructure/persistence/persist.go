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

// Migrate applies embedded migrations in order. Each migration runs in
// its own transaction and is recorded in schema_migrations; already-
// applied files are skipped (idempotent).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("%w: migrations table: %v", domain.ErrPersistence, err)
	}
	for _, m := range migrations {
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
