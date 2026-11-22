# ADR-0005: Caller authentication model

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Callers are external applications. The service holds valuable
credentials (account sessions) - authentication design is
security-critical.

## Problem

API keys, mTLS, or OIDC/JWT for caller authentication?

## Considered alternatives

1. **API keys** (chosen for v1): 32-byte random, `sk_` prefix,
   stored as SHA-256 hash + prefix; `authorization: Bearer` metadata.
2. **mTLS** (optional hardening): strongest for fixed service-to-service
   fleets; certificate lifecycle cost.
3. **OIDC/JWT** (upgrade path): right answer once an organizational IdP
   exists; adds infrastructure dependency for v1.

## Decision

v1 ships API keys over TLS with an interceptor-first design:
the authn mechanism is an interceptor contract, so mTLS/OIDC layer in
without contract changes. Keys rotate (overlap window) and revoke
immediately; the raw key is displayed exactly once at creation.

## Rationale

Simple, testable, language-neutral; constant-time hash comparison;
the interceptor contract keeps the upgrade path honest.

## Trade-offs

API keys are long-lived bearer credentials - mitigate with TLS
requirement, rotation policy, scope-limited permission sets, and
audit.

## Security implications

Key hashes leak nothing usable (preimage-resistant, high entropy);
prefix-only storage supports identification without exposure.

## Operational implications

Key rotation is an operational routine (documented); revocation
must be fast (in-memory denylist refreshed from storage).
