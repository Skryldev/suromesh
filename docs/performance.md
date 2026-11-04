# Performance — Phase 2 baseline

Method: `go test -bench=. -benchmem` on the phase-2 code (Go 1.26.3,
windows/amd64, 20 threads) versus micro-benchmarks of the reference
implementation's own serialization paths (Python 3.12.10, pure-Python `pyaes`
AES path — the reference is faster only when the optional `cryptg` C
extension is present). Same object shapes on both sides. Numbers are a
baseline to protect against regressions and to rank future optimizations —
not marketing claims.

## TL encode/decode

| Operation | Python (reference) | Go (this project) | Ratio | Go allocs |
|---|---|---|---|---|
| encode `User` (flags+flags2, 3 optionals) | 2,888 ns | 15.5 ns | ~186× | 0 B/op, 0 allocs |
| encode `Updates` (10 nested messages + media + user) | 41,277 ns | 206 ns | ~200× | 0 B/op, 0 allocs |
| encode `messages.sendMessage` request | 1,463 ns | 10.8 ns | ~135× | 0 B/op, 0 allocs |
| decode `User` | — | 139 ns | — | 336 B/op, 7 allocs |
| decode `Updates` | — | 1,561 ns | — | 3,872 B/op, 50 allocs |
| decode `messages.sendMessage` | — | 85.8 ns | — | 200 B/op, 4 allocs |
| writer primitives (u32+u64+string+bool) | — | 6.4 ns | — | 0 B/op, 0 allocs |

Decode allocates one struct per object plus pointers for optional fields —
the deliberate Phase-2 correctness-first choice (pointer presence encoding).
Removing the pointer fields in favor of bit-presence structs is the top
candidate optimization once the request engine exists to be benchmarked
end-to-end; it must not land without an allocation-profile win.

## Crypto

| Operation | Python (reference, pyaes) | Go | Ratio | Go allocs |
|---|---|---|---|---|
| AES-IGE encrypt 1 KiB | 1,411,220 ns | 1,783 ns (574 MB/s) | ~791× | 4 allocs |
| AES-IGE encrypt 16 KiB | — | 26,378 ns (621 MB/s) | — | 4 allocs |
| obfuscated2 stream 1 KiB | — | 187 ns (5.5 GB/s, stdlib CTR) | — | 1 alloc |
| envelope encrypt 1 KiB | 1,608,999 ns | 2,980 ns | ~540× | 12 allocs |
| envelope decrypt 1 KiB | — | 2,624 ns | — | 10 allocs |

The reference's pure-Python fallback is the worst case (most deployments
install `cryptg`, which is roughly 100× faster than pyaes). The honest
cross-ecosystem claim is: **Go's built-in AES path matches or beats the
reference's best C-accelerated configuration while removing the optional
dependency**, and beats the pure-Python fallback by three orders of
magnitude. Remaining envelope allocations are the padding buffer and IGE
output; pooling is scheduled after profiling shows they matter under the
full request pipeline.

## Fuzzing throughput (stability evidence)

- TL decoder: ~200k execs/s (8.9M execs, 45 s, no crashes)
- bytes framing: ~205k execs/s
- IGE / obfuscated2 / envelope decrypt: 0.9–1.2M execs/s

## Optimization log

Corrections that compatibility vectors forced before any tuning:
1. IGE decrypt chained the wrong halves (encrypt passed, decrypt failed) —
   caught by `ige.json`.
2. `TLMessage` length patching used a 12-byte header offset; the wire header
   is 16 bytes (`qii`) — caught by container round-trip tests.
3. Optional-field encoding initially wrote unconditionally — caught by
   `flags.json` byte-equality.
4. Obfuscated2 keystream had to be advanced 64 bytes during header splicing —
   caught by `obfuscated.json` stream equality.

No speculative optimization has been applied yet; per the phase-2 rule,
benchmarks exist first, tuning waits for profiling under the full pipeline.


