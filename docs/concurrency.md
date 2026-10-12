# Concurrency Model — Transport (Phase 3)

This document is the authoritative description of goroutine ownership,
channel/lock ownership, shutdown ordering, cancellation propagation, and
backpressure in the `github.com/Skryldev/suromesh/transport` package. The Phase 2 protocol
packages are single-goroutine libraries with no internal goroutines; all
concurrency lives here.

## Goroutine inventory

A running transport has exactly **1 + 3×N goroutines**, where N is the
number of live connections (N is 0 or 1; a broken connection's goroutines
exit before the replacement's start):

| Goroutine | Count | Lifetime | Ownership |
|---|---|---|---|
| `run` (lifecycle owner) | 1 | `Connect` → terminal/Closed | Owns dialing, reconnect budget, connection replacement, terminal state. The **only** goroutine that starts/stops per-connection goroutines. |
| `reader` | 1 per conn | conn → death | Sole caller of `ws.Conn.ReadMessage`. Owns the abridged `Decoder` (its buffer) and the de-obfuscation direction. Pushes packets into `inbound`. |
| `writer` | 1 per conn | conn → death | The **only data writer**. Sole consumer of `outbound`. Owns the reused abridged encode buffer. |
| `heartbeat` | 1 per conn | conn → death | Enqueues ping items (non-blocking) and samples queue depth. Never touches the socket directly. |

There is no goroutine per message, no hidden goroutines, and no goroutine
spawns on the hot paths. All per-connection goroutines are tracked in one
`sync.WaitGroup` (`runWG`) that the lifecycle owner waits on before
recycling a connection.

## Channel ownership

| Channel | Capacity | Producer → Consumer | Full behavior |
|---|---|---|---|
| `outbound` | `Config.OutboundQueueSize` (default 1024) | `Send` callers + heartbeat → `writer` | PolicyBlock (default): producer blocks with context — end-to-end backpressure. PolicyReject: `ErrQueueFull` immediately. Ping items are **non-blocking**: a saturated queue skips a ping (traffic itself proves liveness). |
| `inbound` | `Config.InboundQueueSize` (default 1024) | `reader` → `Recv` callers | Blocking push: when full, the reader stops reading the socket, the TCP receive window fills, and the server slows — memory stays bounded by the queue plus one in-flight frame. |
| `errCh` (per conn) | 3 | reader/writer/heartbeat → lifecycle owner (via `sync.Once` first-report) | Buffered; never blocks. First error wins; the owner recycles the connection. |
| `done` | signal | lifecycle owner exit → `Close`/`Recv` waiters | Closed exactly once (`sync.Once`). |

Queues are **never** recreated on reconnect: packets queued but unsent at
connection death are transmitted on the replacement connection. Bytes
already handed to a dead socket are lost — retransmission is the request
engine's responsibility (Phase 4), matching the reference's sender.

## Lock ownership

| Lock | Protects | Contention profile |
|---|---|---|
| `stateMachine.mu` | the lifecycle state word | Taken only for transitions (dial/reconnect/close events) — never on the packet path. |
| `ws.Conn.writeMu` | concurrent frame writes | Safety net only: the transport's single-writer design means it is uncontended. Pong auto-replies from the read path are the only legitimate concurrent writer. |
| `SoroushPeer.mu` (wstest only) | fake-peer frame log | Test-only. |

There are no locks on the packet path: encode, obfuscation, and framing are
lock-free in the writer; decode likewise in the reader. Cross-goroutine data
transfer happens exclusively through the bounded channels above.

## State machine

```
  New ──Connect()──▶ Connecting ──dial ok──▶ Connected
                       │  ▲                    │ │
        dial fail,     │  └───── conn lost ────┘ │
        budget left ───┴──▶ Reconnecting ◀───────┘
                            │   │
             dial ok ──────┘   └─ budget exhausted / fatal ──▶ Closed
  any state ───────────── Close() ────────────────────────────▶ Closed
```

- Transitions are enforced by a legality table (`legalTransition`) — invalid
  moves are **impossible**, not merely discouraged (`ErrBadState` semantics).
- Terminal states: `Closed` only. Protocol violations terminate the
  transport (`ErrProtocolFatal`) instead of reconnect-looping against a
  broken peer; budget exhaustion terminates with `ErrReconnectExhausted`.
- `Healthy`/`Degraded` are deliberately not states: a connection is either
  usable or not. Degradation is observable via `Metrics` (queue saturation
  events, protocol errors, ping success) and `Hooks.OnQueueDepth` sampling.

## Reconnect policy

- One authoritative owner: the `run` goroutine. Concurrent death reports
  collapse through the per-connection `sync.Once`/`errCh` — reconnect storms
  and duplicate loops are structurally impossible.
- Classification-driven: temporary/timeout/closed → reconnect with
  exponential backoff (base 1 s, ×2, cap 30 s, ±30 % jitter by default —
  mirroring the reference's 1 s fixed delay but production-safe).
  Protocol violations → stop. Cancellation → stop immediately.
- Budget: `MaxReconnects` (default 5, the reference's `connection_retries`;
  negative = unlimited). Each *successful* connection resets the budget.
- Forced recycle: `MaxConnectionAge` (default 1800 s, mirroring the
  reference) proactively replaces a healthy connection; the manager performs
  it inline — no second loop.

## Cancellation propagation

```
Close()/terminal error
  → state = Closed (Send/Connect reject immediately)
  → t.ctx cancelled
      → run(): backoff sleeps abort; dial contexts abort
      → serveConn(): cancels connCtx
          → writer: select on connCtx → exit
          → heartbeat: ticker select → exit
          → conn.Close(): unblocks a reader parked in ReadMessage
          → reader: read error or connCtx → exit
      → runWG.Wait() → all per-conn goroutines joined
  → done closed → blocked Recv/Send unblock (ErrClosed / ctx.Err)
```

Cancellation during I/O (write in flight, read parked, backoff sleeping,
queue blocked) is exercised by dedicated failure-injection tests; every path
returns within milliseconds, not deadlines.

## Backpressure summary

| Direction | Mechanism | Bound |
|---|---|---|
| Outbound, slow network | writer drains queue; kernel socket + write deadline stall the writer; queue fills; `Send` blocks (or rejects) | `OutboundQueueSize` + 1 in-flight frame |
| Inbound, slow consumer | reader blocks pushing to `inbound`; TCP window backpressures the server | `InboundQueueSize` + 1 in-flight message |
| Hostile input | every length validated before allocation (frame size, message size, packet size, counts) | `MaxPacketSize`, `MaxMessageSize` |

No queue grows without bound anywhere in the pipeline; `go test -race ./...`
and the goroutine-leak tests in `tests/transport` enforce the model
continuously.


## Request engine (Phase 4) additions

| Goroutine | Count | Ownership |
|---|---|---|
| `recvLoop` (dispatcher) | 1 per engine | Sole owner of inbound correlation: decrypt, payload traversal, registry finalization from the wire, ack batching, replay execution. Exits on engine ctx or transport closure. |

Caller goroutines run `Invoke` — they touch only their own pending entry
and the registry's short critical sections. There is still **no
goroutine-per-request**: pending requests wait on a `done` channel closed
exactly once by the terminal winner (CAS in `pending.finalize`).

Channel/lock additions:

| Item | Kind | Policy |
|---|---|---|
| `Updates()` | bounded chan (1024) | drop-new on overflow, counted — the dispatcher never blocks on update consumers |
| `replaySignal` | chan struct{}, buffered 1 | non-blocking stage signal; replay runs on the dispatcher |
| `registry.mu` | RWMutex | O(1) critical sections only; snapshot/failAll swap the map wholesale |
| `idAssigner.mu` | Mutex | msg_id+seq_no assigned as one atomic pair |

Shutdown ordering (engine): reject new Invokes → cancel dispatcher ctx →
`failAll(ErrEngineClosed)` finalizes every pending → dispatcher exits →
`done` closes → blocked Invokes return. Transport state callbacks never
block: `notify` runs them outside the state lock (the engine's
Reconnecting callback stages replay; the dispatcher executes it), which is
what keeps the lifecycle goroutine immune to backpressure.


## Sessions & authentication (Phase 5)

- `session.Session` is fully mutex-guarded (RWMutex); all accessors are
  concurrency-safe, and the file adapter serializes writes through its own
  lock plus atomic rename.
- Concurrent Save/Load on one path is safe: readers retry through the
  transient Windows rename window (bounded 20 ms); POSIX is race-free by
  rename atomicity. Verified at 100 concurrent readers+writers under
  `-race` (TestFileSessionConcurrentSaveLoad, 3+ consecutive passes).
- The auth exchange is single-threaded by construction (a caller goroutine
  drives 3 synchronous steps); no shared mutable state beyond the sender.
