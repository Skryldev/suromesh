# Security Architecture (Phase 24 design)

Design-first: nothing here is implemented yet. Principles: no custom
crypto, least privilege, secrets never beside data, sanitized
observability, honest audit.

## 1. Caller authentication (ADR-005)

| Mechanism | Verdict | Rationale |
|---|---|---|
| **API keys (v1 default)** | chosen | Simple, revocable, language-neutral; 32 random bytes, `sk_` prefix; server stores **SHA-256 hash + prefix** only; constant-time comparison |
| mTLS | optional hardening | Ideal for service-to-service; cert lifecycle cost; supported later without contract change (interceptor swap) |
| OIDC/JWT | upgrade path | Recommended once an org IdP exists; token → tenant mapping claims; no contract change |
| Custom crypto protocols | forbidden | — |

Key lifecycle: create (returns raw key **once**) → use → rotate (old
key valid until expires_at) → revoke (immediate). All events audited.

## 2. Authorization (ADR-006)

- Central authn/authz interceptors resolve: key → tenant → permissions.
- Permission set (v1): `accounts:read`, `accounts:write`, `auth:write`,
  `messaging:send`, `messaging:read`, `dialogs:read`,
  `media:transfer`, `streams:read`, `sessions:write` (import/revoke).
- **Defense in depth**: every use case re-checks account ownership by
  resolving `account_id` through the caller's tenant — cross-tenant
  references return `NOT_FOUND` (no existence leak).
- Deny-by-default; unknown permission requested → deny + audit.

## 3. Secrets handling

| Secret | Rule |
|---|---|
| OTP | lives only inside the `SubmitCode` RPC (in-memory); never logged, persisted, or echoed in errors; never cached |
| 2FA password | same as OTP (single RPC lifetime) |
| Session material (auth key) | encrypted at rest: **AES-256-GCM envelope** — per-session DEK, DEK wrapped by external KEK (KMS/env-provided); KEK never stored beside data; key rotation = re-wrap DEKs (ADR-008) |
| API keys | stored as SHA-256 hashes + 8-char prefix for identification |
| Phone numbers | stored as HMAC-SHA256 (lookup) + masked display (`+98 ••• •• 21 77` style); raw phone only crosses the SubmitCode path in-memory |
| Logs/evidence | structural redaction middleware: configured secret registry scrubbed before emit (inherits the SDK's redaction discipline) |

## 4. Network security

- **TLS required** in production (service terminates TLS or sits behind
  a TLS-terminating proxy in a private network; both documented).
- gRPC **reflection disabled** by default (enabled only in dev).
- Health service on the same port but unauthenticated ONLY for
  `grpc.health.v1` (no data); full separation via a second listener is
  the documented alternative.
- Production guidance: private network / mTLS between proxy and
  service; no public exposure without the authn layer.

## 5. Abuse prevention & quotas

| Control | Default (tunable) |
|---|---|
| Per-credential request rate | token bucket, e.g. 600 req/min, burst 120 |
| Per-tenant concurrent accounts | 10 (v1) |
| Per-tenant concurrent operations | 32 |
| Per-tenant active streams | 16 |
| Request message size | 1 MiB (media frames: 256 KiB chunk cap) |
| Media size per upload | 2 GiB (protocol big-file path); daily bytes quota per tenant |
| Auth RPC rate | strict separate bucket (SMS cost protection) |
| Global admission | in-flight cap; overload → `RESOURCE_EXHAUSTED` |

## 6. Audit events (no payloads)

`credential.created/rotated/revoked`, `account.bound/suspended/resumed/
removed`, `auth.code_requested/succeeded/failed`, `auth.srp.succeeded/
failed`, `session.imported/revoked/shredded`, `tenant.quota.exceeded`,
`authz.denied`. Each: actor (credential id), tenant, target (opaque
ids), request-id, outcome. **Never**: OTP, password, key material,
session bytes, message text.

## 7. Threat notes (carried from the supply-chain model)

- Compromised DB → session blobs useless without KEK.
- Compromised caller credential → tenant-scoped blast radius;
  immediate revocation path; audit trail.
- Compromised process memory → unavoidable (holds plaintext sessions
  for connected accounts); mitigate with deployment hardening, not
  in-process tricks.
- Log injection/sensitive leakage → redaction middleware + no-payload
  audit design.