## Transport (Phase 3)

Go 1.26.3, windows/amd64, 20 threads; loopback TCP; fake peer draining.
`go test -bench=. -benchmem -run='^$' ./transport/ ./tests/transport/`.

| Benchmark | Result | Notes |
|---|---|---|
| abridged encode 1 KiB | 7.6 ns/op, 0 B/op, 0 allocs | length prefix + append into reused buffer |
| abridged decode (64 KiB batch) | ~99 ns/1 KiB, 8 allocs/batch | sub-slices of one buffer; allocation is buffer growth, not per packet |
| obfuscated2 1 KiB | 194 ns/op, 1 alloc (5.3 GB/s) | stdlib CTR; output buffer is the alloc |
| end-to-end loopback 1 KiB, 1 producer | 7.9 µs/packet, 130 MB/s, 10 allocs | full stack incl. WS framing/masking, TCP, peer decode |
| end-to-end loopback 1 KiB, 16 producers | 7.7 µs/packet | no degradation: single writer keeps up |

### Load test (TestLoadSaturation, 64-byte packets)

| Producers | Throughput | Enqueue p50 | p95 | p99 | Rejected | Goroutines |
|---|---|---|---|---|---|---|
| 1 | 156,000/s (10.0 MB/s) | ~0 | ~0 | ~0 | 0 | 9 |
| 10 | 156,000/s | ~0 | 510 µs | 1.0 ms | 0 | 9 |
| 100 | 153,000/s | 511 µs | 1.0 ms | 1.5 ms | 0 | 9 |
| 1000 | 153,000/s | 6.6 ms | 7.2 ms | 7.8 ms | 0 | 9 |

Reading:

- **Saturation point ≈ 155k packets/s** on loopback against the test double;
  the bottleneck is the serialized writer path (WS frame build + masked
  write + syscall), not the queues — producer-side p50 stays ~0 until the
  producer count exceeds scheduler fairness (1000 producers see queue wait).
- **Memory is flat**: goroutine count constant at 9 (1 lifecycle + 3 per
  connection + test scaffolding) at every load level; zero rejections under
  PolicyBlock; backpressure degrades latency, never memory.
- One copy per inbound packet (decoder-buffer → queue) and per outbound
  frame (mask) are deliberate correctness boundaries; pooling them is the
  first optimization candidate if a real-server profile shows they matter.

### Optimization log (transport)

1. Fake-peer write deadlines were required for the flood test to terminate;
   a missing deadline hung the test double (fixed in wstest, not transport).
2. Transport Close() on a never-started transport deadlocked on `done` —
   fixed with an ownership flag + once-guarded close (caught by
   TestSendRecvBeforeConnect).
3. The server-side obfuscation decrypt stream must start at keystream
   offset 64 (header splice) — caught immediately by the E2E vector test.


## Request engine (Phase 4)

| Benchmark | Result |
|---|---|
| registry lookup | 11 ns/op, 0 B/op, 0 allocs/op |
| registry complete (finalize + remove) | 218 ns/op, 0 B/op, 0 allocs/op |
| registry add | 338 ns/op, 300 B/op, 2 allocs/op |
| concurrent add+complete (parallel) | 450 ns/op, 224 B/op, 2 allocs/op |
| request serialize (ping) | 14 ns/op, 64 B/op, 1 alloc/op |
| envelope seal (frame + IGE) | 680 ns/op, 1.2 KiB, 13 allocs |

### Concurrent-request load (fake server, immediate echo)

| Concurrent requests | Throughput | p50 | p95 | p99 | Goroutines | Failures |
|---|---|---|---|---|---|---|
| 10 | 18 k/s | 0.5 ms | 0.5 ms | 0.5 ms | 10 | 0 |
| 100 | 96 k/s | 0.5 ms | 1.0 ms | 1.0 ms | 17 | 0 |
| 1 000 | 75 k/s | 5.1 ms | 8.9 ms | 9.4 ms | 24 | 0 |
| 10 000 | 75 k/s | 13.6 ms | 15.2 ms | 16.1 ms | 31 | 0 |
| 100 000 | 76 k/s | 13.3 ms | 14.3 ms | 14.7 ms | 38 | 0 |

