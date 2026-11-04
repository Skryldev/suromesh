# Transfer Manager Semantics (Phase 8)

`client/transfer.go` owns the concurrency, lifecycle, and observability of
every file transfer. Engines (`upload.go`, `download.go`) never launch
goroutines of their own; they hold a manager slot for the duration of one
transfer.

## Lifecycle

```
            begin()                 finish()
Pending ───────────► Running ────────────► Completed
   │                    │
   │                    │  CAS-once terminal transitions
   ├────────────────────┼──────────────────► Failed
   └────────────────────┴──────────────────► Cancelled
```

- `begin(ctx, kind, total)` — acquires a slot (blocking on the caller's
  ctx), registers the transfer, returns `(transfer, tctx, release, err)`.
  `tctx` is the caller's ctx plus manager cancellation. `release` cancels
  the context, deregisters, and frees the slot; engines defer it exactly
  once.
- Terminal transitions are `atomic.CompareAndSwap` — the first
  `finish(state)` wins; later calls are no-ops. A transfer can therefore
  never be both failed and cancelled.
- Cancellation surfaces as `context.Canceled` from the engine loop and is
  classified by `classifyTransferEnd` (ctx dead → Cancelled, else Failed).

## Concurrency bounds

| Bound | Default | Enforced by |
|---|---|---|
| Active transfers | 16 | semaphore channel (`sem`), one token per transfer |
| Upload workers per file | 1 (reference-sequential) | sequential part loop |
| Progress rate | 250 ms / per part | inline per part, panic-isolated |

The semaphore is the single admission gate: `TestConcurrentTransfersBounded`
starts 12 uploads against 4 slots and asserts — via a gate inside each
upload's reader — that exactly 4 become active while the remaining 8 stay
queued, then all complete with exact metrics.

Why one token per whole transfer (not per part): memory is bounded by
`part_size` per transfer either way, and per-transfer tokens keep
`Active` meaningful and slot accounting leak-proof
(`m.Active == 0` asserted after every stress test).

## Progress contract

- Callbacks run **inline** on the transfer goroutine, never under manager
  locks, never concurrently for the same transfer.
- A panicking callback is recovered in `reportProgress`; the transfer
  continues (`TestProgressPanicIsolated`).
- `TransferProgress.Current` counts bytes confirmed by the server (upload)
  or written to the sink (download); `Total` is the declared size (0 =
  unknown).

## Failure taxonomy

| Outcome | Trigger | Metric |
|---|---|---|
| `Completed` | all parts/chunks + integrity check passed | `completed` |
| `Failed` | RPC error, reader error, short read, integrity mismatch | `failed` |
| `Cancelled` | caller ctx, manager shutdown, `CancelTransfer` | `cancelled` |

Shutdown ordering: `stop()` closes `stopCh` (new `begin()` fails with
`ErrTransferClosed`) → cancels every active transfer's context → engines
observe ctx death at the next loop iteration and settle with errors.
`TestCancelAllTransfersOnClose` proves all in-flight transfers settle.

## Metrics snapshot

`Client.TransferMetrics()` returns atomically loaded counters
(`TransferMetricsSnapshot`): started, completed, failed, cancelled,
bytesUp, bytesDown, upload retries, active count. Stress tests reconcile
them exactly against the transferred byte volumes.
