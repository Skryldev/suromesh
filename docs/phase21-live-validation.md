# Phase 21 — Live Validation Record

First real Soroush Plus contact in the project's history. All evidence
below is sanitized (no phone number, OTP, auth key, or payload content;
the key fingerprint is a non-invertible derived identifier approved by
the Phase 18 evidence schema).

## Timeline (UTC, 2026-10-09)

1. **Endpoint reachability** — `im-server.splus.ir` resolves
   (185.60.137.29); TLS answers; plain HTTPS returns 404 (expected for
   a WebSocket-only path).
2. **WebSocket upgrade probe** — a raw upgrade request to
   `/apiws` with the reference's headers (Origin
   `https://web.splus.ir`, subprotocol `binary`) completed:
   `HTTP/1.1 101 Switching Protocols`, server
   `akka-http/10.7.3`, in **39.3 s**. The service accepts our
   transport's handshake parameters. LIVE / PASS (handshake
   parameters).
3. **SDK connect (live harness, `connect` area)** — first run failed at
   30 s (`initialize: context deadline exceeded`). Diagnosis: the
   measured link needs ~40 s for the WS upgrade alone; the harness's
   30 s connect budget was unrealistic for this route. Test-harness
   fix (evidence-based, still bounded): connect budget raised to 180 s
   with the measurement documented in code.
4. **SDK connect, second run** — reached the key exchange and
   **completed it**: the diagnostic driver reported
   `auth-key established (fingerprint 32dd754e676568ab)`. This
   validates, against the REAL service: WebSocket transport,
   obfuscated2 framing, abridged framing, plain MTProto envelopes,
   req_pq_multi / req_DH_params / set_client_DH_params, RSA key
   handling, DH math, and auth-key derivation. **LIVE / PASS — DH key
   exchange.**
5. **First encrypted RPC** (`invokeWithLayer(182,
   initConnection(..., getConfig))`) — the connection was lost
   (`rpc: connection lost`); no server response observed. This is a
   single, unrepeated occurrence. Classification per Phase 21 §21:
   **INCONCLUSIVE** — possibly an SDK defect in the encrypted-envelope
   path, possibly the same link instability that later plagued the
   raw handshakes, possibly a server-side close. §22 forbids patching
   on this evidence; the differential work performed (salt, msg_id,
   seq_no, init-request shape — all byte-faithful ports of
   mtprotostate.py / telegrambaseclient.py) found no divergence to
   act on.
6. **Raw-layer diagnostic** — subsequent connection attempts could not
   re-establish the WebSocket: every upgrade now ends in
   `ws: handshake response: EOF` (server closes mid-handshake;
   ~20 attempts over ~10 minutes, including six-retry cycles).
   Interpretation: server-side throttling/cooldown of this client
   endpoint after the earlier repeated connect attempts. Per §7 (no
   intentional abuse / rate-limit triggering), **all network probing
   was stopped**. Classification: **ENVIRONMENT_FAILURE** (denial of
   the validation channel, not an SDK defect).

## OTP / credential usage

The authorized operator phone was NEVER transmitted: the flow never
reached SendCode (it requires an initialized session, which never came
up). **No SMS/OTP was requested or consumed.** The phone number exists
nowhere in the repository, evidence, or output (verified by scan).

## Area verdicts

| Area | Verdict | Note |
|---|---|---|
| Connect (transport+handshake) | **PARTIAL** | upgrade accepted once (39.3 s); unstable/throttled afterwards |
| DH key exchange | **PASS** | live auth-key establishment, fingerprint recorded |
| Initialize (first encrypted RPC) | **INCONCLUSIVE** | one occurrence; connection lost; unattributable |
| AUTH | **BLOCKED** | never started (requires initialized session; probing stopped per §7) |
| SESSION / PEERS / DIALOGS / HISTORY / UPDATES / MESSAGING / MUTATIONS / MEDIA / FILE REFERENCES / ALBUMS / RECONNECT / ERRORS / LONG-RUNNING | **BLOCKED** | depend on initialized, authorized session |

## Next-session procedure (for the operator)

1. Wait out the server-side cooldown (hours; or a different egress IP).
2. Re-run the Phase 18 runbook areas in order
   (`tests/live/README.md`); the harness's connect budget now matches
   the measured link.
3. If the first encrypted RPC drops again reproducibly on a healthy
   link, the INCONCLUSIVE observation graduates to a defect
   investigation with a capture-first plan (wire-level bytes of the
   server's close behavior) before any protocol-layer change.

No evidence JSONL was produced (the recorder is created per-suite; the
attempts above used the sanctioned sanitized drivers). Nothing
credential-shaped entered any artifact.
