# Error Model (Phase 15)

The error taxonomy and classification across all layers. Programmatic
behavior must use errors.Is/errors.As — no error-string matching.

## Layers

| Layer | Type | Examples |
|---|---|---|
| transport | `*transport.Error` (Class) | dial failure, protocol violation |
| rpc | `*rpc.RPCError` (Code/Type/Message/Argument) | wire 400/420/500 errors |
| rpc | `rpc.ErrConnectionLost`, `rpc.ErrEngineClosed`, `rpc.ErrPendingLimit` | sentinels |
| client invoke | `ErrClosed`, `ErrNotReady`, `FloodWaitError`, `ConnectionLostError` | client wrappers |
| auth | auth sentinels (Phase 9: ErrPasswordRequired, ErrCodeInvalid, …) | mapped via authRPCError |
| media | ErrUnsupportedMedia, ErrMediaLocation, ErrFileReferenceExpired, ErrPartUploadFailed, ErrTransferIntegrity, ErrInvalidPartSize, ErrSizeUnknown, ErrNonReplayableReader | Phase 8/13 |
| updates | ErrUpdateGap, ErrPaginationStalled | Phase 10/11 |
| peers | ErrAccessHashUnavailable, ErrPeerNotFound, ErrInvalidPeer, ErrPhoneUnoccupied | Phase 11 |
| session | ErrCorrupt, ErrClosed, ErrInvalidState | Phase 5/9 |

## Classification

- **Sentinel**: stable identity via errors.Is (ErrClosed, ErrCodeExpired, …).
- **Typed**: structured fields via errors.As (*rpc.RPCError carries
  Code/Type for programmatic classification; FloodWaitError carries
  Seconds; ErrAccessHashUnavailable carries the Peer).
- **Wrapped**: chains preserve identity — every wrapper implements
  Unwrap; authRPCError renders only sentinel text while unwrapping to
  the raw RPC error.
- **Terminal**: ErrClosed / ErrEngineClosed / ErrTransferClosed /
  ErrAuthorizationRevoked — no retry, no recovery.
- **Retryable**: 500-class, tolerated FLOOD_WAIT, pre-send connection
  loss (see docs/retry-policy.md).
- **Cancellation**: context.Canceled / DeadlineExceeded always win and
  are never swallowed by wrappers.

## Secret hygiene

No error produced by this library contains: auth keys or key bytes, SRP
material, passwords, phone codes, phone_code_hash, access hashes, file
references, message bodies, media bytes, search query text. Structural
identifiers (peer kind + id, message id, RPC type string) are permitted
where they make errors actionable. Verified by the per-phase
secret-leak tests and the Phase 15 behavioral audit.
