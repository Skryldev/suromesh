# Runtime Reliability Results — AccountRunner & Manager (Phase 27 §7)

Executed: 2026-11-24 · Go 1.26 (Docker `golang:1.26-alpine`) · linux/amd64 ·
`-race` in the CI pipeline (`scripts/ci.sh race`) · all results below are
from actual runs recorded in `evidence/phase27/` (JUnit XML:
`phase27/junit/service-race.xml`, `service-unit.xml`).

## Contract tests added in Phase 27

| # | Behavior pinned | Test | Result |
|---|-----------------|------|--------|
| 1 | Cancel BEFORE dispatch: op runs 0 times, caller gets `context.Canceled` | `TestReliability_CancelBeforeDispatch` | PASS |
| 2 | Cancel DURING dispatch: caller gets `ctx.Err()`, op completes exactly once, pump stays healthy for later callers | `TestReliability_CancelDuringDispatch` | PASS |
| 3 | Shutdown with pending work: every queued mutation fails `ErrAccountStopped`; in-flight op may complete (≤1 executed); Stop is bounded (1s budget honored, no deadlock); post-stop Mutate rejected typed | `TestReliability_ShutdownWithPendingMutations` | PASS |
| 4 | Queue saturation: blocked pump + full queue ⇒ immediate `ErrTooManyRequests` (bounded probe, 64 async attempts vs queue depth 4) | `TestReliability_QueueSaturationDeterministic` | PASS |
| 5 | Concurrent mutation requests: 64 concurrent callers, max observed concurrency 1 (serialized), all complete | `TestReliability_ConcurrentMutations` | PASS |
| 6 | Duplicate runner prevention under race: 16 concurrent `StartRunner` for one account → exactly 1 winner, exactly 1 upstream client constructed | `TestReliability_DuplicateStartRunnerRace` | PASS |
| 7 | Failed Start closes the constructed client (no leaked client across 3 failed attempts) | `TestReliability_StartFailureClosesClient` | PASS |
| 8 | Repeated start/stop cycles via Manager: exactly one fresh client per cycle (5 cycles), every cycle reaches a live client | `TestReliability_RepeatedStartStopCycles` | PASS |
| 9 | Repeated connect/disconnect cycles (10×): no wedge, observation tracks last transition | `TestReliability_RepeatedConnectDisconnectCycles` | PASS |
| 10 | Goroutine hygiene: 23 runner cycles return goroutine count to baseline (2s poll budget; warm-up cycle excluded) | `TestReliability_GoroutineHygiene` | PASS |
| 11 | Subscriber cleanup: 8 subscribers all released (channels closed) on runner stop; subscribe-after-stop fails typed | `TestReliability_SubscriberCleanupOnRunnerStop` | PASS |

Pre-existing coverage (unchanged, still green): mutation serialization,
concurrent reads under semaphore, session persist on upstream signal,
stop during blocked mutation, slow-subscriber drop policy, hub delivery
ordering, tenant-scoped manager Get, plus the stress battery
(`TestStress_*`: throughput/ordering, saturation, concurrent
subscribe/unsubscribe, multiple subscribers, idempotent stop).

## Defects found & fixed during this audit (with regression tests)

1. **Concurrent `StartRunner` double-create** — two concurrent starts for
   the same account could each construct an upstream client (one leaked).
   Fix: per-account in-flight start coordination (`manager.go`
   `startFlight`); losers join the winner's outcome deterministically.
   Evidence: test 6 (16-way race, `f.ClientCount() == 1`).
2. **Leaked client on failed Start** — `Start` failures after client
   construction (e.g. AutoConnect failure) never closed the client; a
   retry stacked a second client on the leaked first.
   Fix: `closeOnFailure` in `hub.go` Start.
   Evidence: test 7.
3. **Mutate enqueue/completion race** — a caller whose request was
   enqueued just as the pump drained it could block until its own ctx
   (potentially forever with Background ctx). Fix: fast-path done check +
   `pumpDone` guard in the wait select; documented cancellation semantics
   (§7 of the report): cancellation does NOT roll back in-flight work —
   callers reconcile via the idempotency ledger.
   Evidence: tests 1–3.
4. **Upload handler double-close panic** (process crash) and
   **download worker leak on cancel** — see
   `security-findings.csv` SEC-15/SEC-16; regression tests
   `TestMediaUpload_ScriptedSuccessCompletesWorker`,
   `TestMediaUpload_ScriptedWorkerFailureMapped`,
   `TestMediaDownload_ClientCancelReleasesWorker` (all PASS).

## Single-replica operating constraint (verified, unchanged)

- `deployment.yaml`: `replicas: 1` + `strategy: Recreate` (in-memory
  account ownership per ADR-0002). Scaling beyond 1 replica is a
  documented failure mode (undefined account ownership, duplicate
  upstream sessions), not a silent one: there is no code path that
  claims cross-process safety.
- Multi-replica ownership remains gated on a lease+fencing design
  (open owner decision, unchanged from Phase 26).

Limitations: tests exercise the fake-upstream harness (deterministic;
the real SDK adapter's Close/connect semantics are covered by the SDK's
own lifecycle/stress suites in `client/`). Live reconnect behavior under
real network loss remains BLOCKED_NO_LIVE_ACCOUNT (documented in the
test matrix).
