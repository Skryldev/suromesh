# Sessions & Authentication (Phase 5)

The `session` and `auth` packages implement SPlusthon-compatible session
persistence and the MTProto authorization-key exchange. The reference
implementation (sessions/*, network/authenticator.py, crypto/rsa.py) is the
behavioral oracle; compatibility is pinned by Python-extracted vectors.

## Architecture

```
Session (interface-ish state container, session/session.go)
├── identity / metadata      dc_id, addr, port   (long-lived)
├── auth key                 256-byte key + derived aux_hash/key_id
├── authorization state      NoKey → KeyExchanged → Authorized|Unauthorized
└── persistence adapters
    ├── FileSession          versioned JSON + HMAC, atomic rename
    └── StringSession        SPlusthon wire-compatible compact form
```

Deliberate separation: **long-lived authorization state** (key, DC) is
persisted; **transient transport state** (session ID, seq_no counters,
time offset) lives in the rpc engine and is never serialized. A TCP/WebSocket
reconnect rotates the transport session ID (reference `state.reset()`) but
never regenerates the auth key.

## Session state model

```
NoKey ──SetAuthKey──▶ KeyExchanged ──SetAuthorized──▶ Authorized
   ▲                      │                              │
   └────ClearAuthKey──────┴──────SetUnauthorized─────────┘
        (DC migration / server key loss / 401-class errors)

Illegal transitions are rejected with ErrInvalidState:
  - SetAuthorized without a key
  - key rotation while Authorized
```

On reconnect: `KeyExchanged`/`Authorized` sessions are reused as-is — the
key survives; only transport-level identifiers rotate.

## Persistent file format (v1)

```
"SUROMESH"                        8-byte magic
u32le  jsonLen
json   {"v":1,"dc":…,"addr":…,"port":…,"auth":hex|null,"state":…}
16B    HMAC-SHA256(magic ‖ jsonLen ‖ json; key=magic)   — integrity
```

- **Deterministic**: identical state → identical bytes (asserted by test).
- **Corruption-detected**: magic, length bounds, JSON validity, state
  whitelist, key-length check, and the HMAC tag. Any violation →
  `ErrCorrupt`; the library never adopts partial state.
- **Atomic**: `CreateTemp(dir)` → chmod 0600 → write → fsync → rename.
  A crash mid-write leaves the previous valid file; readers never observe
  half-written data.
- **Windows robustness**: rename-over-open-file and read-during-rename
  produce transient sharing violations; both sides retry (bounded, ~20 ms
  worst case). POSIX renames atomically on the first attempt.
- **Permissions**: 0600 on the temp file before rename (Unix). Windows
  ignores Unix modes — ACL tightening is a deployment concern, documented
  as a platform limitation.

### Locking decision

**No advisory file locking.** Rename-based atomicity guarantees readers see
old-or-new complete state; last-writer-wins is the intended multi-process
semantic for a per-process session. An advisory lock would add stale-lock
failure modes after crashes without protecting anything rename does not.
Consequence (documented): two processes sharing one file will overwrite
each other's key rotations — run one writer per session file.

## StringSession compatibility

Format v1 (reference sessions/string.py): `'1' + urlsafe_b64(payload)` where
payload = `dc_id:u8 ‖ addr_len:u16be ‖ addr ‖ port:u16be ‖ key:256`.

Compatibility verified against Python-extracted vectors
(`tests/vectors/string_session_malformed.json`):

| Case | Reference behavior | Go behavior |
|---|---|---|
| valid domain/IPv4 | parse | identical fields |
| empty string `""` | fresh empty session | identical |
| zero key (all-zero 256 bytes) | parse, key=None | parse, HasAuthKey=false |
| keyless serialize | `save()` returns `''` | `String()` returns `""` |
| bad version `2…` | error | `ErrNotAStringSession` |
| truncated payload | struct.error | `ErrTruncated` |
| invalid base64 | binascii.Error | `ErrNotAStringSession` |
| addr_len overflow | struct.error | `ErrTruncated` |
| round trip | save↔parse stable | byte-identical |

Migration: a SPlusthon-generated StringSession imports directly
(`ParseStringSession`), and a Go session exports to a string SPlusthon can
read (same layout, same base64 alphabet) — both directions covered by
`TestFullAuthFlowE2E` and the compatibility vectors.

## Auth key lifecycle

```
NoKey ──connect──▶ Key Exchange (3 plain steps) ──▶ KeyExchanged ──login──▶ Authorized
                      │
                      ├─ req_pq_multi → resPQ (nonce check)
                      ├─ factorize pq → RSA-encrypted p_q_inner_data
                      │    (fingerprint match against embedded keys)
                      ├─ req_DH_params → server_DH_params_ok
                      │    (temp AES key from nonces; IGE decrypt;
                      │     g/g_a/g_b range + 2^1984 safety checks)
                      └─ set_client_DH_params → dh_gen_ok
                           (new_nonce_hash verification)
```

- The exchange runs over the **plain** (unencrypted) framing —
  `mtproto.PlainMessage` through the Phase 3 transport.
- RSA: 20 embedded Soroush server keys (`crypto/rsakeys`), fingerprints =
  last 8 bytes LE of `SHA1(TL(n)‖TL(e))`; block = `SHA1(data) ‖ data ‖
  random(235−len)` raised to e — no PKCS#1 padding (protocol-required).
- Time offset from `server_DH_inner_data.server_time`; monotonic msg-id
  guard covers clock jumps.
- Reconnect with an existing key skips the exchange entirely (E2E-asserted
  via the fake server's exchange counter).

## Fake-server integration (tests/auth)

`TestFullAuthFlowE2E` runs the real transport stack: WebSocket →
obfuscated2 → abridged → plain 3-step exchange with an independently
computed server side (own RSA keypair, own DH math) → key derived →
persisted → reloaded → reconnected without re-exchange → migrated through
a StringSession into a fresh FileSession. The client's integrity checks
(nonce equality, DH ranges, nonce hash) use independent math from the
server's, so agreement is evidence, not tautology.

## Real-server validation

**Not performed.** No test credentials for a Soroush Plus environment were
available. Everything above is validated against the Python reference
implementation as the oracle (vectors + E2E against reference-mirroring
fakes). Real-network validation remains open work for whoever holds test
account credentials; nothing in this phase fabricates it.

## Security review (summary; full detail in docs/security.md)

- Secrets (auth keys) live in memory only where required; `Session.Close`
  zeroes the key bytes (best-effort — Go cannot guarantee erasure of all
  copies; documented).
- No logging path emits key material: `Session.Redacted` exposes only the
  derived (non-secret) key ID; enforced by tests that scan every error and
  rendering for key-byte patterns.
- Corruption never poisons: all decoders are loud (`ErrCorrupt`,
  `ErrNotAStringSession`, `ErrTruncated`); fuzzers assert no panics.
- Production randomness only on crypto paths (`crypto/rand` via the
  Config defaults); deterministic sources exist for tests exclusively and
  are never defaults.

## Phase 9 auth-state integration addendum

The Phase 5 authorization lifecycle (NoKey → KeyExchanged →
Authorized | Unauthorized) is now driven by the auth flows:
`SignIn`/`CheckPassword` success → `SetAuthorized` + save; permanent 401s
→ `SetUnauthorized`; `LogOut` → `ClearAuthKey` + save (file ends
keyless). The session file format is unchanged (v1, HMAC-integrity,
atomic rename): `state: "authorized"` requires a key on load — logout
afterwards reloads as a fresh, keyless session. Concurrency stress
(`TestAuthSessionSaveLoadStress`) hammers SetAuthorized/SetUnauthorized/
Save from 16 goroutines and requires a clean, consistent reload.
