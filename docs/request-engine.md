# Request Engine (Phase 4)

The `github.com/Skryldev/suromesh/rpc` package executes MTProto RPC requests over the Phase 3
transport. It reuses the Phase 2 TL/crypto layers untouched; its own
responsibilities are correlation, lifecycle, and the service-message
recovery the protocol requires.

## 1. Request lifecycle

```
Invoke(ctx, req)
  ├─ 1. Serialize            req.Encode → body bytes (1 buffer/call)
  ├─ 2. Assign metadata      msg_id + seq_no (atomic pair; content-related)
  ├─ 3. Register             registry[msg_id] = pending   ← before send
  ├─ 4. Seal envelope        salt|session|TLMessage → AES-IGE (Phase 2)
  ├─ 5. Transport.Send       bounded queue; ctx bounds the wait
  ├─ 6. Await                select { done, ctx.Done }  (no goroutine/call)
  ▼
dispatcher (1 goroutine, owns inbound correlation)
  ├─ transport.Recv/Inbound  bounded channel
  ├─ MTProto decrypt         msg_key + IGE checks (Phase 2)
  ├─ handlePayload           TLMessage framing → objects
  │    ├─ rpc_result         → registry lookup → decode → finalize
  │    ├─ msg_container      → iterate: each inner correlates independently
  │    ├─ gzip_packed        → decompress → recurse (bounded depth)
  │    ├─ updates            → Updates() channel (bounded, drop-new)
  │    └─ service msgs       → salt/bad-msg/ack handling below
  ▼
pending.finalize (exactly one winner) → registry.remove → close(done)
```

Every arrow has one owner: the caller owns steps 1–5 and the wait; the
dispatcher owns 6–7 and every finalization triggered by the wire. The
canceller (ctx timeout or explicit cancel) may also finalize — the CAS in
`pending.finalize` guarantees exactly one winner, and losers are classified
(duplicate/late/unknown) rather than double-completed.

## 2. Request state machine

```
Created → Registered → Sent → AwaitingResponse → Completed
                                  ├→ RPCError
                                  ├→ Timeout        (caller ctx)
                                  ├→ Cancelled      (caller ctx)
                                  └→ ConnectionLost (reconnect/shutdown)
Registered/Sent → Cancelled (send failed or cancelled pre-response)
any state → Terminal (single atomic CAS; winner writes outcome, closes done,
                     removes the registry entry)
```

Terminal states are exactly: Completed, RPCError, Timeout, Cancelled,
ConnectionLost, BadMessage. No request can remain pending: the dispatcher,
the canceller, engine shutdown (`failAll`), and reconnect (`failAll` or
staged replay) all funnel through the same CAS — the registry is a passive
index whose entries are removed by whichever path finalizes.

## 3. Pending registry

`registry.go`: one `RWMutex` + `map[int64]*pending`. Chosen over sharded or
lock-free structures deliberately: ops are O(1) with tiny critical
sections, the dispatcher is a single goroutine, and measured contention at
1 000 producers is negligible (BenchmarkRegistryConcurrentAddComplete:
450 ns/op end-to-end add+complete). Sharding would complicate failAll and
snapshot for no measured win — revisit only with profiling evidence.

Memory: a finalized request is unreachable from the registry (the winner
removes it); `failAll` swaps in a fresh map wholesale; the fuzz suite
asserts the registry never grows under hostile input and that completed
entries are never re-completable.

## 4. Message IDs

Assigned by `idAssigner` (mutex-protected pair assignment wrapping the
Phase 2 `MessageIDGenerator`):

- monotonic: `(unix_seconds+offset) << 32 | (nanos << 2)`, guard `last+4`;
- unique: monotonic guard + single-assignment under the id lock;
- alignment: client msg_ids are always ≡ 0 (mod 4) — even — exactly like
  the reference (content-relatedness rides on seq_no parity);
- timestamp relationship: server-corrected offset (bad-msg 16/17) adjusts
  the generator and resets `Last`, preserving monotonicity across the jump;
- concurrency: verified with 1/10/100/1 000/10 000 goroutines × 100
  assignments — zero duplicates, zero invalid IDs, race-clean.

## 5. Sequence numbers

`mtproto.SeqNoGenerator` semantics (reference `_get_seq_no`):

| Message class | seq_no | counter |
|---|---|---|
| RPC request (content-related) | `2*seq + 1` (odd) | increments |
| Container wrapper around a batch | `2*seq` (even) | does not increment |
| Acknowledgements (`msgs_ack`) | even | does not increment |
| Service replies (`msgs_state_info`) | even | does not increment |
| bad-msg 32 (seq too low) | — | counter += 64 and resend |
| bad-msg 33 (seq too high) | — | counter -= 16 and resend |

Consecutive control messages legitimately share the same even seq_no
(`TestSeqNoSemantics` pins all of these).

## 6. Response correlation

`rpc_result#f35c6d01.req_msg_id` is the key. Outcomes:

| Wire situation | Engine behavior |
|---|---|
| valid response | decode → `finalize(result)` → invoke returns |
| RPC error in result | `*RPCError{Code, Type, Message, Argument}` → finalize |
| duplicate response | CAS loses → `DuplicateResponses` counter |
| late response (after timeout/cancel) | lookup misses → `UnknownResponses` |
| unknown message ID | same as late; never creates state |
| malformed response bytes | decode error contained per message → finalize(err) for that call only |
| connection error | see §10 |
| container response | each inner message correlated independently; a failure in one never touches siblings |
| gzip-wrapped result | decompressed (bounded) then correlated |
| update | routed to `Updates()`, never to a pending call |

## 7. RPC error model

