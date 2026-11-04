# Security Review

Scope: the SuroMesh library as of Phase 5. The threat model is a client
library talking to the Soroush Plus network over untrusted networks, with
session material at rest on local disk.

## 1. Cryptographic inventory

| Purpose | Primitive | Source | Notes |
|---|---|---|---|
| Envelope confidentiality | AES-256-IGE | stdlib `crypto/aes` + hand-rolled IGE chaining | IGE is protocol-required (not a general-purpose mode); pinned by Python vectors both directions |
| Envelope integrity | msg_key = SHA256(…)[8:24] | stdlib | MTProto 2.0 construction; server rejects mismatches, client verifies inbound |
| Temp key exchange | DH (RFC-style 2048-bit groups) | `math/big` modexp | safety-range checks (2^1984) from the reference, enforced |
| RSA block | raw modexp, SHA1-prefixed 255-byte block | `crypto/rsa`-free hand modexp in `rsakeys` | protocol-required shape (SHA1(data)‖data‖rand), NOT PKCS#1 — deliberate |
| Fingerprints | SHA1(TL(n)‖TL(e)) last 8 LE | stdlib | reference formula, all 20 keys vector-verified |
| File integrity | HMAC-SHA256 | stdlib | detects corruption; NOT a secrecy mechanism (fixed key) |
| Padding / nonces | `crypto/rand` | stdlib | every production path; deterministic sources are test-only and never defaults |

SHA1 appears only where the wire protocol mandates it (fingerprints, RSA
block prefix, DH inner-data prefix). No new protocol uses SHA1 for
authentication decisions beyond what MTProto itself specifies.

## 2. Secret handling

**Secrets**: auth keys (256 bytes), session IDs (per-connection), DH
private exponents (b), RSA test private keys (tests only).

Rules enforced:

- **Never logged.** `Session.Redacted` emits `key(id=…)` using the derived
  key ID (computable from public traffic, non-secret). Tests scan every
  error path and rendering for 32-byte hex runs of actual key material
  (`TestRedactedNeverContainsKeyMaterial`,
  `TestErrorMessagesNeverContainKeys`).
- **Never in errors.** Error strings carry classifications and nonces'
  *consequences*, never nonce/key bytes.
- **Zeroing.** `Session.Close` zeroes the key array. Honest caveat: Go
  cannot guarantee erasure — copies may exist in GC-managed duplicates,
  stack temporaries, or transport buffers. We avoid *introducing* copies:
  the auth key is stored once per session and referenced elsewhere.
- **Struct printing.** No session/auth type has a `String()`/`GoString()`
  that dumps fields; `Redacted` is the only rendering.

## 3. Input handling (hostile server / hostile files)

- **TL decoding**: every length bounds-checked before allocation; vector
  counts validated against remaining bytes; hostile lengths rejected
  (`ErrTooLarge`/`ErrInvalidLength`). Fuzzed (8.9M execs).
- **Abridged framing**: packet length validated against a configured max
  before allocation; fuzzed.
- **Containers**: bounded (100 messages / ~1 MiB, reference limits);
  nested depth capped at 4.
- **Gzip**: decompression bomb guard (16 MiB limit).
- **Session files**: magic + bounds + JSON + HMAC before any field is
  trusted; key length must be exactly 256; state must be whitelisted.
  Fuzzed (24k execs, plus 15.7M on the string form).
- **Auth exchange**: every server reply crosses nonce-equality checks,
  DH safety-range checks, and the final new_nonce_hash verification — a
  tampered or miscomputed reply is rejected (fault-injection tests cover
  each step).

## 4. Denial-of-service posture

- No attacker-controlled unbounded allocation on any decode path (all
  framings validate-then-allocate).
- Bounded queues everywhere (Phase 3); hostile floods backpressure, never
  balloon.
- Rho factorization bounded (2^22 steps) — a hostile pq burns bounded CPU.
- Transport read deadlines (90 s) + WS-level liveness prevent slow-peer
  resource pinning.

