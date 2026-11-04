# Runtime Audit (Phase 15)

The repository-wide runtime audit: ownership, goroutines, channels,
cancellation, persistence, retries, resource bounds, and failure
propagation. No production change in this phase preceded this document.

## 1. Ownership graph (§3)

| Subsystem | Owner | State owned | Second owner? |
|---|---|---|---|
| Client lifecycle | `Client` (lifecycle state machine + `closed` + `lifecycleCtx`) | terminal shutdown authority | none |
| Transport | `transport.Transport` | connection, reconnect loop, backoff budget | none (client triggers via Close) |
| RPC engine | `rpc.Engine` | pending registry, recvLoop, done ctx | none |
| Pending RPCs | `rpc.registry` | in-flight requests + their single terminal outcome | none |
| Update ordering | `MessageBox` (owned by updateManager sync loop) | pts/qts/seq, gaps, diff state | none |
| Update delivery | `updateManager` (raw queue → syncLoop → event queue → workers) | queues, subscriber registry | none |
| Transfers | `transferManager` | semaphore, active registry, terminal CAS | none |
| Entity cache | `entityCache` | access hashes (server-authoritative) | none |
| Conversation store | `conversationStore` | derived read/unread/top state | none |
| Session persistence | `session.Session`/`FileSession` | key, auth state, update state, file bytes | none |
| Subscriber lifecycle | `Subscription` + updateManager registry | ACTIVE/UNSUBSCRIBED/CLIENT_CLOSED | none |

**Verdict: no double ownership found.** The Phase 15 Close-barrier
strengthening and terminal-CAS guards (below) are hardening, not
re-ownership.

## 2. Goroutine graph (§2)

Every client-owned goroutine: owner, creation point, input, output,
cancellation source, termination condition, survives reconnect?, survives
Close?

| Goroutine | Owner | Created | Input | Output | Cancelled by | Terminates when | Reconnect? | Close? |
|---|---|---|---|---|---|---|---|---|
| transport `run` | Transport | `Connect` | dial results | connection lifecycle | transport ctx (= user Close) | ctx done / terminal violation / budget exhausted | yes (owns reconnect) | yes (terminal) |
| transport `reader` | Transport | per connection | wire bytes | decoded msgs → engine | connCtx | conn loss / Close | no (dies with conn) | yes |
| transport `writer` | Transport | per connection | outgoing frames | wire bytes | connCtx | conn loss / Close | no | yes |
| transport `heartbeat` | Transport | per connection | ticker | pings | connCtx | conn loss / Close | no | yes |
| engine `recvLoop` | rpc.Engine | `Start` | decoded msgs | pending completion / updates channel | engine done | Close / terminal | no (re-created per engine) | yes |
| update `syncLoop` | updateManager | `start` | raw queue | ordered events, conversation sync | stopCh | stop (Close) | no (manager torn down on reconnect) | yes |
| update `rawFeeder` | updateManager | `start` | engine.Updates() | raw queue | stopCh | stop | no | yes |
| update `worker` ×N | updateManager | `start` | event queue | subscriber fan-out | stopCh | stop | no | yes (waits in-flight callback) |
| subscriber callback | caller | delivery | update | caller code | caller's ctx | returns | — | Close waits for in-flight |
| test/diag `ctxmerge` pump | per operation | mergeContext | ctx pair | cancel propagation | either ctx done | fires cancel then exits | no | yes |

**No goroutine survives Close.** Verified by the 100-cycle leak test
(`TestRuntimeGoroutineLeak100Cycles`).

## 3. Channel graph (§9 backpressure)

| Channel | Cap | Producer | Consumer | Overflow policy | Metric | Shutdown |
|---|---|---|---|---|---|---|
| engine.updates | UpdatesQueue (1024) | dispatcher | rawFeeder | DROP_NEWEST + UpdatesDropped | engine.UpdatesDropped | drained by close |
| update rawQ | 4096 | rawFeeder | syncLoop | DROP_OLDEST (preserves pts frontier) | counted via received−ingested gap | closed with manager |
| update events | UpdateQueue (1024) | syncLoop emit | workers | grace 250 ms → DROP_NEWEST + hook | dropped counter | workers exit on stopCh |
| transfer sem | MaxConcurrentTransfers | begin | Upload/Download | BLOCK (caller ctx) | Active metric | stopCh unblocks |
| album collector next | 128 | flush | Next | bounded drop + Dropped() counter | Dropped() | caller-owned |

All policies explicit; none implicit. Slow-subscriber pressure is
absorbed by the events queue + drop policy; slow-syncLoop pressure by
rawQ drop-oldest (protecting the pts frontier); server flood by
engine-level pending limit.

## 4. Cancellation graph (§4)

`Client.Close` = global terminal barrier:

```
Close
 ├─ closed.Store(true)                 (Invoke/OnUpdate guards)
 ├─ lifecycleCancel()                  (mergeContext → every op ctx)
 ├─ updateManager.stop()               (syncLoop/rawFeeder/workers; waits)
 ├─ engine.Close()                     (failAll pending; recvLoop done)
 ├─ transport.Close()                  (run loop terminal; conn goroutines)
 ├─ transferManager.stop()             (stopCh; waiters get ErrTransferClosed)
 └─ transition → StateClosed           (after all stops complete)
```

Close waits for: sync loop drain, raw feeder exit, worker exit (so
in-flight subscriber callbacks complete before Close returns). Pending
RPCs are failed with ErrEngineClosed by engine.Close. Transfer waiters
unblock with ErrTransferClosed.

## 5. Persistence graph (§11)

Saver invocations: auth exchange (connect), logout, update-state
coalesced flush (≤ 1/s, shutdown-forced). FileSession.Save: mutex +
writing flag, temp file → fsync → rename (atomic replacement), HMAC
integrity envelope, 0600. Failure never mutates in-memory state (errors
returned to the caller / logged via metrics only).

## 6. Retry graph (§7)

Single authority: `client/invoke.go invokeWithRetries` + rpc engine's
connection-level semantics. Full matrix in docs/retry-policy.md. No
subsystem invented its own policy (media retries are part-level per the
reference; update difference retries are deadline-gated per the
reference's MessageBox flow).

## 7. Resource-bound graph (§8)

See docs/resource-budgets.md — every structure classified
BOUND/STREAMED/CALLER-OWNED/PROVEN-CONSTANT.

## 8. Failure propagation (§20)

See docs/failure-matrix.md — detection/recovery/state impact per
failure, with the tests that pin each row.

## 9. Timer audit (§10)

See docs/timers.md. One finding fixed this phase: the update overflow
path allocated a one-shot timer per emit; now a scoped timer per emit
call with Stop (bounded lifetime, no pileup) — the sync-loop-only
caller makes a shared reusable timer safe in future if needed.

## 10. Phase 15 production changes

1. `updates.go emit`: `time.After` → scoped `time.NewTimer` + Stop
   (timer-audit finding: unbounded one-shot timer churn on the overflow
   path under sustained slow-consumer pressure).
2. `upload.go`/`download.go`: deferred terminal-CAS guard — a panic
   unwinding through a transfer now settles the transfer exactly once
   before propagating (invariant: no slot strand, no missing terminal
   state). Panics still propagate (programmer error is never swallowed).