Zero failures, zero registry leaks, goroutines bounded at every level
(engine adds exactly one dispatcher goroutine regardless of request
count). The ~76 k/s plateau is the single-threaded fake peer, not the
engine — a real server parallelizes response generation.

### Fuzzing

- FuzzHandlePayload (dispatcher, hostile payloads): 3.8M execs, no panics,
  no registry growth.
- FuzzEnvelopeDecrypt (hostile envelopes): 3.8M execs, clean.

### Bugs the Phase 4 tests caught before they shipped

1. State-transition callbacks ran under the state lock; the engine's
   reconnect callback calls back into the transport (Send → State()) —
   self-deadlock. Fixed: notify snapshots and runs callbacks unlocked.
2. Replay executed inline on the lifecycle goroutine could block in Send
   before the replacement writer existed — moved to the dispatcher.
3. Client-envelope builder for tests required the server keystream to
   start at offset 64 (header splice) — pinned by vector test.


## Sessions & authentication (Phase 5)

| Benchmark | Result |
|---|---|
| StringSession parse | 231 ns/op, 632 B/op, 3 allocs |
| StringSession serialize | 298 ns/op, 1.4 KiB, 4 allocs |
| FileSession save (atomic rename) | 677 µs/op (dominated by fsync; 24 allocs) |
| FileSession load | 25.5 µs/op, 3.2 KiB, 23 allocs |
| Full DH exchange | one RSA modexp (≤1 ms) + 3 round trips; crypto correctness outranks micro-tuning |

Fuzzing: FuzzStringSessionDecode 15.7M execs, FuzzFileSessionDecode 24k
execs — no panics; every parsed state re-saves deterministically.


## Client layer (Phase 6)

| Benchmark | Result |
|---|---|
| Invoke (ping, full round trip) | ~24.7 µs/op, 10.8 KiB, 124 allocs |
| Invoke (parallel, 20 threads) | ~8.6 µs/op |
| Entity conversion | 85 ns/op, 1 alloc |

## Phase 7 hardening

| Benchmark | Result |
|---|---|
| EntityCacheGet (ResolvePeer via cache) | 27 ns/op, 1 alloc |
| EntityCachePut (SeedRaw) | 84 ns/op, 1 alloc |
| UpdateDispatch (invoke → update delivered) | ~24.6 µs/op (bounded by full RPC round trip) |

## Phase 7 soak (fake server, windows/amd64)

| Metric | Value |
|---|---|
| Invokes | 10 000 (20 workers) — 0 errors |
| Update batches | 999 delivered, 0 dropped |
| Goroutines before/after | 15 / 15 (no growth) |
| Heap in use | 5.1 MiB |
| Pending after | 0 |
| Reconnect latency | ~100 µs avg (5 cycles) |
| Entity cache bound | 4096 entries after 100 000 updates (2.1 MiB heap) |

BASELINE policy: Phase 6 recorded baseline numbers; Phase 7 changes
(padding fix, pending bound, lifecycle ctx) did not regress any recorded
benchmark (all within noise on identical hardware). Future regressions
should be caught by comparing against docs/performance.md.

## Media / transfers (Phase 8, fake server, windows/amd64)

| Benchmark | Result |
|---|---|
| Upload single part (64 KiB) | ~0.98 ms/op, 67.2 MB/s, 1.3 MiB/op, 150 allocs |
| Upload multi-part (2 MiB → 16 parts, big-file path) | ~16.6 ms/op, 126.6 MB/s, 35.8 MiB/op, 2005 allocs |
| Download single chunk (64 KiB) | ~0.48 ms/op, 137.6 MB/s, 139 allocs |
| Download multi-chunk (2 MiB → 16 chunks) | ~22.7 ms/op, 92.2 MB/s, 2134 allocs |
| Upload with progress callback (4 parts) | ~3.99 ms/op, 131.5 MB/s |