`RPCError{Code, Type, Message, Argument}` — the wire error_message is
split into its alpha type and numeric argument (`FLOOD_WAIT_7` → type
`FLOOD_WAIT`, argument 7). `RetryAfter()` exposes the wait for
FLOOD_WAIT/FLOOD_PREMIUM_WAIT/SLOWMODE_WAIT/FLOOD_TEST_PHONE_WAIT. The
error supports `errors.As`; the full 534-entry errors.csv mapping to
distinct exception types belongs to the client layer (Phase 6) and will
build on this type.

## 8. Container handling

Outbound batching (multiple requests per envelope + `msg_container`) is
**deferred**: the reference packs N requests per container, but the
measured win is small for interactive workloads (envelopes here are ≤ ~200
bytes vs the 1 MiB container budget) while batching adds head-of-line
coupling between unrelated callers and complicates per-request timeouts.
Inbound containers are fully supported and correlated independently
(`TestInvokeContainerResponse`, `TestDispatchContainerCorrelatesIndependently`).
Revisit outbound batching with a real-server profile (Phase 9 gate).

## 9. Context cancellation

`Invoke` wraps the caller's context with the effective deadline (explicit
ctx deadline > engine `DefaultTimeout` (30 s) > none). Cancellation
propagates: to `Transport.Send` while queued (backpressure unblocks) and
to the wait select after send. The response-vs-cancel race resolves by CAS:
whoever finalizes first wins; if the response wins, `Invoke` returns the
real result instead of `ctx.Err()` — exactly-once, no send-on-closed, no
goroutine per request.

## 10. Timeout implementation

One `context.WithTimeout` per request — a heap timer, not a goroutine
(requirement: no timer goroutine per request). Firing cancels the ctx; the
waiter finalizes with `DeadlineExceeded`, removes the registry entry, and
counts `RequestsTimedOut`. A response that arrives later misses the
lookup and is counted unknown — it can never resurrect state.

## 11. Connection-loss semantics

When the transport enters Reconnecting, every handed-off byte is in the
ambiguity window:

```
Client → Server   request received, executed, response lost on a dead socket
```

Default policy (`RetryOnReconnect=false`): **fail all pending requests
deterministically with `ErrConnectionLost`**. No blind retransmission — a
retry of `messages.sendMessage` can duplicate a user-visible message.

## 12. Reconnect interaction

`RetryOnReconnect=true` reproduces the reference behavior: pending
requests are staged at Reconnecting (snapshot, non-blocking) and replayed
by the dispatcher after the replacement connection is up — re-registered
under fresh msg_ids (the reference re-assigns ids and clears pending
state on reconnect). The replay runs on the dispatcher, never on the
transport's lifecycle goroutine (a callback that blocked in Send would
deadlock the state machine — found and fixed via `TestRetryOnReconnectReplays`).
Classification remains: safe-to-retry (idempotent reads), unsafe
(mutations — require application policy, Phase 6 will expose it), unknown
(the conservative default).

## 13. Backpressure integration

`Invoke` → `Transport.Send(ctx, …)`: under PolicyBlock a full transport
queue blocks the caller with ctx bounds (cancellation unblocks it);
under PolicyReject it fails fast and the pending is finalized
deterministically. The engine adds no queue above the transport — the
only engine-side buffering is the bounded updates channel (drop-new) and
the bounded ack batch (flushed every dispatcher iteration).

## 14. Shutdown

`Engine.Close`: state flips closed (new Invokes fail fast) → dispatcher
cancelled → `failAll` finalizes every pending with `ErrEngineClosed` →
`done` closes → blocked Invokes return. Verified with zero, one, thousands
of pending requests, and Invokes racing `Close`
(`TestShutdownWithThousandsPending`, `TestRequestCreationRacingShutdown`).

## 15. Allocation profile (windows/amd64, Go 1.26.3)

| Benchmark | Result |
|---|---|
| Registry lookup | 11 ns, 0 B, 0 allocs |
| Registry complete (finalize+remove) | 218 ns, 0 B, 0 allocs |
| Registry add | 338 ns, 300 B, 2 allocs (map insert + pending) |
| Concurrent add+complete (1 000 goroutines) | 450 ns/op, 2 allocs |
| Request serialize (ping) | 14 ns, 64 B, 1 alloc |
| Envelope seal (frame+IGE) | 680 ns, 1.2 KiB, 13 allocs |
| End-to-end request/response | ~11 KiB, ~24 allocs (incl. transport WS framing + fake peer) |

Known allocation hotspots (logged, not yet optimized — no profile-driven
justification yet): per-request Writer + body copy, envelope padding/IGE
buffers, per-frame WS mask+copy. Candidates for Phase 9 pooling.

## 16. Load results (fake server, immediate echo)

| Concurrency | Throughput | p50 | p95 | p99 | Goroutines | Failures |
|---|---|---|---|---|---|---|
| 10 | 18 k/s* | 0.5 ms | 0.5 ms | 0.5 ms | 10 | 0 |
| 100 | 96 k/s | 0.5 ms | 1.0 ms | 1.0 ms | 17 | 0 |
| 1 000 | 75 k/s | 5.1 ms | 8.9 ms | 9.4 ms | 24 | 0 |
| 10 000 | 75 k/s | 13.6 ms | 15.2 ms | 16.1 ms | 31 | 0 |
| 100 000 | 76 k/s | 13.3 ms | 14.3 ms | 14.7 ms | 38 | 0 |

*10-request sample is noise-dominated. The ~76 k/s ceiling is the fake
peer (single loop goroutine, per-frame allocations in the test double),
not the engine. Goroutine count stays bounded (engine adds exactly one
dispatcher; producers are the caller's own goroutines).
