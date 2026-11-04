# Retry Policy (Phase 15)

The complete retry classification matrix. Single authority:
`client/invoke.go invokeWithRetries` (+ rpc engine connection-level
semantics). No subsystem may invent an independent policy without a
reference trace.

## Matrix

| Failure | Retry? | Backoff | Max attempts | Idempotent? | State impact | Caller-visible |
|---|---|---|---|---|---|---|
| RPC_CALL_FAIL (500) | YES | 2 s fixed | Config.Retries (default 5) | yes (reference: request provably not executed) | none | original error after exhaustion |
| 500-class server errors | YES | 2 s fixed | Config.Retries | yes | none | original error after exhaustion |
| FLOOD_WAIT ≤ threshold | YES | wait = seconds | Config.Retries | yes | none | error if wait > threshold or exhausted |
| FLOOD_WAIT > threshold | NO | — | — | — | none | FloodWaitError with seconds |
| connection loss pre-send | YES (engine re-send) | reconnect-driven | transport budget | yes | none | ErrConnectionLost if exhaust |
| connection loss mid-send | NO | — | — | NO (ambiguous execution) | pending failed ErrConnectionLost | ErrConnectionLost / ConnectionLostError |
| context cancellation | NO | — | — | — | none | context.Canceled (wins over everything) |
| deadline exceeded | NO | — | — | — | none | context.DeadlineExceeded |
| malformed response | NO | — | — | — | protocol-error metric | decode error |
| AUTH_KEY_UNREGISTERED / 401 | NO | — | — | — | auth state → unauthorized/revoked | mapped auth error |
| SESSION_REVOKED / USER_DEACTIVATED / AUTH_KEY_DUPLICATED | NO | — | — | — | AuthorizationRevoked | ErrAuthorizationRevoked |
| FILE_REFERENCE_EXPIRED | YES (exactly one, media refresh path) | none | 1 | yes (idempotent re-fetch) | file reference rotated | original error if refresh fails |
| media part RPC error | NO (reference: part failure is fatal) | — | — | — | transfer failed | ErrPartUploadFailed |
| update difference transient (500/flood) | YES (deadline-gated) | on next trigger (gap/deadline) | loop-free by construction | yes | diff ended, retried on next trigger | none (internal) |
| update difference 401-class | NO | — | — | — | auth state effects | auth error surfaced via hooks |
| channel diff CHANNEL_PRIVATE/INVALID | NO | — | — | — | channel state forgotten (banned) | none (internal) |
| persistence Save failure | NO (caller-driven) | — | — | — | in-memory state untouched | Save error |

## Anti-pattern guards (enforced)

- **No retry storms**: fixed 2 s server backoff; flood waits honored
  once per attempt; transport backoff is exponential with budget.
- **No retry multiplication**: mutations (send/edit/delete/forward/pin)
  ride the same invoke path — the classification above never
  connection-loss-retries a mid-flight mutation (ambiguous execution).
- **No infinite loops**: every retry path has a finite attempt budget or
  a deadline gate; update recovery re-triggers only on new gaps or the
  15-minute no-update deadline.
- **No retries after Close**: lifecycle ctx cancellation wins over every
  sleep/backoff; engine done fails all pending immediately.
- **No retries after cancellation**: ctx.Err() is checked before any
  retry decision.
- **No unsafe replay**: connection-loss mid-send is never retried
  (non-idempotent ambiguity); only pre-send connection loss is replayed
  (RetryOnReconnect opt-in, reference parity).
