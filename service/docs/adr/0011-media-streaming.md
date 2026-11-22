# ADR-0011: Media streaming RPC patterns

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

SDK media is streaming by design (O(part) memory, big-file path,
progress, cancellation). gRPC messages have size limits (default
4 MiB).

## Problem

Unary chunked transfer or streaming RPCs for media?

## Considered alternatives

1. **Unary with byte arrays** - rejected: 2 GiB uploads exceed
   message limits and buffer in memory.
2. **Client-streaming upload + server-streaming download** (chosen).

## Decision

`UploadMedia` is client-streaming: first frame = metadata (size,
name, mime, target peer, idempotency key - the upstream protocol
requires size up-front), subsequent frames = 256 KiB chunks (capped).
`DownloadMedia` is server-streaming chunks. Limits: 2 GiB per upload,
tenant daily byte quotas, per-tenant concurrent transfers. Server
memory stays O(chunk) (SDK property). File-reference expiry surfaces
as typed error + re-fetch guidance (SDK invariant).

## Rationale

Matches the SDK's streaming model 1:1; no memory blowups; natural
progress reporting (frame counts).

## Trade-offs

Clients must implement streaming logic (standard gRPC); partial
uploads are discarded (no server-side resume in v1 - documented).

## Security implications

Chunk-size caps prevent memory abuse; quotas prevent transfer
abuse; upload metadata (name/mime) is content-shape data only.

## Operational implications

Transfer metrics (bytes, durations) exported per the observability
design; idle-stream timeouts bound stuck transfers.