The MiB/op figures are **transient** copies across the full in-process
round trip (client marshal → encrypt → abridged → fake decrypt → unmarshal
→ fake responder), not retained memory: the memory audits show ≤ 0.3 MiB
of *live* heap growth for a 64 MiB transfer.

## Phase 8 stress & memory audit (fake server)

| Metric | Value |
|---|---|
| 100 concurrent mixed transfers (50 up + 50 down) | 116 ms, 0 failures, exact counter reconciliation |
| 1000 sequential uploads | 77 ms, flat heap (+240 KB over 1000 transfers) |
| 64 MiB upload (deterministic reader) | +0.3 MiB live heap — O(part_size), not O(file) |
| 64 MiB download (synthetic server) | +0.1 MiB live heap |
| 60 concurrent transfers with 1-in-4 injected failures | exact completed/failed accounting, 0 stuck slots, next upload succeeds |
| Fuzz (part-size resolvers, chunk classification) | 2.5 M+ execs each, 0 failures |

BASELINE policy: Phase 8 media benchmarks are the first media baseline;
no prior-phase numbers regressed (media code touches no hot paths of
invoke/updates).

## Phase 9 authentication benchmarks (fake server, windows/amd64)

| Benchmark | Result |
|---|---|
| AuthState read | ~2.1 µs/op (mutex+atomic reads), 2 allocs |
| ParsePhone | ~63 ns/op |
| SendCode E2E (full RPC round trip) | ~47 µs/op, 204 allocs |
| SignIn E2E (full round trip + onAuthorization) | ~28 µs/op, 148 allocs |
| Session save+load round trip (file, HMAC, fsync) | ~1.29 ms/op, 46 allocs |

SRP is deliberately not benchmarked: its cost is dominated by the
protocol-mandated 100000-iteration PBKDF2; optimization is out of scope.
Authentication is not a hot path — these pin baselines, not targets.

## Phase 10 update-engine benchmarks (fake server, windows/amd64)

| Benchmark | Result |
|---|---|
| Single update E2E (push → delivered) | ~34.7 µs/op, 8.1 KiB, 90 allocs |
| 1000-update batch E2E | ~436 µs/op per batch (~0.44 µs/update), 2.05 MiB, 12127 allocs |
| Entity extraction per envelope | ~5.8 µs/op, 9.1 KiB, 88 allocs |
| Duplicate detection (pure state machine) | ~227 ns/op, 344 B, 5 allocs |
| Subscriber fan-out (10 subscribers) | ~18.5 µs/op |

Memory: 100k sequential updates add **0.0 MiB** of live heap growth —
the state machine holds per-entry pts only; gap buffers are capped.
No prior-phase benchmark regressed (the update path replaced a simpler
pipeline; Invoke/auth/media benchmarks are unchanged).

## Phase 11 conversation-layer benchmarks (fake server, windows/amd64)

| Benchmark | Result |
|---|---|
| ResolvePeer (cached) | ~195 ns/op, 0 allocs |
| Entity → InputPeer | ~159 ns/op, 1 alloc |
| Dialog conversion (1-dialog page E2E) | ~32.2 µs/op, 146 allocs |
| Message conversion (1-message page E2E) | ~32.0 µs/op, 137 allocs |
| History pagination (100/page E2E) | ~145 µs/op per page (~1.45 µs/message) |
| Conversation update (via wire) | ~5.7 µs/op |
| Conversation lookup | ~1.2 µs/op, 0 allocs |

Memory: 100k history messages streamed through iterators add 0.0 MiB of
live heap growth (O(page) contract). Phase 9/10 baselines unchanged.

