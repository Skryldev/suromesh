# Observability

The client exposes two complementary mechanisms: **Hooks** (event
callbacks, push) and **Metrics** (counter snapshot, pull). Neither
requires a logging/metrics framework; adapt slog/zap/zerolog/prometheus
by wrapping them.

## Hooks (push)

| Hook | Fires on | Payload |
|---|---|---|
| OnStateChange | every client lifecycle transition | from, to State |
| OnAuthenticated | connection established with a valid key | (none) |
| OnRPCStart | Invoke begins (after guards) | constructor name |
| OnRPCComplete | Invoke settles | method, duration, error |
| OnRPCRetry | automatic retry | method, attempt, cause |
| OnUpdateReceived | update enters the subsystem | constructor ID |
| OnSlowConsumer | update queue overflow → drop | queue depth, dropped total |
| OnSubscriberPanic | an update handler panicked | (none — payload may contain user content) |
| OnTransportError | classified transport error | error, Classification |

Contract: hooks MUST NOT block (they run on lifecycle/RPC paths); hooks
never receive auth keys, nonces, AES key/IV material, or encrypted
payloads.

## Metrics (pull)

`c.Metrics()` returns a point-in-time snapshot. One atomic load per
counter + a few RLocks — safe to poll at metering intervals (1–15 s);
not for per-request hot-path polling.

| Metric | Source |
|---|---|
| State / TransportState | client + transport state machines |
| ConnectionsTotal / Reconnects / ConnFailures | transport |
| BytesSent / BytesReceived | transport |
| InvokesStarted / Completed / Errors | rpc engine |
| InvokeRetries | rpc resends |
| PendingRequests / PendingLimit / PendingLimitHits | rpc registry |
| UpdatesReceived / Delivered / Dropped | update manager |
| UpdateQueueDepth / Cap / Workers / Subscribers | update manager |
| EntityCacheEntries | entity cache |
| SessionState | session |

## Cardinality policy

The built-in snapshot carries NO labels — no user IDs, chat IDs, message
IDs, or key material. Per-method labels would bound cardinality at the
schema's ~400 constructors; we omit them from the snapshot (callers can
derive them from OnRPCStart/Complete hooks if they accept the
cardinality). This keeps the metrics surface safe for direct exposure to
Prometheus-style systems without label explosion.

## Session events

Session persistence emits no hooks directly; the client's
OnAuthenticated covers the key-establishment moment, and FileSession
corruption surfaces as an `ErrCorrupt` error from `OpenFile` (loud, never
silent). Session save failures propagate to the Connect caller.

## Phase 9 authentication metrics addendum

Aggregate counters only (no labels, no identifiers):
AuthSendCodeTotal, AuthSignInTotal, AuthSuccessTotal, AuthFailureTotal,
AuthPasswordReqTotal, AuthLogoutTotal, AuthFloodWaitTotal, plus the
AuthState string. Behavioral tests prove phone numbers, codes,
phone_code_hash values, passwords, and keys never appear in the metrics
or hook surfaces.

## Phase 10 update metrics addendum

New aggregate counters (no labels, no identifiers): UpdatesAccepted,
UpdatesDuplicate, UpdatesStale, UpdatesUnsupported, UpdateGaps,
DifferenceRequests, ChannelDifferenceRequests, DifferenceFailures.
New panic-isolated hooks: OnUpdateGap(kind), OnDifferenceSync(scope) —
kinds/scopes are fixed strings ("gap", "unsupported family", "account",
"channel"); no payload, no user content, no identifiers.

## Phase 13 media addendum

Media operations reuse the existing aggregate counters (transfer
metrics: bytes up/down, upload retries; mutation metrics) — no new
labels. Media bytes, filenames, captions, MIME values, media IDs and
access hashes never enter metrics or hooks.
