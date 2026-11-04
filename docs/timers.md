# Timer / Ticker Audit (Phase 15)

Every timer/ticker in production code: owner, stop path, leak risk.
`time.After` is treated as a finding on hot paths (unowned one-shot
timer alive until it fires).

| Site | Kind | Owner | Stop path | Leak risk | Verdict |
|---|---|---|---|---|---|
| `client/invoke.go` retry backoff | NewTimer | invokeWithRetries call | deferred Stop; ctx wins via select | none | OK |
| `client/updates.go` emit grace | NewTimer | sync loop (emit) | deferred Stop | one timer per emit call on the overflow path — bounded by queue depth in practice; **Phase 15 fix**: replaced `time.After` (unowned, pileup risk under sustained pressure) | FIXED |
| `client/updates.go` deadlinePoll | Ticker | syncLoop | deferred Stop | none | OK |
| `client/albums.go` Next wait | NewTimer | caller (Next) | deferred Stop; ctx wins | none | OK |
| `transport/transport.go` heartbeat ping | Ticker | per-connection | deferred Stop | none | OK |
| `transport/transport.go` MaxConnectionAge | NewTimer | per-connection | stop on conn exit | none | OK |
| `transport/transport.go` backoff sleep | NewTimer | run loop | select on ctx | none | OK |
| `rpc/engine.go` request deadline | context.WithTimeout | per invoke | canceled at completion | none | OK |
| `transport` dial | context.WithTimeout | per dial | deferred cancel | none | OK |
| test-only timers | — | tests | — | — | out of scope |

## Rules going forward

1. No `time.After` on per-message/per-request paths (allocation + pileup).
2. Every NewTimer/NewTicker has a visible Stop.
3. Sleeps inside retry loops must select on a cancellation channel.
4. Long-running tickers live only in owner loops with deferred Stop.
