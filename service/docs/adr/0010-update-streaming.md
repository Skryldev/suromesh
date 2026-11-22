# ADR-0010: Update streaming and delivery guarantees

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

The SDK delivers accepted updates exactly-once, in MessageBox
order, with own-updates silent, into in-process subscribers. gRPC
clients consume over server-streaming RPCs.

## Problem

What delivery guarantee does the service make across streams,
restarts, and consumer failures?

## Considered alternatives

1. **Durable event log + consumer offsets (Kafka-like)** - rejected
   for v1: a stateful broker is exactly the unjustified distributed
   system the constraints warn about; no requirement justifies it yet.
2. **At-most-once per stream + catch-up semantics** (chosen).

## Decision

`StreamUpdates` is at-most-once **per stream**: bounded per-stream
buffer; overflow disconnects that stream (documented); consumer
reconnects and re-derives missed data through reads (history/dialogs)
- the SDK's persisted update state keeps the *account's* frontier
correct across service restarts, so the next stream starts from the
reconciled present. Per-stream sequence numbers make gaps detectable.
Durable replay (event sourcing) is a future ADR if a requirement
materializes.

## Rationale

Honest guarantee vocabulary (at-most-once vs exactly-once) instead
of marketing semantics; zero new stateful infrastructure; matches how
the SDK itself persists state (frontier, not payloads).

## Trade-offs

Consumers wanting strong delivery must reconcile via reads; this
is documented prominently in the streaming guides.

## Security implications

Slow-consumer disconnects protect the process (bounded memory);
no unbounded queues anywhere.

## Operational implications

Monitor disconnect-on-overflow rate as the key streaming SLO
signal; stream lifetimes bounded by idle-timeout + keepalives.
