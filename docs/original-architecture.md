# SPlusthon — Original Architecture Analysis

**Phase 1 deliverable** · Analysis target: [shayanheidari01/SPlusthon](https://github.com/shayanheidari01/SPlusthon) @ `cad97548` (v1.1.4)
**Purpose:** establish the exact protocol/behavioral ground truth that SuroMesh (Go reimplementation) must preserve, and identify what should be redesigned.

All findings below were verified by reading the actual source. File references are relative to the repository root.

---

## 1. Repository Overview

SPlusthon is a fork of **Telethon 1.x** adapted to the **Soroush Plus** messenger protocol (`splus.ir`). Telethon's architecture is preserved almost 1:1; the Soroush-specific deltas are concentrated in:

| Area | Soroush-specific behavior |
|---|---|
| TL schema | `splusthon_generator/data/api.tl` — Telegram **Layer 182** schema, forked and modified (Soroush constructor IDs differ from upstream Telegram; custom types/functions added). `LAYER = 182` (in `splusthon/tl/alltlobjects.py`). |
| Transport | Default transport is **`ConnectionWebSocket`** (`splusthon/network/connection/websocket.py`) — MTProto's *obfuscated-abridged* framing carried over binary WebSocket frames to `wss://{ip}:{port}/apiws`. |
| DC routing | **All DC IDs (1–8) map to one endpoint:** `im-server.splus.ir:443` (`telegrambaseclient.py:27`, `_get_dc()` at `:815`). There is no real multi-DC IP table; `dc_id` is effectively metadata carried in the session. |
| RSA keys | 20 embedded Soroush server RSA public keys (`splusthon/crypto/rsa.py:87-268`), PKCS#1 DER/PEM "BEGIN RSA PUBLIC KEY" format. |
| Custom TL | Soroush-only namespaces and types (see §2.3). |
| Defaults | `api_id=1030400`, `api_hash='6edb16cf88714a4e9a805e928c39c937'`, `app_version='3.9.2 A'`, `lang_code='fa'` (base client defaults). |

Package layout (Python packages = future Go ownership boundaries):

```
splusthon/
├── client/            # Facade: SoroushClient = mixin of 12 method-classes + base
├── network/           # MTProtoSender, MTProtoState, PlainSender, RequestState,
│   └── connection/    #   transports: tcp(full/abridged/intermediate/obfuscated/mtproxy/http) + websocket
├── crypto/            # AES-IGE, AES-CTR, RSA, AuthKey, factorization, CDN decrypter, libssl bridge
├── sessions/          # abstract / memory / sqlite / string sessions
├── tl/
│   ├── core/          # hand-written: TLMessage, MessageContainer, GzipPacked, RpcResult
│   ├── types/         # generated (~1688 constructors)
│   ├── functions/     # generated (RPC requests)
│   └── custom/        # hand-written convenience wrappers (Message 1250 LOC, Conversation, Dialog...)
├── events/            # event builders (NewMessage, MessageEdited, Raw, ChatAction, ...)
├── _updates/          # MessageBox (update gap/pts bookkeeping, 830 LOC), EntityCache
├── extensions/        # BinaryReader, markdown/html parsers, MessagePacker
├── errors/            # RPCError hierarchy + generated rpcerrorlist (534-entry errors.csv)
└── utils.py           # entity resolution, part-size tables, markdown helpers (1570 LOC)
splusthon_generator/   # TL code generator: .tl parser → Python source/docs/errors
```

Dependencies: `aiohttp` (WebSocket), `rsa` (RSA), `pyaes` (pure-Python AES fallback), optional `cryptg` (C AES-IGE) / `isal` (fast gzip), `python-socks`/`pysocks` (proxies), `hachoir` (media metadata, optional).

---

## 2. Protocol Architecture

### 2.1 Layer stack (bottom → top)

```
 WebSocket (binary frames, wss://im-server.splus.ir:443/apiws, Origin: https://web.splus.ir, subproto "binary")
  └─ Obfuscation layer (AES-256-CTR keystream over every frame byte)
      └─ Abridged packet codec (length-prefixed packets, multiple of 4 bytes)
          └─ MTProto 2.0 encrypted message envelope (auth_key_id + msg_key + AES-IGE payload)
              └─ MTProto service messages: msg_container / TLMessage(msg_id, seq_no, body)
                  └─ TL objects: rpc_result, gzip_packed, Updates, RpcError, ...
```

Everything above the WebSocket is byte-identical to Telegram MTProto 2.0. The wire format is *not* JSON/WebSocket-RPC — Soroush reuses Telegram's binary protocol verbatim.

### 2.2 Transport details (websocket.py)

- URL: `wss://{ip}:{port}/apiws` (port 443) or `ws://` otherwise. Required headers: `Origin: https://web.splus.ir`; WebSocket subprotocol `binary`; `max_msg_size = 16 MiB`; aiohttp-level `heartbeat = 30 s`.
- Socket tuning: `TCP_NODELAY`, `SO_KEEPALIVE`, 64 KiB SO_SNDBUF/SO_RCVBUF, `TCP_QUICKACK` (Linux).
- **Periodic self-reconnect every 1800 s** (`_reconnect_interval`), independent of health.
- The WS reader adapts aiohttp messages to a `readexactly(n)` deque-of-chunks reader (memoryview slicing; no copy in the fast path).
- The WS writer buffers into one bytearray and flushes as a **single binary frame** on `drain()`.
- A class-level bug history is annotated in-code: cached `aiohttp.ClientSession` is per-instance (fixed), new-session leak on connect failure (fixed), reconnect-loop task churn (fixed).

### 2.3 Obfuscated framing (`ObfuscatedConnection` + `WebSocketObfuscatedIO`)

Identical to Telegram "obfuscated2":

1. Generate 64 random bytes (rejecting first byte `0xEF`, keywords `PVrG`/`GET `/`POST`/`\xEE\xEE\xEE\xEE`, and zero bytes 4–8).
2. `encrypt_key = random[8:40]`, `encrypt_iv = random[40:56]` → AES-256-CTR encryptor.
3. `rev = random[55:7:-1]` → `decrypt_key = rev[0:32]`, `decrypt_iv = rev[32:48]` → decryptor.
4. Place codec tag (`\xEF\xEF\xEF\xEF` for abridged) at `random[56:60]`, then overwrite `random[56:64]` with `encryptor(random)[56:64]`. The full 64-byte header is sent **before** the first abridged packet (it is not part of any packet).

Note: the header itself is sent as the first WebSocket frame content; subsequent frames are pure keystream-encrypted abridged packets. CTR state is continuous across frames (no per-frame reset).

### 2.4 Abridged packet codec (`tcpabridged.py`)

- Tag: `0xEF` (inside obfuscation header when obfuscated).
- Encode: `len(data) >> 2` as one byte if `< 127`, else `0x7F` + 3-byte **little-endian** length. Then payload. Payload must be 4-byte multiple (MTProto envelope is padded to 16, so always holds).
- Decode: symmetric; length byte ≥ 127 → read 3 more bytes LE, then `length << 2` payload bytes.

### 2.5 MTProto 2.0 envelope (`mtprotostate.py`)

- Plaintext layout before encryption: `salt:int64_le | session_id:int64_le | message(s)`, padded with **12 + (−(len+12) mod 16)** random bytes (total length ≡ 0 mod 16, min padding 12).
- `msg_key = SHA256(auth_key[88:120] || plaintext)[8:24]`.
- AES key/IV: `sha256a = SHA256(msg_key || auth_key[0:36])`, `sha256b = SHA256(auth_key[40:76] || msg_key)`;
  `aes_key = sha256a[0:8] ‖ sha256b[8:24] ‖ sha256a[24:32]`, `aes_iv = sha256b[0:8] ‖ sha256a[8:24] ‖ sha256b[24:32]` (x=0 client / x=8 server).
- Wire: `auth_key_id:int64_le | msg_key:16 | AES-256-IGE(plaintext)`.
- Decryption verifies: key id match, session id match, msg id odd, freshness (±30 s / 300 s deltas, "too new/old" ignored, 10 consecutive ignores ⇒ `SecurityError`), msg_key = `SHA256(auth_key[96:128] || body)[8:24]`, replay window = 500-entry deque of recent remote ids.
- `msg_id` generation: `(unix_seconds << 32) | (nanos << 2)` with monotonic guard (`last + 4`); `time_offset` corrected on bad-msg 16/17 or on first bad-salt.
- `seq_no`: `2*seq (+1 if content-related)`; seqno errors 32/33 adjust `_sequence += 64 / -= 16` and resend.
- Gzip: content-related bodies > 512 bytes are optionally wrapped in `gzip_packed#3072cfa1` if smaller.
- Batch send: `MessagePacker` packs up to **100 messages / 1,044,448 bytes** (MAXIMUM_SIZE = 1044456 − 8) into `msg_container#73f1f8dc`.

### 2.6 Plain (unencrypted) protocol (`mtprotoplainsender.py`)

Used only for auth-key generation: `0:int64 | msg_id:int64 | len:int32 | body` — no salt, no session id, no encryption.

### 2.7 TL wire encoding rules (`tl/tlobject.py`, generated code, BinaryReader)

- All integers **little-endian**; constructor IDs are 4-byte LE u32 written after each object.
- `int` 32-bit LE signed, `long` 64-bit LE signed, `int128`/`int256` raw little-endian bytes, `double` IEEE-754 LE, `float` unused.
- `string`/`bytes`: 1-byte length if `< 254` else `0xFE` + 3-byte LE length; payload + zero padding to 4-byte boundary (padding = `(len+1)%4` short form, `len%4` long form). 0xFF long-form byte is not used by this codec.
- `Vector<T>`: `0x1CB5C415 | count:int32 | T...` (inline, untyped at decode; unknown members inside a vector are skipped via `TypeNotFoundError`).
- `Bool`: `boolTrue#997275b5` / `boolFalse#bc799737` as bare constructor IDs (no fields). `Null#56730bcc`, `True#3fedd339`.
- **Flags**: `flags:#` field of a constructor is a u32 bitmask; optional args `flags.N?type` are serialized in **declaration order** (with the flags word first, and a second `flags2` word when bit 32+ is needed, as in `user`, `channel`, `userFull`).
- Dates: unix seconds int32 LE; zero epoch = "never".
- Unknown constructor at decode ⇒ `TypeNotFoundError(constructor_id, remaining_bytes)`.
- `rpc_result#f35c6d01 req_msg_id:long result:Object` — parsed manually: peeks inner constructor; `rpc_error#2144ca19` → error object; `gzip_packed` → decompress; otherwise raw bytes kept as body (lazy decode).
- Layer negotiated via `invokeWithLayer#da9b0d0d { layer:int, query:initConnection#c1cd5ea9 { api_id, device_model, system_version, app_version, lang_pack:'', lang_code, system_lang_code, proxy?, params?, query:help.getConfig } }` sent as the very first content-related message on connect (`telegrambaseclient.py:601-613`). Parse failures of the config response are tolerated/logged.

### 2.8 Soroush-specific TL content (delta vs upstream Telegram 182)

Custom namespaces/types added (not in upstream Telegram):

- `thirdParty.message#d630e7f7`, `thirdParty.reply#214899ac`, `update.thirdParty#7d2bba1f`, functions `thirdParty.request#c8520739`, `thirdParty.tell#a7ebdf75`.
- `conference.*` call namespace (14 constructors + 11 functions, e.g. `conferenceCall#f36441d5`, `conference.createConferenceCall#350ba3bd`).
- Soroush profile extensions: `userTypeNotify#3eab4210`, `userTypeMxb#35168d6a im_code:string`, `genderFemale#d6a1f9ff`/`genderMale#5b79da8a`, `participantLocation#f449bf69`, `userFullCustom#2f5a5335`, `channelFullCustom#ab97c375` (+ `customInfo` fields on `users.userFull`/`messages.chatFull`), functions `account.updateGender#7374f514`, `account.updateUserLocation#1cf44003`.
- Payments: `messageMediaPayment#02dfaf58`, `inputMediaPayment#84da7526`, `messageActionProtectedMessage#b8257746`, `messageActionContactReturned#1e47f27a`.
- Misc: `users.savedMusic*`, `account.savedMusicIds*`, `contacts.getBirthdays`, `account.updateBirthday`, `account.updateBusinessWorkHours`, `account.setMainProfileTab`, `profileTab*`, `messages.toggleDialogFilterTags`.

Constructor IDs of many common types (`user#274db727`, `chat#41cbf256`, `channel#8e87ccd8`, `message#38116ee0`, `userTypeNormal#ec35854d`, …) differ from upstream Telegram layer 182 — **the SPlusthon `api.tl` in this repo is the only authoritative source**. Total: **1,688 registered constructors** (`alltlobjects.tlobjects`), of which 116 core objects are hand-written in `tl/core` + `mtproto.tl`.

Notable absence: `invokeWithoutUpdates` is **not in the schema**, yet `telegrambaseclient.py:605` and `users.py:65,68` reference `functions.InvokeWithoutUpdatesRequest` when `receive_updates=False` — that path would raise `AttributeError` at runtime. Treat "no-updates mode" as **not actually working** in the original; do not copy this bug, but note the behavioral uncertainty.

### 2.9 Authentication key exchange (`network/authenticator.py`)

Standard MTProto DH exchange over the plain sender:

1. `req_pq_multi#be7e8ef1(nonce:int128)` → `resPQ#05162463` (nonce verified).
2. Factorize `pq` (pollard rho, `crypto/factorization.py`); build `p_q_inner_data#83c95aec` (big-endian pq/p/q inside TL string wrappers); RSA-encrypt with each server key by fingerprint until one matches: `SHA1(data) ‖ data ‖ random(235−len(data))`, raw RSA modexp to a fixed 256-byte block (`crypto/rsa.py:encrypt`). `req_DH_params#d712e4be`.
3. `server_DH_params_ok#d0e8075c` → key/iv = `SHA1(new_nonce‖server_nonce)+…` (`helpers.generate_key_data_from_nonce`); decrypt `server_DH_inner_data#b5890dba` (skip 20-byte SHA1 prefix); validate g/g_a/g_b ∈ (1, dh_prime−1) and the 2^1984 safety range; `client_DH_inner_data#6643b654` (+SHA1 prefix) encrypted with IGE; `set_client_DH_params#f5045f1f`.
4. `dh_gen_ok#3bcbf734` → `auth_key = g_a^b mod dh_prime` (2048-bit, big-endian bytes); `AuthKey.aux_hash = SHA1(key)[0:8]`, `key_id = SHA1(key)[12:20]` (both little-endian u64). `time_offset = server_time − local_time`.

Only the *first* (non-old) key set is used in practice; `use_old=True` is a fallback path.

---

## 3. Data Flow

### 3.1 Request lifecycle

```
user code ── await client(request)
  │ users.py __call__: resolve entities (input peers), flood-wait pre-check
  │ retry loop (request_retries, default 5)
  ▼
MTProtoSender.send(request) → RequestState{serialized bytes, future}
  ▼
MessagePacker queue (deque) → batch into msg_container (≤100 msgs / ~1 MiB)
  │ assign msg_id + seq_no per inner message; container is content_related=False
  ▼
MTProtoState.encrypt_message_data (gzip-if-smaller, pad, IGE encrypt)
  ▼
Connection.send queue → abridged encode → obfuscate → WS binary frame (single flush)
  ▼
pending_state[msg_id] = state  (set in _send_loop before enqueue to connection)
```

### 3.2 Response lifecycle

```
WS frames → WSReader (readexactly) → de-obfuscate → abridged decode
  ▼ recv queue (batched drain)
MTProtoState.decrypt_message_data → TLMessage(msg_id, seq_no, obj)
  ▼ _process_message: ack set, handler jump-table by CONSTRUCTOR_ID
rpc_result → pop pending_state[req_msg_id] → decode result object
  │ error? → rpc_message_to_error → future.set_exception
  │ ok?    → future.set_result; store_own_updates() feeds self-resulting updates
updates   → updates_queue (unbounded asyncio.Queue) → _update_loop
service   (pong, bad_salt, bad_msg, new_session, msgs_ack, detailed_info, future_salts,
           destroy_*, msgs_state_req/resend) → handled internally, states re-queued on
           bad salt/seqno, sent messages resent
```

### 3.3 Update flow

`_update_loop` (client/updates.py:267) drains the updates queue in batches, feeds each `Updates*` object to **MessageBox** (`_updates/messagebox.py`, 830 LOC) which:

- tracks per-account `(pts,qts,date,seq)` and per-channel pts state,
- detects **gaps** (`GapError`) and synthesizes `updates.getDifference` / `updates.getChannelDifference` requests,
- `_preprocess_updates` injects an entity map into each update and caches entities (memory EntityCache + deferred session flush),
- produces a dispatch list; each update is either dispatched **sequentially** or as **one asyncio task per update** (`sequential_updates=False` default), tracked in `_event_handler_tasks`.

Dispatch (`_dispatch_update`): builds per-builder events (`EventBuilderDict`), runs conversation listeners, then for each registered `(builder, callback)` applies `builder.filter(event)` and awaits the callback inside the update task. Handlers are **concurrent across updates, sequential within one update's builder list**; handler exceptions are logged (one specific exception type is special-cased).

Keepalive (`_keepalive_loop`, 3 s cadence): sends MTProto `ping#7abe77ec` (random int64) when idle — sender treats an unanswered ping as a dead connection and triggers reconnect — plus periodic cleanup of exported senders, entity flush, and session save.

---

## 4. Connection & Session Lifecycle

### 4.1 Connection lifecycle

```
connect()
  ├─ _connect (sender): retry loop (connection_retries=5, delay 1 s)
  │    ├─ connection.connect(timeout=10 s)   # WS handshake + obf header + codec tag
  │    ├─ if no auth_key: do_authentication (plain sender, full DH exchange)
  │    └─ start _send_loop + _recv_loop tasks
  ├─ send invokeWithLayer(182, initConnection(help.getConfig))  — errors tolerated
  ├─ start _update_loop + _keepalive_loop
  └─ session.save()

Failure paths:
  recv timeout 90 s ─┐
  IOError / WS close ─┼→ _start_reconnect → disconnect → state.reset() (new session_id)
  decrypt errors    ─┘     → retry connect (auto_reconnect=True → retries)
  InvalidBufferError 404 → auth key unknown to server → clear key, regenerate
  InvalidBufferError 429 → transport-level flood → hard disconnect
  explicit disconnect() → cancel loops, fail/cancel all pending futures, close socket
```

### 4.2 Session model

| Session | Storage | Contents |
|---|---|---|
| `MemorySession` | RAM | dc_id/address/port, auth_key(+tmp), takeout_id, entity set `(id, hash, username, phone, name)`, per-entity update states, uploaded-file cache (md5,size → InputPhoto/InputDocument) |
| `SQLiteSession` | SQLite v8 | same, persisted; versioned schema with upgrade path; used by default when a path string is given |
| `StringSession` | compact string | `'1' + urlsafe_b64(payload)`; payload = old: `>B4sH256s` (dc_id, IPv4, port BE u16, 256-byte auth key, 352 bytes total) or new: `>BH<len>sH256s` (dc_id, addr_len BE u16, addr UTF-8, port, key). No entities/update-state. |

Self-user is stored in the entity table under id `0` with `access_hash = user_id` (a documented hack, `telegrambaseclient.py:705-715`).

DC migration: on `*_MIGRATE_*` RPC errors, `_switch_dc(new_dc)` updates session dc fields, **drops the auth key**, reconnects and re-authenticates. Exported senders (`auth.exportAuthorization` + `invokeWithLayer(initConnection(importAuthorization))`) are borrowed per-DC with reference counting and idle-disconnect after 60 s. CDN downloads clone a session per CDN dc (`upload.fileCdnRedirect` handled in downloads).

---

## 5. Concurrency Model (Python)

- Single asyncio event loop; **all** networking in two tasks per sender (`_send_loop`, `_recv_loop`), two tasks per client (`_update_loop`, `_keepalive_loop`), plus per-update dispatch tasks (non-sequential mode) and transient auth/migration tasks.
- Unbounded queues: `MessagePacker._deque`, `Connection._send_queue/_recv_queue` (asyncio.Queue, unbounded), `client._updates_queue` (unbounded). No backpressure anywhere; a slow consumer grows memory.
- Shared mutable state protected only by the GIL/event loop: `pending_state` dict, `_pending_ack` set, `flood_waited_requests`, MessageBox state. The WebSocket per-instance session cache and reconnect flags were past race-condition sites (annotated fixes in-code).
- Cancellation is task-cancel based (`helpers._cancel`), with disconnect shielding (issue #3942 pattern).

---

## 6. Error System

- `RPCError` base with generated subclasses from `errors.csv` (534 entries): message-pattern regexes map `error_message` (e.g. `FLOOD_WAIT_X`) to typed exceptions with a `capture` field (e.g. wait seconds).
- Transport/`SecurityError`/`TypeNotFoundError`/`BadMessageError`/`InvalidBufferError` (+ `.code` 404/429 semantics) are hand-written.
- Client retry policy (`users.py:_call`): retry on `ServerError`, `RpcCallFailError`, `RpcMcgetFailError`, `InterdcCall*Error`, `TimedOutError` (2 s sleep); flood-wait < `flood_sleep_threshold` (60 s) → sleep and retry, else raise; migrate errors → `_switch_dc`.

---

## 7. Media

- **Upload** (`uploads.py`, `utils.get_appropriated_part_size`): part size 128 KiB (≤100 MiB) / 256 KiB (≤750 MiB) / 512 KiB; big-file path (`upload.saveBigFilePart`) when `file_size > 10 MiB`; MD5 for small files; file_id = random int64; `inputFileBig#fa4f0bb5` has no md5. Files cached in session by (md5, size) to reuse `InputPhoto/InputDocument`. Photo/document/video/voice attribute assembly via `utils.get_attributes` (hachoir optional).
- **Download** (`downloads.py`): `MIN_CHUNK_SIZE = 4096`, `MAX_CHUNK_SIZE = 512 KiB`, chunk sizes must be multiples of 4 KiB; `upload.getFile{,CdnRedirect}`; CDN redirect → per-CDN client with AES-CTR file decryption (`crypto/cdndecrypter.py`); iterative downloaders (`RequestIter`) with sleep on `TimedOutError` (1 s); 16 MiB max per message enforced by transport anyway.
- Media is streamed in bounded chunks (no full-file buffering) — the design is already memory-friendly; the Go port should keep `io.Reader/io.Writer` semantics.

---

## 8. TL Code Generator (`splusthon_generator/`)

Pipeline: `data/api.tl` + `mtproto.tl` → `parsers/tlobject/*` (regex-based TL parser with `{t:Type}` generics, `flags.N?` optionals, namespace handling) → `generators/tlobject.py` (emits `_bytes()`/`from_reader()`/`to_dict()` per class into `tl/types/__init__.py`, `tl/functions/*.py`, `alltlobjects.py` with LAYER constant) + `generators/errors.py` (errors.csv → rpcerrorlist) + `generators/docs.py` (mkdocs HTML, uses methods.csv/friendly.csv).

Generated code characteristics (per-class, as seen in `User`):

- `_bytes()`: `b''.join((ctor, flags_word, ..., fields...))` — per-field inline struct.pack, vector prefix `b'\x15\xc4\xb5\x1c' + count`, `serialize_bytes` for str/bytes; **asserts** cross-field flag consistency (e.g. `bot`/`bot_info_version`).
- `from_reader(reader)`: sequential reads with flags; bit tests per optional field.
- `to_dict()` for pretty-printing; equality via dict comparison.

Determinism: stable, but Python output is line-oriented with `typing` imports; no wire-level metadata beyond constructor IDs.

---

## 9. Performance Assessment of the Original

Measured-by-construction facts (no benchmarks in repo — tests are minimal: 55 LOC total across 5 test files):

1. **Serialization is allocation-heavy**: every `_bytes()` builds a list of small `bytes` then joins; `from_reader` creates Python objects for every field. Deeply nested `Updates` produce hundreds of intermediate objects per message.
2. **Double serialization**: `RequestState.__init__` serializes once (`bytes(request)`), and `invokeAfterMsg` wrapping re-serializes the opaque bytes inside a new TL envelope.
3. **AES-IGE in pure Python** (`pyaes` fallback) is ~100× slower than native; cryptg/libssl optional. Go's `crypto/aes` + hand-rolled IGE will be native speed.
4. **Gzip per message** for content-related > 512 B — CPU/latency tradeoff decided at runtime by comparing sizes.
5. **Event loop batching** is present and decent (batch drain of recv queue, batch send), but everything funnels through **one** asyncio loop; crypto and TL work are on the networking critical path (encrypt happens inline in `_send_loop`).
6. **No backpressure** on any queue; memory is unbounded under burst.
7. **Reconnect reset** loses no requests (re-enqueued) but loses exact ordering across containers.
8. 90 s recv timeout + 30 s WS heartbeat + 30 min forced reconnect define the health-check cadence.

## 10. Python-Specific Limitations

- GIL serializes CPU work; no parallelism for crypto/serialization without processes.
- `struct`/`bytes` churn and interpreter overhead dominate hot paths (dominant cost: object churn, not syscalls).
- Async fan-out semantics (per-update tasks) depend on loop scheduling; ordering guarantees are weak.
- Static typing absent; protocol errors surface at runtime (`struct.error`, `TypeError`) rather than compile time.
- Distribution requires compiled wheels (`cryptg`) for decent throughput — packaging friction.

## 11. Components to Redesign in Go vs. Preserve

**Preserve exactly (wire-compatible):**
- TL schema (api.tl 182 + mtproto.tl core 116) incl. Soroush constructor IDs and flags2 handling.
- Obfuscated-abridged-over-WebSocket framing (header algorithm, CTR continuity, abridged lengths) and `wss://…/apiws` handshake requirements (Origin, subprotocol).
- MTProto 2.0 envelope: salt/session/msg_id/seq_no, IGE key derivation, padding, msg_key checks, replay/freshness windows.
- Auth-key exchange (embedded 20 RSA keys, fingerprints = last 8 bytes LE of `SHA1(tl_bytes(n) ‖ tl_bytes(e))`, raw-RSA `SHA1‖data‖pad` block).
- rpc_result/rpc_error handling, gzip_packed, msg_container limits (100 msgs / 1,044,448 B), bad-salt/bad-msg resends, ack behavior (incl. `auth.logOut` ack quirk), `new_session_created` salt adoption, msgs_state/resend replies.
- StringSession v1 byte layout (both 352-byte legacy and new format) — for cross-library session interchange.
- Client-side protocol policy numbers: recv timeout 90 s, WS heartbeat 30 s, ±30/300 s msg freshness, 500-id replay deque, ping cadence, flood/migrate retry semantics, upload/download part-size tables.

**Redesign for Go:**
- Replace mixins/ABC facade with a single `Client` struct + option pattern; keep method names close to Telethon where sensible (`SendMessage`, `GetMessages`, `IterMessages`, `Events`).
- Generated **Go** TL code: strongly-typed structs, direct encoding into caller-reusable buffers, no reflection, no `to_dict` in hot paths (stringers/debug only).
- Replace asyncio-future pending map with a `context.Context`-aware request registry (per-msg-id channel or `sync.Map`), with bounded pending count and per-request timeout.
- Explicit goroutine ownership: one reader goroutine, one writer goroutine per connection, bounded send queue with backpressure, update dispatcher with configurable concurrency + bounded buffer.
- Encryption off the critical path only if profiling justifies; in Go, IGE over `crypto/aes` is fast enough inline.
- `sync.Pool` for encode buffers if profiling shows benefit; zero-copy slicing for frame parsing.
- Structured errors (`errors.Is/As`, typed `RPCError{Code, Message}`), context propagation end-to-end, deterministic graceful shutdown, `go test -race` cleanliness.

**Do not copy these original defects:**
- `InvokeWithoutUpdatesRequest` referenced but absent from schema (no-updates mode is broken upstream) — decide explicitly whether to implement `invokeWithoutUpdates` support or drop the option.
- Unbounded queues (all of them).
- Class-level config cache (`_config` on the class) shared across instances — move to per-client state.
- WS forced reconnect every 1800 s without draining pending requests first (rely on sender resend) — the Go port should make this optional and drain-aware.
- Self-user stored as entity id `0` with `access_hash = user_id` (works, but document it; keep for StringSession/SQLite parity only where needed).

## 12. Compatibility Risks for the Go Port

1. **Schema provenance**: `api.tl` claims LAYER 182 but contains types from newer Telegram layers (conference namespace, savedMusic) plus Soroush customs. Risk: none for interop *with Soroush servers* as long as the exact file is used; risk for *diffing against upstream Telegram* — avoid automated schema updates from Telegram sources.
2. **Obfuscation + WS**: server may close connections missing `Origin: https://web.splus.ir` or subprotocol `binary`; must be replicated byte-for-byte.
3. **msg_key / padding checks**: any deviation (e.g. Go's `crypto/cipher` stream handling) will fail server-side validation — cover with Python-vector tests (serialize in Python, decrypt in Go and vice versa).
4. **RSA fingerprints**: fingerprint formula is Telegram-style but keys are Soroush's; a mismatch falls back to old keys only if `use_old` — Go must try all 20 keys and fail loudly.
5. **Auth flow strictness**: `dh_gen_ok` nonce-hash checks and the 2^1984 range checks must be kept (server rejects otherwise).
6. **Unknown constructors**: server may send types the schema lacks; mirror upstream behavior (skip inside vectors, `TypeNotFoundError` otherwise → logged + message skipped, connection survives).
7. **Time**: msg_id generation uses local clock + offset correction; Go must implement the same monotonic guarantee (`last_msg_id + 4`).
8. **StringSession interop**: the 256-byte auth key is fixed-length; Go sessions must round-trip Python-generated strings (and vice versa) — add cross-check fixtures.

## 13. Proposed Go Architecture (Phase 2 preview)

```
github.com/Skryldev/suromesh/
├── tl/            # hand-written core (envelope, container, gzip, rpc) + generated code
│   ├── schema/    # api.tl + mtproto.tl (embedded, versioned)
│   └── gen/       # code generator (go run ./cmd/tlgen) → tl/generated
├── transport/     # websocket dialer, obfuscation stream, abridged codec, framed conn
├── mtproto/       # state (msg_id/seq/salt), encrypt/decrypt, auth exchange, plain sender
├── client/        # Client, options, invoke loop (retries/flood/migrate), entities
├── session/       # memory + string (v1-compatible) + file/sqlite-optional storage
├── events/        # builder/filter/dispatcher with bounded worker pool
├── media/         # streaming upload/download, CDN, part-size policy
├── errors/        # sentinel + RPCError code mapping (from errors.csv)
├── observability/ # Logger/Hooks interfaces, atomic counters, optional metrics
└── internal/      # buffer pools, backoff, test helpers (fake server)
```

Key Go mechanisms: `context.Context` end-to-end; per-connection single-writer goroutine fed by a bounded channel; reader goroutine with a bounded update channel; request registry keyed by int64 msg_id with deadline; `math/big` only in DH/factorization (auth path), never on hot paths; `golang.org/x/crypto` avoided if stdlib suffices (AES-IGE is a thin XOR loop over `crypto/aes`; CTR from stdlib; SHA1/SHA256 from stdlib; RSA modexp via stdlib `crypto/rsa`/`math/big` — no custom crypto primitives, only custom *padding/framing* which is protocol-required).

Dependency policy: stdlib-first. The only candidate external dependency is a WebSocket implementation — but the framing is simple enough to implement over `net/http` hijack or raw TLS dial + RFC 6455 handshake in ~300 LOC, keeping zero third-party deps viable; decide after benchmarking both.

## 14. Test Vectors Needed (Phase 2 entry criteria)

1. Python `bytes(req)` for representative requests (sendMessage, getHistory, upload.saveFilePart, invokeWithLayer init chain) → assert Go byte equality.
2. Python-encrypted envelope (given auth_key, salt, session_id) → Go decrypts; reverse direction byte-equality.
3. Obfuscation header reproducibility test (fixed random → same keystream bytes).
4. StringSession round-trip fixtures (old 352-byte format + new format).
5. rpc_error → Go error mapping for top-50 codes from errors.csv.
6. Fake WS server (`internal/testsrv`) replaying captured frames for full-stack integration.

---

*End of Phase 1 analysis. Phase 2 (core TL + generator) starts from this document's §2, §8, §12, §14.*
