# Phase 18 — Live Soroush Plus Validation

## Executive Summary

Phase 18 asked: *does the Go SDK behave correctly against the actual
Soroush Plus service, not merely against our fake transport/server?*

**Credential status: UNAVAILABLE.** No real Soroush Plus credentials
exist for this project (unchanged since Phase 9; stated in every phase
report). Per Phase 18 §34, the phase therefore completed the
**Live-Readiness Track** and the release verdict is:

```text
RELEASE BLOCKED — LIVE CREDENTIALS REQUIRED
```

What was delivered (all `STATIC`/`FAKE`-validated, never live):

1. **A complete, fail-closed live harness** (`tests/live/`) with eleven
   activation areas (connect, auth, session, peers, dialogs, mutations,
   updates, media, reconnect, errors, soak), an explicit
   `SP_LIVE_AREAS` gate (nothing runs accidentally — verified: with
   `SP_SDK_LIVE=1` and no areas named, all 15 live tests skip), and
   per-area credential prerequisites (auth requires phone+code up-front
   so no SMS is ever wasted; account areas require an authorized session
   file).
2. **Sanitized evidence capture**: a redactor registered with every
   operator secret that scrubs records in memory AND on disk; SHA-256
   prefixes instead of content; FNV auth-key fingerprints instead of key
   material; method names/latencies/error classes only.
3. **Proof of executability** (§34.7): the exact shared validation
   operations (session reuse, own-update, media round trip, error
   matrix, dialogs/history, reconnect, incoming window) run green
   against the in-process fake over the real protocol stack
   (`TestFakeWiring*`, 8 tests) — when credentials arrive, the live
   suites exercise only the SERVICE, not untested harness code.
4. **The release qualification matrix** below, with every live cell
   resolved to BLOCKED.

## Credential Status

```text
UNAVAILABLE
```

No phone number, OTP, password, or account access is held by this
project. No real network authentication was attempted; no real endpoint
was dialed during this phase (the fail-closed verification used the
no-configuration path, where every test skips before any I/O).

## Live Test Results — Release Qualification Matrix (§32)

Legend — Reference: differential tests vs the executed Python reference
(Phases 1–17). Fake: deterministic fake-server suites (real protocol
stack). Live: THIS phase. Every `?` became BLOCKED.

| Area            | Reference | Fake | Live    | Status  | Evidence |
| --------------- | --------- | ---- | ------- | ------- | -------- |
| Auth            | PASS      | PASS | BLOCKED | BLOCKED | docs/auth.md; srp golden vectors (tests/vectors); tests/live/auth_test.go executable |
| Session reuse   | PASS      | PASS | BLOCKED | BLOCKED | docs/session.md; phase16 workflow A; tests/live/session_test.go executable |
| Peer resolution | PASS      | PASS | BLOCKED | BLOCKED | docs/peers.md; phase11/16 suites; tests/live/peers_test.go |
| Dialogs         | PASS      | PASS | BLOCKED | BLOCKED | docs/dialogs.md; dialogs fake suite; tests/live (dialogs area) |
| History         | PASS      | PASS | BLOCKED | BLOCKED | docs/history.md; digest/conversion suites; tests/live (dialogs area) |
| Messaging       | PASS      | PASS | BLOCKED | BLOCKED | docs/mutations.md; mutation suites; tests/live (mutations area) |
| Updates         | PASS      | PASS | BLOCKED | BLOCKED | docs/updates.md; MessageBox port + E2E suites; tests/live (updates area) |
| Reconnect       | PASS      | PASS | BLOCKED | BLOCKED | docs/reliability.md; reconnect suites; tests/live (reconnect area) |
| Media upload    | PASS      | PASS | BLOCKED | BLOCKED | docs/transfer.md; media suites; tests/live (media area) |
| Media download  | PASS      | PASS | BLOCKED | BLOCKED | docs/transfer.md; media suites; tests/live (media area) |
| FileReference   | PASS      | PASS | BLOCKED | BLOCKED | docs/media-domain.md; expiry retry suite; LIVE would be NOT_OBSERVED unless expiry occurs naturally |
| Albums          | PASS      | PASS | BLOCKED | BLOCKED | docs/albums.md; album suites |
| Errors          | PASS      | PASS | BLOCKED | BLOCKED | docs/error-contract.md; error matrix suite; tests/live (errors area) |
| Long-running    | PASS      | PASS | BLOCKED | BLOCKED | Phase 15 soak (10k/10k/1k/500/100); tests/live (soak area, 1–6 h cap) |

