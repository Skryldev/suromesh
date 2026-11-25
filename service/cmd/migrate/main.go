// Command migrate applies the SuroMesh Service's embedded PostgreSQL
// migrations (roll-forward only — there is no down-migration; see
// docs/operations/deployment.md).
//
// Subcommands:
//
//	up      apply all pending migrations (serialized by a PostgreSQL
//	        advisory lock — safe to run concurrently and from multiple
//	        replicas; second entrant waits, then finds nothing to do)
//	status  list every known migration and whether it is applied
//
// Configuration: SUROMESH_STORAGE_POSTGRES_DSN (required). An optional
// -timeout bounds the run (default 2m). The DSN must point at a
// database whose schema may be modified by migrations — never point it
// at a production database unless you intend to migrate it.
//
// Exit codes: 0 success, 1 migration/connection failure, 2 usage error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Skryldev/suromesh/service/internal/infrastructure/persistence"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 2*time.Minute, "overall time budget")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: migrate up|status [-timeout 2m]")
		return 2
	}
	dsn := os.Getenv("SUROMESH_STORAGE_POSTGRES_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "migrate: SUROMESH_STORAGE_POSTGRES_DSN is required")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pool, err := persistence.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate: connect:", err)
		return 1
	}
	defer pool.Close()

	switch fs.Arg(0) {
	case "up":
		return up(ctx, pool)
	case "status":
		return status(ctx, pool)
	default:
		fmt.Fprintf(os.Stderr, "migrate: unknown command %q (want up|status)\n", fs.Arg(0))
		return 2
	}
}

func up(ctx context.Context, pool *pgxpool.Pool) int {
	if err := persistence.Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: up:", err)
		return 1
	}
	if err := persistence.VerifySchema(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: schema verification failed:", err)
		return 1
	}
	fmt.Println("migrate: up complete, schema verified")
	return 0
}

func status(ctx context.Context, pool *pgxpool.Pool) int {
	type row struct {
		version   string
		appliedAt *time.Time
	}
	known := persistence.KnownMigrationVersions()
	applied := map[string]time.Time{}
	rows := map[string]bool{}
	// applied rows from the database (the table may not exist yet).
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: status:", err)
		return 1
	}
	recs, err := pool.Query(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate: status:", err)
		return 1
	}
	defer recs.Close()
	for recs.Next() {
		var r row
		if err := recs.Scan(&r.version, &r.appliedAt); err != nil {
			fmt.Fprintln(os.Stderr, "migrate: status:", err)
			return 1
		}
		rows[r.version] = true
		if r.appliedAt != nil {
			applied[r.version] = *r.appliedAt
		}
	}
	if err := recs.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: status:", err)
		return 1
	}
	fmt.Printf("%-16s %-10s %s\n", "VERSION", "STATE", "APPLIED AT")
	pending := 0
	for _, version := range known {
		if rows[version] {
			fmt.Printf("%-16s %-10s %s\n", version, "applied", applied[version].UTC().Format(time.RFC3339))
			continue
		}
		pending++
		fmt.Printf("%-16s %-10s %s\n", version, "PENDING", "-")
	}
	// Versions recorded in the DB but unknown to this binary: someone
	// downgraded the binary after migrating — an operator-visible hazard.
	for v := range rows {
		knownV := false
		for _, kv := range known {
			if kv == v {
				knownV = true
				break
			}
		}
		if !knownV {
			fmt.Printf("%-16s %-10s %s\n", v, "UNKNOWN-TO-BINARY", applied[v].UTC().Format(time.RFC3339))
		}
	}
	fmt.Printf("summary: %d known, %d pending\n", len(known), pending)
	return 0
}