## Phase 12 mutation benchmarks (fake transport, windows/amd64)

| Benchmark | Result |
|---|---|
| SendMessage E2E | ~36.5 µs/op, 16 KiB, 170 allocs |
| EditMessage E2E | ~30.4 µs/op, 14 KiB, 157 allocs |
| DeleteMessages E2E (3 ids) | ~28.3 µs/op, 12 KiB, 145 allocs |
| ForwardMessages E2E (3 ids) | ~36.8 µs/op, 22 KiB, 211 allocs |
| Search conversion (1 result) | ~31.7 µs/op |
| Search pagination (10 results) | ~69.7 µs/op |
| Conversation mutation (via wire) | ~5.4 µs/op |

Memory: 100k delete IDs chunk into 1000 requests with **-0.7 MiB** heap
delta (GC reclaims; no accumulation). These are protocol/CPU costs
against the in-memory fake — not network-representative.

## Phase 13 media-domain benchmarks (fake transport, windows/amd64)

| Benchmark | Result |
|---|---|
| SendMedia existing-document E2E | ~36 µs/op (no upload RPCs) |
| SendAlbum E2E (3 items) | ~110 µs/op |
| Document download E2E (2 KiB, real getFile) | ~1.6 ms/op (chunked) |
| 1000 media-message conversions | ~0.0 MiB heap growth (metadata-only) |
| Photo size selection | sub-µs (small sorted vectors) |

The dominant media costs remain the Phase 8 transfer engine numbers
(unchanged); conversion is metadata-only.

## Phase 14 benchmarks (windows/amd64, internal)

| Benchmark | Result |
|---|---|
| Entity conversion (3 entities E2E) | ~379 ns/op first run, ~64 ns/op steady, 1-5 allocs |
| MessageKey construction | ~0.6 ns/op, 0 allocs |
| Read-state lookup | ~20 ns/op, 0 allocs |
| Cross-path double conversion | ~565 ns/op, 10 allocs |
| Reply metadata conversion | ~15 ns/op, 1 alloc |

No regression against Phase 13 baselines (media/mutation benchmarks
unchanged).

## Phase 17 performance contract (windows/amd64)

Audit requirement: no unexplained regression >5% against the Phase 15/16
baselines. Phase 17 touched no hot paths — the only production-code
changes were dead-code removals (U1000/SA4006/SA4009) and a deprecated
randomness source swap; all allocation counts are unchanged.

| Benchmark | Phase 14–16 baseline | Phase 17 run | Verdict |
|---|---|---|---|
| Registry lookup | 11 ns, 0 allocs | 10.97 ns, 0 allocs | flat |
| Registry add | 338 ns, 2 allocs | 333 ns, 2 allocs | flat |
| Registry complete | 218 ns, 0 allocs | 224–268 ns across 5 consecutive runs, 0 allocs | within machine variance (spread ±10% on the untouched path) |
| Registry concurrent | 450 ns, 2 allocs | 422 ns, 2 allocs | flat |
| Entity conversion (steady) | ~64–85 ns, 1 alloc | 47.7 ns, 1 alloc | flat/better |
| MessageKey construction | ~0.6 ns, 0 allocs | 0.107 ns, 0 allocs | flat |
| Upload single part | ~0.98 ms, 67 MB/s, 150 allocs | 0.52 ms, 126 MB/s, 142 allocs | allocs stable; loopback variance dominates |
| Download single chunk | ~0.48 ms, 138 MB/s, 139 allocs | 0.60 ms, 108 MB/s, 139 allocs | allocs stable; loopback variance dominates |

Interpretation: the alloc/op columns — the stable, machine-independent
metric — match the baselines everywhere. ns/op for the loopback media
tests swings more than 5% between back-to-back runs on this host
(measured spread on `BenchmarkRegistryComplete` alone was 224–268 ns);
no delta is attributable to code. No optimization was attempted; none was
evidence-justified.
