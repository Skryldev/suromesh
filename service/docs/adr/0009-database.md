# ADR-0009: Database selection

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Durable needs: tenants, credentials, account bindings, encrypted
session blobs, operation/idempotency records, audit log, (future)
leases.

## Problem

PostgreSQL, SQLite, or a document/KV store?

## Considered alternatives

1. **PostgreSQL** (chosen): relational fit, transactions for
   idempotency dedupe, mature Go stack, future lease/fencing support,
   backups.
2. **SQLite** - right for single-node hobby scale; wrong for the
   intended availability and future sharding story.
3. **Redis/Mongo** - rejected: no requirement justifies a second
   stateful system in v1 (per the constraint on unjustified
   distributed systems).

## Decision

PostgreSQL 15+ as the single durable store. Migrations are
versioned SQL applied explicitly (no silent auto-migrate). No Redis,
no Kafka, no message broker in v1 - the update fan-out is in-process
and the durability story is the SDK's persisted update state (ADR-010).

## Rationale

One stateful dependency; well-understood operations; the idempotency
and lease needs are exactly SQL's strengths.

## Trade-offs

Postgres becomes a hard runtime dependency (readiness-critical);
documented in the failure matrix.

## Security implications

Credentials in DB are hashes/encrypted blobs only (ADR-005/0008);
connection string via secret store.

## Operational implications

Backup/restore runbook pairs DB backups with KEK availability;
monitoring: connection pool saturation, replication lag (future).
