# ADR-0007: Account ownership and concurrency

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Each account runs one SDK `client.Client` (safe for concurrent
use, bounded goroutines, MessageBox as sole ordering authority, and a
documented mutation-ambiguity policy).

## Problem

How many clients per account, and how are operations serialized?

## Considered alternatives

1. **Client per RPC** - rejected: handshake cost per operation,
   no update continuity, state thrash.
2. **Client pool per account** - rejected: pointless (the client is
   concurrent-safe) and complicates ordering.
3. **One client per account owned by an AccountRunner actor** (chosen):
   the runner owns connect lifecycle, session persistence, a bounded
   mutation queue (serializes mutations; reads run concurrently), and
   an update fan-out hub for streams.

## Decision

AccountRunner responsibilities: desired-vs-observed state
reconciliation, ctx-scoped operation dispatch, session load/save
integration, backpressure via bounded queue (reject with
RESOURCE_EXHAUSTED when full). Single-process ownership in v1
(ADR-002); distributed ownership via lease+fencing when scaling -
in-memory ownership is never claimed as distributed safety.

## Rationale

Matches SDK guarantees instead of fighting them; a single
serialization point gives deterministic ordering for mutations; the
fan-out hub maps cleanly onto server-streaming RPCs.

## Trade-offs

Mutation throughput is bounded per account (by design - upstream
rate limits dominate anyway); the runner adds one scheduling hop.

## Security implications

Ambiguity policy (never replay blind) is enforced in the runner,
not in each handler - one place to audit.

## Operational implications

Runner state must survive restarts: desired state persisted;
observed state rebuilt at boot (session restore).
