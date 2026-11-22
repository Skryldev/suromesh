# ADR-0008: Session persistence and encryption

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Session blobs contain the account's authorization key - the
credential for the account.

## Problem

How is session material stored?

## Considered alternatives

1. **Plaintext in DB** - forbidden.
2. **Application-level encryption with in-repo key** - rejected: key
   beside data defeats the purpose.
3. **Envelope encryption: per-session DEK (AES-256-GCM), DEK wrapped
   by an external KEK** (chosen). KEK sources (in order): KMS
   (recommended), environment-provided key file. Rotation = re-wrap
   DEKs; deletion = destroy wrapped DEK (crypto-shred).

## Decision

SessionRepository stores: `ciphertext, wrapped_dek, format_version,
updated_at`. Format versioning mirrors the SDK's session format
discipline. The service never returns session material over gRPC
(status/metadata only). String-session import is privileged
(`sessions:write`) and the imported material is immediately
re-encrypted into the envelope format.

## Rationale

Standard envelope pattern; crypto-shredding gives safe account
deletion; KEK separation means a DB compromise alone is not an account
compromise.

## Trade-offs

KEK availability becomes critical-path (documented failures F6/F11);
KMS latency on session load (cached wrapped DEKs mitigate).

## Security implications

KEK compromise = full compromise (same as any single-key design) -
mitigate via KMS controls; DEK uniqueness bounds blast radius of
per-blob issues.

## Operational implications

Backup policy must pair DB backups with KEK availability; key
rotation is a routine operation with a documented runbook.
