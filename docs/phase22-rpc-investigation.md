# Phase 22 — First Encrypted RPC Investigation

Investigation of the Phase 21 INCONCLUSIVE observation: the real service
completed the DH key exchange, then the first encrypted request
(`invokeWithLayer(182, initConnection(..., getConfig))`) ended with
`rpc: connection lost`. Offline-only phase: no live contact, no OTP, no
code change without a proven defect.

## Baseline status (§2)

Git: 339 files staged, clean tree; module `github.com/Skryldev/suromesh`;
`tl/schema` cmp-identical to the committed provenance snapshots;
generator `-verify` current (47 files / 1,684 ctors / LAYER 182);
`reference/SPlusthon` 0 modified files; build/vet/gofmt clean; suite
17/17; race ×3 clean; external consumer green. No owner changes
overwritten (nothing was committed; the index is unchanged from Phase 21
plus this phase's additions).

## Traced request path (§3)

Field-by-field trace of the first encrypted RPC through the actual
implementation, with the authoritative sites, the reference rule, and
coverage:

| Field | Implementation | Reference rule | Coverage / differential |
|---|---|---|---|
| Auth-key ownership | `client.Config.Session` → `rpc.Config.AuthKey` → `mtproto.EncryptMessageData` writes `authKey.KeyID` | key_id = SHA1(key)[12:20] LE | `tests/vectors/ige`, `msg_key`, `envelope`; **new: `first_rpc`** |
| Server salt | `engine.salt` starts **0** after exchange; server corrects via `new_session_created`/`bad_server_salt` (`rpc/engine.go:134`, handlers in `rpc/dispatch.go`) | `mtprotostate.py:66 self.salt = 0`; TODO-check comment confirms server-side correction | `TestInvokeBadSaltTriggersResend`; **new: salt=0 vector** |
| Session ID | random per connection (`mtproto.NewSessionID`), reset on reconnect with salt reset (`rpc/engine.go:133,154`) | `reset: struct.unpack('q', os.urandom(8))` | engine lifecycle tests |
| msg_id | `(unix<<32)|(nanos<<2)`, monotonic, last+4 guard (`mtproto/state.go:184`) | `mtprotostate._get_new_msg_id` (identical formula incl. offset guard) | `tests/vectors/envelope` (fixed clock); **new: `first_rpc`** |
| seq_no | first content-related = **1** (odd), counter++ (`mtproto/state.go:196-206`; `engine.ids.assign(true)`) | `_get_seq_no(content_related)` — identical | `TestInvokeSuccessE2E` + **new: seq=1 in vector** |
| Envelope payload | `salt|session_id|msg_id|seq_no|len|body` (`engine.frameMessage`) | `encrypt_message_data: struct.pack('<qq', salt, id) + data` | byte-differential in **new test** |
| Length/padding | `12 + (-(n+12)%16+16)%16` random pad (`EncryptMessageData:93`) | `os.urandom(-(len(data)+12)%16 + 12)` — identical | `tests/vectors/envelope` padding replay |
| msg_key | `CalcMsgKey` = SHA256(key[88:120]‖plain)[8:24] | identical (x=0 client) | `msg_key` vectors + **new integrity check** |
| Encryption | IGE with `CalcAESKeyIV(x=0)` | identical | `ige` vectors (executed reference) |
| Transport framing | envelope → transport.Send (obfuscated2+abridged) | aiohttp WS + same codecs | `obfuscated` vectors; loopback E2E suite |
| Write/close handling | send failure → deterministic cancel (no mutation replay); connection loss → `failAll(ErrConnectionLost)` unless policy | sender semantics | §5 matrix below |
| Response parse/dispatch | `DecryptMessageData` (x=8), header check, `rpc_result` correlation | `decrypt_message_data` (identical checks) | `server_response` vector + engine E2E |

## Invariants checked (§4) — with new evidence

1. **NEW differential vector `tests/vectors/first_rpc.json`** — generated
   by EXECUTING the Python reference (same fixed clock/seed discipline as
   all fixtures; `tools/vectors/generate.py v_first_rpc`): the exact
   first-RPC shape — salt=0, first content-related seq_no=1,
   `invokeWithLayer(182, initConnection(api_id=1030400, "PC 64bit",
   "go", "suromesh", "en", "en", "", help.getConfig))`.
2. **NEW test `TestFirstEncryptedRPC`** (tests/compatibility):
   - payload decomposes exactly as salt|session|msg_id|seq|len|body;
   - body bytes equal the reference's request serialization;
   - ctor identity is invokeWithLayer;
   - **Go's `EncryptMessageData` reproduces the reference envelope
     BYTE-FOR-BYTE** for this exact shape (given the reference padding);
   - client-side (x=0) open round-trips the payload;
   - msg_key integrity: recomputation matches genuine plaintext and
     rejects any single-byte tamper;
   - Go's generated encoder emits correct ctor framing for the same
     request family.
3. Full vector regeneration (all 20+ fixtures incl. the new one)
   reproduced every pre-existing file **byte-identically** — reference
   environment intact, no drift.
4. Existing invariants re-run green: envelope/ige/msg_key/obfuscated
   differentials, tamper rejection (server-shape), fuzz seeds.

No live credentials, no Phase 21 key material, no plaintext payloads in
any fixture (the vector's "payload" is synthetic test data produced by
the seeded reference run).

## Connection-loss path (§5) — injection matrix (existing coverage)

| Injection point | Test | Verified behavior |
|---|---|---|
| Before request write (queue reject/backpressure) | `TestOutboundQueueRejectPolicy`, `TestQueueSaturationSurfacesToInvoke` | deterministic failure, no wire bytes, counted cancelled |
| During send (ctx cancel mid-write path) | `TestContextCancellationDuringSend` | caller cancellation surfaces, no partial-send zombie |
| After write, before response | `TestConnectionLossFailsPending` | pending fails with `ErrConnectionLost`, cause preserved, NO replay (no unsafe mutation duplicate) |
| During response read (garbage/truncated) | `TestMalformedResponseDoesNotKillEngine`, `TestPartialWriteFromServer` | classified (EnvelopeErrors), engine stays healthy |
| During encrypted-envelope parsing | malformed-envelope fuzz + `TestInvokeLateResponseAfterTimeout` | decode failures contained, unknown responses counted |
| Reconnect policy | `TestRetryOnReconnectReplays`, `TestServerDisconnectTriggersReconnect`, `TestReconnectExhaustion` | documented replay policy, bounded attempts |
| Shutdown determinism | `TestShutdownDuringRead`, `TestShutdownDuringReconnectBackoff`, `TestShutdownWithThousandsPending` | drain, terminal states |
| Goroutine leaks | client runtime leak suite (100 cycles) | return to baseline |

No gaps found; no new injection tests required. Retry policy unchanged.

## Evidence and limitations

- **Evidence**: the SDK's construction of the exact failing message is
  byte-identical to the executed reference across every layer
  (serialization → framing → key derivation → encryption → transport
  framing differentials). A live server that accepts the reference
  client's first RPC receives, from SuroMesh, bytes that are
  indistinguishable at every layer we can control.
- **Limitations**: (a) the drop itself was a single, unrepeatable event —
  the endpoint subsequently throttled all handshakes (Phase 21), so no
  live cross-check was possible in this phase by design; (b) bytes we
  cannot fully control offline: WebSocket/TLS-level timing, the
  obfuscated2 header randomness (covered by vectors but re-randomized
  per connection), and server-side admission policy; (c) gzip: our
  first RPC body is small, so gzip_if_smaller is inactive (vectors
  disable it for byte-stability; the roundtrip fixture covers decode).
- No defect was found; **no production code was changed** (fix policy §7
  never triggered). Additions: one vector generator function, one
  fixture JSON, one differential test — all test-infrastructure.

## Final classification (§6)

```text
TRANSPORT_OR_ENVIRONMENT_FAILURE
```

rationale: the message construction is proven reference-identical
(excluding an SDK envelope defect), and the surrounding live evidence
(39 s handshake latency, then universal handshake EOFs after repeated
attempts — server-side throttling) points at the connection layer/service
admission. Honest caveat: the drop event itself remains unrepeatable, so
a residual unexplained component is acknowledged; if it reproduces on a
demonstrably healthy link, the Phase 22 revalidation runbook escalates
to capture-first diagnosis (see docs/phase22-live-revalidation.md).

## Exact code changes

- `tools/vectors/generate.py`: `+v_first_rpc()` (new vector function;
  all pre-existing vectors regenerate byte-identically).
- `tests/vectors/first_rpc.json`: new fixture (executed-reference
  output, seeded).
- `tests/compatibility/first_rpc_test.go`: new differential test
  (PASS).
- No production-file changes. No schema/generated/reference changes
  (generator `-verify` current; `git -C reference status` clean).

## Final release status

Release qualification remains **BLOCKED** pending the mandatory
real-service validation (Phase 18/21 gate). This investigation removes
the implementation-construction hypothesis for the observed drop and
arms the revalidation attempt with a byte-level baseline.