## 5. Known limitations

1. **Go memory erasure is best-effort.** Zeroing at Close covers the
   canonical copy; GC copies are unreachable but not wiped.
2. **SHA1** is protocol-mandated in three places; not a design choice.
3. **Windows ACLs**: the 0600 mode is a Unix concept; session files on
   Windows inherit directory ACLs. Deployment should restrict the data
   directory.
4. **No real-server validation yet** (no test credentials); reference-
   oracle coverage is complete, live-network validation remains open.
5. **Two processes sharing one session file** last-writer-wins (documented
   locking decision); no inter-process lock.
6. **The HMAC on session files detects corruption, not tampering** — an
   attacker with write access to the file can re-HMAC. At-rest secrecy/
   integrity against local attackers requires OS-level protection
   (encrypted home, DPAPI etc.), out of library scope.
7. **Replay of the very first exchange**: the client pins its own nonce
   and verifies echoes, and dh_gen carries the nonce hash — a MITM without
   the server's RSA private key cannot forge replies (only the server can).


## Phase 7 hardening addendum

- **Pending-request bound** (8192 default): protects against registry
  growth when the server stalls; over-limit invokes fail with
  `rpc.ErrPendingLimit` before any bytes are sent.
- **Secret-leak audit** re-run over client/auth/session/rpc production
  code: zero `fmt.Printf`/`log`/`slog` calls; zero direct key-byte
  references outside the crypto/session packages; hooks and errors carry
  derived identifiers only (key ID, constructor name, counts).
- **Subscriber panic isolation**: a panicking update handler is recovered
  per-delivery; the panic is reported via `OnSubscriberPanic` without the
  payload (which may contain user content).
- **Config validation**: negative workers/queue/pending/timeouts are
  rejected at `New` — never discovered mid-connect.
- **Envelope padding** (Phase 7 fix): the dispatcher no longer misreads
  IGE padding as messages (spurious protocol errors under variable-size
  payloads; found by the soak, fixed, regression-tested).

## Phase 8 media addendum

- **Integer overflow (found & fixed)**: user-supplied `PartSizeKB` was
  multiplied in int32; `4194304 × 1024` wraps to 0 and would pass a
  divisibility check, later panicking on `offset % 0` (download) — a
  self-inflicted DoS reachable from a single config value. Part-size
  resolution now runs in int64 inside `resolveUploadPartSize` /
  `resolveDownloadChunkSize` and is pinned by fuzz targets
  (`FuzzResolveUploadPartSize`, `FuzzResolveDownloadChunkSize`: *reject or
  satisfy every protocol invariant, for any int32 input*).
- **Part-count wrap (found & fixed)**: sizes whose part count exceeds the
  int32 wire field are rejected (`ErrFileTooLarge`) instead of wrapping to
  a fabricated smaller file.
- **Negative offset (found & fixed)**: `Download` with a negative
  `DownloadOptions.Offset` previously hit Go slice-bounds panic in the
  alignment math (`rest[bad:]` with negative `bad`); now rejected up front.
- **No fabrication under pressure**: retries replay via reader seek only
  (`ErrNonReplayableReader` otherwise); a short read mid-file fails the
  transfer rather than padding; MD5 exists solely where the protocol
  demands it (small-file uploads).
- **Panic isolation boundary**: progress callbacks are recovered per call
  without payload capture; transfer engines themselves do not recover —
  bugs fail loudly rather than corrupting transfer state.
- **Test-infrastructure honesty**: the fake transport's frame log (every
  received frame retained) was initially masquerading as a 64 MiB
  "leak" in the upload memory audit; retention is now switchable
  (`SoroushPeer.SetRetainFrames`) and the bounded-memory claims come from
  audits where only the client holds live state.

## Phase 9 authentication addendum

