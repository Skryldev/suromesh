# ADR-0006: Authorization and tenancy enforcement

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Multi-tenant service holding per-tenant Soroush Plus accounts.

## Problem

How is tenant isolation enforced - per-RPC checks, policy engine,
or interceptor-only?

## Considered alternatives

1. **Interceptor-only** - rejected alone: one missed check leaks.
2. **External policy engine (OPA)** - rejected for v1: operational
   cost without proportional benefit at this scale.
3. **Central interceptor + per-use-case ownership re-checks** (chosen).

## Decision

Interceptors authenticate and resolve tenant + permissions
(deny-by-default). Every use case that touches an account resolves
the account **through the caller's tenant** - cross-tenant references
are `NOT_FOUND` (no existence oracle). Permission constants are
compile-time; the permission matrix per RPC is documented in the API
reference and pinned by tests.

## Rationale

Defense in depth: two independent enforcement layers; the
tenant-scoping pattern makes whole-account enumeration impossible by
construction.

## Trade-offs

Slightly more code per use case; zero tolerance for bypassing
the repository scoping (review-enforced).

## Security implications

Existence-oracle avoidance is itself a security property;
authorization denials are audited.

## Operational implications

Isolation tests are mandatory per phase (test strategy T-ISOL).
