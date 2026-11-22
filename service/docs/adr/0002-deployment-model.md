# ADR-0002: Service deployment model

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

The service must manage authenticated accounts whose state lives in
memory (SDK clients) and encrypted blobs (storage). Horizontal scaling
introduces account-ownership questions.

## Problem

Single-process first, or horizontally scaled from day one?

## Considered alternatives

1. **Horizontally scaled with distributed coordination from day
   one** - rejected for v1: requires lease/fencing infrastructure and
   a complex sharding story before the product has proven value.
2. **Single-process, multi-account** (chosen for v1).
3. **Kubernetes-only** - rejected as mandatory; the service is
   K8s-shaped but container-first.

## Decision

v1 deploys as a **single instance owning all bound accounts**,
stateless beyond PostgreSQL. The scaling path (documented, not built):
N instances + PostgreSQL lease table (heartbeat, fencing token) with
account-to-instance assignment; instances serve only leased accounts.
In-memory ownership is never claimed to provide distributed safety -
the lease protocol is the mechanism when scaling happens.

## Rationale

Fastest path to a correct, testable v1; the failure matrix (F5) is
trivially satisfied single-process. Scaling later is an operations
change plus the lease implementation, not a redesign.

## Trade-offs

Single instance is a SPOF for account availability (mitigated by
restart + session restore); maximum account density bounded by one
node until sharding exists.

## Security implications

Smaller secret-management surface in v1; lease tokens add a
security-relevant primitive later (fencing on all account writes).

## Operational implications

Restart playbook required (session restore is designed in); at
scale, shard rebalancing must be drain-aware (graceful handoff).
