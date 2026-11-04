# Runtime Architecture (Phase 15)

The central runtime document: the full stack, ownership, and
cancellation boundaries.

```
Client (lifecycle authority: closed flag, lifecycleCtx, state machine)
 ├── Session (key, auth state, update state, persistence)
 ├── Transport (connection + reconnect authority; reader/writer/heartbeat)
 ├── RPC Engine (pending registry + recvLoop; single terminal outcome per request)
 ├── Update Engine (rawQ → syncLoop/MessageBox → eventQ → workers → subscribers)
 │     └── MessageBox = SOLE ordered protocol state (pts/qts/seq/gaps/diff)
 ├── Transfer Manager (semaphore + active registry + terminal CAS)
 ├── Entity Cache (server-authoritative hashes)
 ├── Conversation Store (DERIVED state; sync-loop writer)
 └── Conversation Layer (peers/dialogs/history/search/mutations/media/albums)
```

## Ownership boundaries

- MessageBox is the ONLY update-ordering authority. ConversationStore is
  derived. Entity cache is server-authoritative. The pending registry is
  the only pending-request authority. TransferManager is the only
  transfer-worker authority. Client lifecycle is the only terminal
  shutdown authority. (Proof by audit: docs/runtime-audit.md.)

## Cancellation boundaries

`Close` is the global terminal barrier (docs/runtime-audit.md §4):
lifecycle cancel → update manager stop (waits) → engine close (fails all
pending) → transport close → transfer stop. Every operation context is
`mergeContext(callerCtx, lifecycleCtx)` — caller cancellation and Close
both win over retry/backoff sleeps.

## Persistence boundaries

Session persistence (FileSession): atomic temp+fsync+rename, HMAC
envelope, mutex-serialized. Persistence failure never mutates in-memory
authorization state. Update state persists coalesced (≤ 1/s) with a
forced shutdown flush.

## Recovery boundaries

Reconnect = transport-owned connection recovery; the engine fails
in-flight ambiguous requests (ErrConnectionLost); MessageBox state
survives (pts/session-persisted); gap recovery runs through difference.
Authentication never re-runs on reconnect (key + authorized state are
session-backed).

## Subsystem docs

- docs/runtime-audit.md — full graphs
- docs/retry-policy.md — classification matrix
- docs/resource-budgets.md — bounds
- docs/timers.md — timer audit
- docs/iterators.md — iterator lifetimes
- docs/error-model.md — taxonomy
- docs/failure-matrix.md — failure table