- **Auth secret surface**: phone numbers, confirmation codes,
  phone_code_hash, 2FA passwords, SRP material, and auth keys never
  enter the error, hook, metric, or log surfaces. Errors render sentinel
  text only (`authRPCError`); raw RPC details stay reachable via
  `errors.As` without sensitive payloads. Static scan of
  `client/auth.go` / `client/srp.go`: zero logging calls.
- **SRP parameter validation before password work** (reference
  password.py:147-153): a hostile/misconfigured server's p, g, B are
  rejected (`ErrSRPParams`) before any password material is hashed;
  the fuzz target `FuzzSRPParams` pins reject-or-bounded-output for
  arbitrary parameter sets.
- **Constant-time comparison** of the server-side proof verification
  (`subtle.ConstantTimeCompare` in the test double's verifier); the
  client never holds a verifier to compare against.
- **Password lifetime**: the password exists only inside
  `computeCheck`'s call frame. Go's garbage collector does not guarantee
  erasure of intermediate byte slices (documented limitation since
  Phase 5; no mitigation is claimed beyond frame-locality).
- **Post-logout safety**: logout clears entities (superset of the
  reference's self-user clear), resets the durable session state to
  keyless, persists the emptied file, and tears the transport down —
  auth operations then fail loudly (`ErrNotReady`) instead of riding an
  unbound key.
- **401 classification**: permanent revocations (SESSION_REVOKED,
  USER_DEACTIVATED, AUTH_KEY_DUPLICATED) mark the client
  AuthorizationRevoked; recoverable 401s (AUTH_KEY_UNREGISTERED,
  AUTH_KEY_INVALID) only drop the authorized flag — re-authentication
  stays possible.

## Phase 10 update-engine addendum

- **No fabrication**: channel difference requests require the channel's
  access hash from the entity cache; without it the entry is forgotten
  and rebuilt from the next server-provided update (reference
  messagebox.py:735-743) — an access hash is never invented.
- **Entity rotation only from the server**: update envelopes rotate
  stored access hashes exclusively with server-supplied values; min-
  entities without hashes are skipped, never guessed.
- **No payload leakage**: metrics are label-free aggregates; the gap and
  difference hooks carry fixed kind strings only. Update payloads (message
  text, peer ids) never reach errors, hooks, or metrics.
- **Hostile streams**: fuzz targets pin the state machine against
  arbitrary decoded updates and difference results (no panic, coherent
  terminal state, bounded gap bookkeeping).
- **Update-state persistence**: added fields to the session file are
  optional JSON under the existing HMAC envelope; V1 files load
  unchanged, and the concurrent-save snapshot rule (Phase 9 fix) applies
  to the new fields.

## Phase 11 conversation-layer addendum

- **Access-hash fabrication remains structurally impossible**: peer
  resolution either finds the hash in the cache/server response or fails
  with ErrAccessHashUnavailable; the reference's bot-only hash=0 fallback
  is deliberately not implemented (docs/peers.md). Fuzz target
  FuzzPeerResolution pins that channels never resolve without a cached
  hash.
- **Marked-ID round trips** are fuzz-pinned (+user / -chat / -100channel).
- **No identifiers in observability**: conversation metrics stay
  aggregate; iterators and resolvers put no usernames, peer IDs, message
  bodies, or hashes into errors beyond the structural
  ErrAccessHashUnavailable{Peer} (kind + raw ID — protocol-level
  addressing data required to be actionable, no secrets).
- **Bounded memory**: iterators are O(page); the conversation store is
  capacity-bounded with documented eviction; fuzz targets pin capacity
  and impossible-state freedom.

## Phase 12 mutation addendum

- **No-logging audit** re-run over the mutation layer: zero
  fmt.Print/log/slog in message_mutation.go, message_response.go,
  message_search.go; missing response mappings surface as nil entries
  (the reference logs them — the no-logging rule replaces the log with
  structural nils, documented deviation).
- **Search query text never enters metrics** (counters only) and never
  enters errors; errors carry RPC identity via errors.As.
- **No fabrication**: unmapped forward/edit results surface as nil; the
  short-form synthesis uses only server-provided fields plus the
  caller-known peer (documented, messages.py:930-944 parity).
- **ID validation**: ≤0 and beyond-int32 message IDs are rejected before
  any wire conversion (FuzzMessageIDs pins the boundary).
- **Bounded input**: delete/forward chunk at 100 IDs/request; hostile
  huge vectors create bounded requests, not unbounded allocations.

## Phase 13 media addendum

- **No media bytes in observability**: captions, filenames, MIME values,
  media IDs, access hashes, and file references never enter metrics
  (aggregate counters only) or hooks.
- **file_reference bytes never enter error strings** — failures use the
  structural ErrMediaLocation/ErrFileReferenceExpired sentinels; wire
  locations are never dumped.
- **No fabrication**: locations require the server's id/hash/reference;
  empty file references pass through verbatim (the server's choice) and
  missing data fails with ErrMediaLocation.
- **Hostile metadata**: fuzz targets pin the classifier, size selection,
  location resolution, stripped splicing and album grouping against
  arbitrary inputs (no panic, bounded output, deterministic).

## Phase 14 addendum

- **Entity/reply conversion** carries only structural data (offsets,
  kinds, server-provided ids) — message text, URLs from TextURL
  entities, and usernames never enter metrics or hooks.
- **Global search attribution** emits peer kinds/ids only inside
  MessageKey diagnostics; the search QUERY text never enters metrics
  or errors.
- **No fabrication**: reply resolution returns the server's message or
  ErrReplyUnavailable; unknown entities are preserved structurally
  (ctor id only) with the raw form reachable via the returned object.
- **Fuzz**: hostile entity ctors, reply headers, read-state maxima and
  attribution sequences pinned (no panic, coherent state).

## Phase 19 repository-wide secret/credential audit

Scope: all source, tests, fixtures, docs, examples, generated files, and
the live harness (reference/ excluded as third-party material; see
docs/licensing.md). Method: pattern scans (credential-shaped strings,
key material, phone-number shapes), filesystem scan for
session/env/pem/key artifacts, manual triage of every hit.

Findings — every hit classified (Phase 19 §6 vocabulary):

| Item | Where | Classification | Action |
|---|---|---|---|
| `api_id=1030400`, `api_hash=6edb16cf...` default pair | client/client.go defaults; docs/original-architecture.md | PROTOCOL CONSTANT — the upstream SPlusthon client's own built-in public defaults, mirrored for parity; not a user credential | KEEP; consumers are expected to supply their own (`Config.APIID/APIHash`) |
| `testPhone = "+34 (123) 123-1234"` and vector phone cases | client/auth_e2e_test.go; tests/vectors/auth.json | DETERMINISTIC TEST FIXTURE — parse_phone exercise; not a real number | KEEP |
| SRP/DH salts, gA/gB, verifier-shaped hex | tests/vectors/*.json (srp, dh_helpers, envelope) | DETERMINISTIC TEST FIXTURES — generated by executing the reference with a seeded PRNG (tools/vectors/generate.py) | KEEP |
| `im-server.splus.ir:443` | client defaults, docs | PUBLIC ENDPOINT — the service address, documented openly | KEEP |
| Test RSA PEM framing + in-memory keypair | auth/exchange_test.go | TEST FIXTURE — keypair generated at runtime; no key material on disk | KEEP |
| Live-harness credentials | tests/live | ENVIRONMENT-ONLY — fail-closed, redacted (STATIC-tested); none present in the tree | KEEP (mechanism) |
| `.session` / `.env` / `.pem` / `.key` / `*.jsonl` evidence files | filesystem scan | NONE EXIST in the tree | — |

No actual secrets, tokens, private keys, bearer tokens, cookies, or real
user identifiers were found in the repository. The audit is reproducible
via docs/release-process.md step 8.
