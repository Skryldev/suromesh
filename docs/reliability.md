# Reliability

## Resource ownership model

Every long-lived goroutine, channel, and timer has a single owner and a
termination path.

| Resource | Owner | Created | Terminated | Bounded |
|---|---|---|---|---|
| Transport reader/writer/heartbeat | transport | transport.Connect | transport.Close | RecvTimeout 90 s, write deadline |
| RPC dispatcher (recvLoop) | rpc.Engine | engine.Start | engine.Close / ctx cancel | read deadline 90 s |
| Update feeder | client.updateManager | manager.start | manager.stop | bounded queue 1024 |
| Update workers (×N) | client.updateManager | manager.start | manager.stop | N=4 default |
| Auth exchange | client.Connect | Connect | Connect return / ctx | bounded 3 steps |
| Retry timer | client.Invoke retry loop | per retry | ctx cancel / timer fire | ≤ 2 s per sleep |
| Pending requests | rpc.registry | Invoke | finalize (exactly once) | MaxPending 8192 |
| Entity cache | client.entityCache | New | Clear/Close | 4096 entries |
| Session file | session.FileSession | OpenFile | Save/Delete | one fd at a time |

## Pending-request bound

`rpc.Config.MaxPending` (default 8192) caps the pending registry. At
capacity, `Invoke` fails with `rpc.ErrPendingLimit` **before any bytes are
sent** — a deterministic, retry-safe rejection that protects memory when
the server stalls. Freed on every finalize; the bound is enforced under
the registry's RWMutex.

## Envelope padding (Phase 7 fix)

The Phase 4 dispatcher looped over remaining payload bytes after the first
TLMessage, misreading the envelope's IGE padding (payload length aligned to
16) as garbage messages. The soak test surfaced this (10 001 spurious
protocol errors). Fixed: handlePayload decodes exactly one TLMessage;
multiple messages ride inside `msg_container`.

## Close semantics

`Close` cancels the client's lifecycle context FIRST (interrupting
in-flight Connects, Invokes, and retry backoff sleeps), then acquires the
connect mutex, tears down engine/updates/transport, and transitions to
Closed. Guaranteed: no goroutine survives Close; pending requests fail
with `ErrEngineClosed`; subsequent operations return `ErrClosed`.

## Lifecycle state legality

| Operation | New | Disconnected | Connecting | Connected | Ready | Closing | Closed |
|---|---|---|---|---|---|---|---|
| Connect | ✓ | ✓ | ErrConnectInProgress | ✓ (idempotent) | ✓ (idempotent) | ✗ | ✗ |
| Invoke | ✗ ErrNotReady | ✗ ErrNotReady | ✗ ErrNotReady | ✓ | ✓ | ✗ | ✗ ErrClosed |
| OnUpdate | inert sub | inert sub | ✓ | ✓ | ✓ | inert | inert |
| Disconnect | no-op | no-op | waits | ✓ | ✓ | ✗ | no-op |
| Close | ✓ | ✓ | ✓ (interrupts) | ✓ | ✓ | ✓ | no-op (ErrClosed) |

Failed Connect: unwinds completely, lands in Disconnected, `LastError`
holds the cause; the client is immediately re-Connectable.

## Reconnect

`Connect` reuses the persisted auth key — the DH exchange runs only when
`session.AuthKey() == nil`. Reconnect latency (fake server, 5 cycles):
~100 µs average. On transport death while Ready: state → Disconnected;
pending requests fail with `rpc.ErrConnectionLost` (no blind retry);
`Connect` re-establishes everything.

## Cancellation

Every blocking operation accepts a context: Connect, Invoke (both the
network wait and retry backoff sleeps), Disconnect. `Close` cancels an
internal lifecycle context spliced into every operation via
`mergeContext`, so `Close` interrupts work even when the caller passed
`context.Background()`. Cancellation identity is preserved:
`context.Canceled` / `context.DeadlineExceeded` are never wrapped into
RPC errors (verified by `TestShortDeadlineCancellation` with 1–50 ms
deadlines and `TestErrorClassification`).

## Soak results (fake server, windows/amd64, Go 1.26.3)

| Metric | Value |
|---|---|
| Invokes | 10 000 (20 workers, mixed pong/update batches) |
| Invoke errors | 0 |
| Update batches | 999 (the n=0 marker is a test edge case, not a defect) |
| Updates delivered | 999 (1 per batch; per-update fan-out at the handler layer) |
| Updates dropped | 0 |
| Goroutines before/after | 15 / 15 |
| Heap in use after | 5.1 MiB |
| Pending after | 0 |
| Reconnect cycles | 5, avg latency ~100 µs |
| Cache bound | 4096 entries after 100 000 updates, heap 2.1 MiB |

## Phase 9 authentication reliability addendum

- Cancellation: SendCode/SignIn/CheckPassword splice the caller's context
  with the lifecycle context — caller cancellation and `Close` win over
  retry sleeps and settle deterministically with no state mutation
  (verified by 32× cancellation and close-during-auth storms).
- Retry: auth requests reuse the Phase 6/7 engine retry policy; the only
  flow-level retries are the reference's own (AUTH_RESTART ≤3 attempts,
  resend-on-expired). RPC rejections are never retried.
- Logout tears the transport down; auth operations fail loudly with
  ErrNotReady until reconnect — no silent use of an unbound key.
- Revocation vs. recoverable 401s are classified (revoked: SESSION_REVOKED,
  USER_DEACTIVATED, AUTH_KEY_DUPLICATED; recoverable: AUTH_KEY_UNREGISTERED,
  AUTH_KEY_INVALID).

## Phase 10 update-consistency addendum

- Gap handling: out-of-order updates are held (bounded 256/entry, 100 ms
  self-resolution window) and reconciled; expired gaps force difference
  recovery — a gap can never silently disappear (counted, hooked, and
  recovered).
- Difference recovery is bounded and loop-free: one recovery at a time,
  ended on success or error, re-triggered only by new gaps or the
  15-minute no-update deadline; the reference's error matrix decides
  retry vs forget (banned channels) vs auth effects (401-class).
- Exactly-once delivery under reconnect: the pts frontier survives
  restarts (persisted session state); missed updates recover through the
  standard difference flow — never a blind state reset.
- Overload behavior: every queue is bounded with an explicit drop policy
  (engine channel: drop-newest with counter; raw queue: drop-oldest to
  preserve the pts frontier; event queue: Phase 7 drop-newest +
  slow-consumer hook). 100k-update streams add zero historical memory.
- Close interrupts everything: raw waits, difference RPCs, deadline
  ticks, subscriber delivery — verified by close-during-difference tests.

## Phase 13 media addendum

- FILE_REFERENCE_EXPIRED during download triggers exactly one reference
  refresh (message re-fetch + identity guard) then one retry — the
  reference downloads.py:120-148 flow, loop-free by construction.
- Album collectors are bounded and cancellation-safe; capacity overflow
  flushes rather than drops or grows.
- Existing-media resends never touch the upload engine; transfer
  concurrency remains owned by the Phase 8 transferManager.

## Phase 14 addendum

- Reply resolution failures (deleted/inaccessible targets) surface as
  ErrReplyUnavailable — never fabricated targets, never hidden retries.
- Global search terminates on the shared pagination-safety discipline;
  per-ID dedup is intentionally absent for cross-chat results (IDs are
  per-peer — the reference's own rule), with the bottom-reached and
  stall guards covering termination.
- Read-state maxima only advance; hostile negative max-id updates
  cannot un-read messages (fuzz-pinned).
- Drafts deferred with the documented schema constraint (see
  docs/drafts.md) — no half-implemented state.