Harness plumbing itself (validated this phase):

| Harness element | Status | Source | Evidence |
|---|---|---|---|
| Bootstrap + tracing | PASS | FAKE | TestFakeWiringSessionReuse/Reconnect |
| Session-reuse op | PASS | FAKE | TestFakeWiringSessionReuse |
| Own-update op | PASS | FAKE | TestFakeWiringOwnUpdate |
| Media round-trip op (SHA-256) | PASS | FAKE | TestFakeWiringMediaRoundTrip |
| Error-matrix op | PASS | FAKE | TestFakeWiringErrorMatrix |
| Dialogs/history op | PASS | FAKE | TestFakeWiringDialogsHistory |
| Incoming window (NOT_OBSERVED path) | PASS | FAKE | TestFakeWiringIncomingWindow |
| Redaction (env secrets) | PASS | STATIC | TestStaticRedactor |
| Redaction (recorded fields, memory+disk) | PASS | STATIC | TestStaticRedactorRecorder |
| Status/source vocabularies | PASS | STATIC | TestStaticEvidenceClassification |
| Hash-prefix evidence | PASS | STATIC | TestStaticHashOf |
| Streaming pattern (O(chunk)) | PASS | STATIC | TestStaticPatternReader |
| Fail-closed (no config → all skip) | PASS | STATIC | verified: 15/15 SKIP with SP_SDK_LIVE=1 and no areas |

## Reference Differential Results

No new live-observable behavior could be compared (no live access); the
standing differential position is docs/reference-compatibility.md
(Phase 17), unchanged this phase. No production behavior was modified in
Phase 18 (zero production-tree changes).

## Protocol Evidence

None collected — LIVE/BLOCKED. The harness records, when it eventually
runs: constructor IDs/names, pts/pts_count, message IDs, peer kinds,
file sizes/part counts, error classes, latencies — and only hashes for
any content.

## Security

- Credentials never enter source control (env or env-named files only).
- BUG-18-1 (found by the STATIC suite during this phase): the recorder
  originally redacted only the serialized file line, leaving in-memory
  entries unredacted. Fixed during the phase: every string field is
  redacted before storage; regression test `TestStaticRedactorRecorder`
  pins memory AND disk safety.
- BUG-18-2: with the master switch on but no areas configured, the first
  verification run showed connect-dependent tests would attempt a real
  dial instead of reporting BLOCKED. Fixed by the explicit
  `SP_LIVE_AREAS` gate; verified 15/15 SKIP. No accidental network
  traffic is possible without a named area.

## Resource / Memory

Fake-mode: media op is streaming (O(part) — hash sink, pattern reader);
no whole-file buffering was introduced. Live resource measurement:
BLOCKED (the soak area performs it when enabled).

## Concurrency

`go test -race -count=1 ./...` — 3 consecutive full runs, 17/17 packages
(including `tests/live`), zero races.

## Bugs

See docs/phase18-audit.md (BUG-18-1, BUG-18-2 — both test-infrastructure,
both fixed with regression tests). No production bugs were discovered
(no live observations existed to discover any).

## Known Limitations

Every LIVE cell in the matrix remains unverified: authentication against
the real service, real SRP parameters, real salt rotation, real
FLOOD_WAIT behavior, real file-reference lifetimes, real update
cadence/pts behavior, DC migration, real album grouping, real gap
recovery. The fake-wiring proof covers harness mechanics only — it can
never substitute for the service.

## Release Verdict

```text
RELEASE BLOCKED — LIVE CREDENTIALS REQUIRED
```

The blocked live tests are exactly the matrix rows above. Phase 18 §38
criterion: the evidence now answers *which parts remain unverified* —
all live rows — and *is the evidence sufficient for v1.0* — no; the
v1.0 path (docs/versioning.md) requires the auth/session/messaging/
updates/reconnect/media/error live rows to PASS.
