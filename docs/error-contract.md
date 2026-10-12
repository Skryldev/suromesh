# Error Contract (Phase 17)

The complete, tested contract for error behavior across the public API.
A caller must be able to distinguish every class below **without parsing
error strings** — via `errors.Is`, `errors.As`, and the classification
helpers.

## Classes

| Class | Detect with | Meaning | Retryable? |
|---|---|---|---|
| Caller cancellation | `errors.Is(err, context.Canceled)` | The caller's context was cancelled | No (caller chose) |
| Timeout | `errors.Is(err, context.DeadlineExceeded)` | The caller's deadline expired | Caller decides |
| Client closed | `errors.Is(err, client.ErrClosed)` | Terminal `Close` happened before/during the call | No (terminal) |
| Not ready | `errors.Is(err, client.ErrNotReady)` | Operation needs authorization/connectivity that isn't there yet | After the state changes |
| Connection lost | `client.AsConnectionLost(err)` / `errors.As(&client.ConnectionLostError{})` | Transport dropped mid-operation | Yes — but mutations are NOT auto-retried (side effects may have happened); see docs/retry-policy.md |
| RPC error | `errors.As(err, &rpc.RPCError{})` | Server answered with an RPC error | Depends on type (below) |
| Flood wait | `client.AsFloodWait(err) (*FloodWaitError, bool)` | FLOOD_WAIT_N above the auto-retry threshold; `.Seconds` is the wait | After waiting |
| Invalid input | `errors.Is(err, client.ErrPhoneInvalid / ErrInvalidPeer / ErrInvalidMedia / ErrInvalidConfig)` | Local validation failed; nothing was sent | No — fix the input |
| Unavailable entity | `errors.As(err, &client.ErrAccessHashUnavailable{})` | Peer identity known but access hash not server-provided | After the entity is learned from the server (never fabricated) |
| Reply unavailable | `errors.Is(err, client.ErrReplyUnavailable)` | Reply target not resolvable from context | No |
| Media errors | `errors.Is(err, client.ErrUnsupportedMedia / ErrInvalidMedia / ErrMediaLocation / ErrFileReferenceExpired)`; `errors.As(&client.ErrPartUploadFailed{})`, `errors.As(&client.ErrDownloadShortChunk{})` | Upload/download failures; file-reference expiry means re-fetch the message | Ref/refetch-dependent |
| SRP/auth params | `errors.Is(err, client.ErrSRPParams / ErrSRPAlgo)` | Server sent bad SRP material or unsupported algorithm | No (security) |
| Update gap | `errors.Is(err, client.ErrUpdateGap)` | Update stream gap could not be resolved | Transient; CatchUp resumes |
| Queue full | `errors.Is(err, client.ErrUpdateQueueFull / rpc.ErrPendingLimit)` | Bounded resource exhausted; backpressure surfaced | Yes, after draining |
| Transport failure | `errors.As(err, &transport.Error{})`, `transport.Classify(err)` | Dial/TLS/WS/framing failures; classes Temporary/Protocol/Fatal | Temporary yes |
| Protocol violation | `errors.As(err, &tl.ConstructorMismatchError{})`, `tl.IsUnknownConstructor(err)`, mtproto `Err*` | Malformed/unexpected wire data | No (fingerprint the case) |
| Session corrupt | `errors.Is(err, session.ErrCorrupt / ErrNotAStringSession / ErrClosed)` | Persistence-layer failures | Load-time only |

## Precedence (tested)

`Invoke` (and every consumer RPC built on it) guarantees:

1. **Caller context wins.** If the caller's context is done, the returned
   error is exactly `ctx.Err()` — cancellation and deadlines are never
   swallowed by retry loops or Close handling.
2. **Close surfaces as `ErrClosed`.** If the client was closed during a
   retry backoff or in-flight wait, the caller gets `ErrClosed` (not a bare
   context error), even though the lifecycle context also fired. This is a
   deliberate, tested precedence: "the client is gone" is the actionable
   truth.
3. **Retry exhaustion surfaces the original RPC error.** After
   `Config.Retries` server-internal failures, the last `*rpc.RPCError` is
   returned unwrapped in identity.
4. **Connection loss is never auto-retried for mutations** (the request may
   have executed server-side). It is returned as-is; `AsConnectionLost`
   identifies it.

## Sentinel inventory (package `client` unless noted)

`ErrClosed`, `ErrNotReady`, `ErrPhoneInvalid`, `ErrInvalidConfig`,
`ErrInvalidPeer`, `ErrPeerNotFound`, `ErrReplyUnavailable`, `ErrInvalidMedia`,
`ErrUnsupportedMedia`, `ErrMediaLocation`, `ErrFileReferenceExpired`,
`ErrTransferClosed`, `ErrUpdateGap`, `ErrUpdateQueueFull`, `ErrSRPParams`,
`ErrSRPAlgo`; `rpc.ErrEngineClosed`, `rpc.ErrPendingLimit`,
`rpc.ErrConnectionLost`; `transport.ErrClosed`; `session.ErrCorrupt`,
`session.ErrNotAStringSession`, `session.ErrClosed`; `tl.ErrUnexpectedEOF`;
`auth.ErrSecurity`.

Typed errors (`errors.As`): `*rpc.RPCError`, `*client.FloodWaitError`,
`*client.ConnectionLostError`, `*client.ErrAccessHashUnavailable`,
`*client.ErrPartUploadFailed`, `*client.ErrDownloadShortChunk`,
`*transport.Error`, `*tl.ConstructorMismatchError`, `*ws.CloseError`.

## Observability helper

`client.ClassifyError(err) string` maps to coarse, secret-free classes:
`none, cancelled, timeout, connection_lost, closed, not_ready, rpc_error,
transport_<class>, unknown`. It is for metrics/logging only — control flow
must use `errors.Is/As`.

## No-secret rule

No error produced by this SDK embeds: auth keys, SRP material, passwords,
phone codes, phone_code_hash, access hashes, file references, message
bodies, media bytes, search query text, phone numbers, or usernames.
Error text identifies *shape* (peer kind/ID, method name, sizes), never
secret content. Enforced by review per phase and by dedicated
auth-security tests (auth_security_test.go).

## Per-family contracts

- **Auth family** (SendCode/SignIn/CheckPassword/LogOut): sentinel-text
  rendering for server auth errors (no raw server strings), terminal
  revocation reflected in `AuthState()`, `ErrClosed` after Close.
- **Mutations** (SendMessage/Edit/Delete/Forward/Pin/Read): exactly-once
  subscriber semantics hold regardless of error; a failed mutation never
  emits an event; OwnUpdate results advance MessageBox state without
  re-dispatch.
- **Iterators**: errors are sticky — `Err()` returns the first failure,
  `Next()` returns false thereafter; context cancellation during pagination
  ends the iterator with `context.Canceled`/`DeadlineExceeded` (never a
  transport-wrapped variant).
- **Media transfers**: terminal states are CAS'd even on panic; a cancelled
  transfer stops at the current part boundary and reports the context
  error; `ErrTransferClosed` after manager close.
- **Subscribers**: handler panics are recovered (events continue); queue
  overflow follows the configured policy and is visible via
  `DroppedUpdates()`/metrics, never by silent corruption.
