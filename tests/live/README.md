# Live Validation Harness (Phase 18)

Opt-in, credential-gated validation against the **real Soroush Plus
service**. Without explicit operator activation these tests NEVER run —
`go test ./...` only executes the fake-wiring and sanitization suites in
this package (classified `FAKE` / `STATIC`).

## Current status

```text
BLOCKED — REAL CREDENTIALS REQUIRED
```

No real Soroush Plus credentials are available to this project. Every
live test below is executable the moment credentials are supplied (proven
executable by the fake-wiring suite) and reports `BLOCKED` until then.
Nothing in the production tree was modified to compensate for the missing
access.

## Operator runbook

1. **Authorize** (creates the session file, exactly one SMS):
   ```sh
   SP_SDK_LIVE=1 SP_LIVE_AREAS=auth,session \
   SP_LIVE_SESSION=/secure/path/live.session \
   SP_LIVE_PHONE=+15550001111 \
   SP_LIVE_CODE_FILE=/secure/path/code.txt \
   go test -run 'TestLiveAuth|TestLiveSessionReuse' ./tests/live/ -v
   ```
   (For 2FA accounts add `SP_LIVE_PASSWORD` / `SP_LIVE_PASSWORD_FILE`.)

2. **Account validation** (uses the authorized session — no re-auth):
   ```sh
   SP_SDK_LIVE=1 \
   SP_LIVE_AREAS=peers,dialogs,errors,reconnect \
   SP_LIVE_SESSION=/secure/path/live.session \
   SP_LIVE_PEER=some_test_peer \
   go test -run 'TestLivePeers|TestLiveDialogs|TestLiveErrorMatrix|TestLiveReconnect' ./tests/live/ -v
   ```

3. **Write operations** (each area is separately opt-in):
   ```sh
   SP_LIVE_AREAS=mutations,media   SP_LIVE_MUTATIONS=1 SP_LIVE_MEDIA=1 ...
   ```

4. **Operator-assisted update validation** (someone sends a message to the
   account from elsewhere during the window):
   ```sh
   SP_LIVE_AREAS=updates SP_LIVE_INCOMING_WAIT=120 ...
   ```

5. **Bounded soak** (1–360 minutes, hard-capped, paced at 30 s/cycle):
   ```sh
   SP_LIVE_AREAS=soak SP_LIVE_SOAK_MINUTES=60 ...
   ```

6. **Connect-only sanity** (no account needed):
   ```sh
   SP_LIVE_AREAS=connect ...
   ```

## Configuration

| Variable | Meaning |
|---|---|
| `SP_SDK_LIVE=1` | master switch (required, never sufficient alone) |
| `SP_LIVE_AREAS` | comma list: `connect,auth,session,peers,dialogs,mutations,updates,media,reconnect,errors,soak` |
| `SP_LIVE_HOST` / `SP_LIVE_PORT` / `SP_LIVE_TLS` | endpoint (default `im-server.splus.ir:443`, TLS on) |
| `SP_LIVE_SESSION` | session file path (account areas REQUIRE it) |
| `SP_LIVE_PHONE` / `SP_LIVE_PHONE_FILE` | phone (auth area only) |
| `SP_LIVE_CODE` / `SP_LIVE_CODE_FILE` | OTP (auth area only) |
| `SP_LIVE_PASSWORD` / `SP_LIVE_PASSWORD_FILE` | 2FA password (only if required) |
| `SP_LIVE_PEER` / `SP_LIVE_PEER_KIND` | test peer (username, or numeric id with kind `chat`/`channel`) |
| `SP_LIVE_MUTATIONS=1` | write operations (send/edit/delete/read/reply) |
| `SP_LIVE_MEDIA=1` | media upload/download ladder (1 B → 11 MiB) |
| `SP_LIVE_BAD_CODE=1` | exactly one invalid-code probe |
| `SP_LIVE_INCOMING_WAIT` | seconds to wait for an operator-sent incoming message |
| `SP_LIVE_SOAK_MINUTES` | soak duration (hard cap 360) |
| `SP_LIVE_EVIDENCE` | JSONL evidence file (redacted, 0600) |
| `SP_LIVE_KEEP=1` | keep artifacts (session file, evidence) — default is cleanup |

## Security rules (enforced by the harness, verified by STATIC tests)

- Credentials exist only in the environment (or a file named by the
  environment); they are never in source control, logs, or evidence.
- Every secret is registered with a redactor that scrubs all evidence
  records in memory AND on disk (`TestStaticRedactorRecorder`).
- Message text and media bytes are never recorded — only lengths and
  SHA-256 prefixes (`HashOf`).
- The auth key is reduced to a non-invertible FNV fingerprint for
  "same key across reconnect" evidence (`keyFingerprint`).
- No OTP is ever requested unless the operator supplied the code
  up-front (no SMS is wasted by a code-less SendCode).

## Classification

Every result record carries `LIVE|FAKE|REFERENCE|STATIC` and
`PASS|FAIL|BLOCKED|SKIPPED|NOT_OBSERVED|NOT_APPLICABLE` — the two
vocabularies are pinned by `TestStaticEvidenceClassification`. Fake-wiring
results are never live results; a live discrepancy without evidence is
never automatically a bug (§35).
