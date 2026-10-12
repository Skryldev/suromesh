# Phase 18 Audit & Bug Register

Scope: live-validation harness construction (Phase 18 §34 Live-Readiness
Track). Zero production-tree changes were made; the frozen layers, the
generated TL output, and `reference/SPlusthon` are untouched.

## Bug register

### BUG-18-1 — evidence recorder left in-memory entries unredacted

- **Symptom**: `TestStaticRedactorRecorder` failed — a secret planted in a
  record field survived in `Recorder.Entries()` even though the on-disk
  JSONL line was redacted.
- **Root cause**: `Record()` passed raw fields into the stored `Evidence`
  and only applied the redactor to the serialized file line — a
  secret-safe FILE but a secret-leaking MEMORY log.
- **Layer**: tests/live (harness only; no SDK code).
- **Reference expectation**: Phase 18 §5/§27 — secrets must never enter
  test artifacts; in-memory and persisted records are equally artifacts.
- **Live observation**: none (found by the STATIC sanitization suite
  during harness construction).
- **Severity**: test-infrastructure (would have become high the moment
  real credentials flowed through the harness).
- **Fix**: `Record()` redacts every string field BEFORE storage; the
  serialized line is redacted too (defense in depth).
- **Regression test**: `TestStaticRedactorRecorder` (pins memory AND disk
  safety).
- **Compatibility impact**: none (harness-only).

### BUG-18-2 — master switch alone could trigger real network dialing

- **Symptom**: during fail-closed verification, `SP_SDK_LIVE=1` with no
  other configuration caused connect-dependent live tests (soak,
  incoming) to attempt a real connection to the default production
  endpoint (failing after the 30 s connect timeout with FAIL, not
  BLOCKED).
- **Root cause**: the master switch gated execution, but the
  *activation* of specific test areas was only implied by the presence
  of area-specific configuration. Absent ALL configuration, nothing
  stopped a connect attempt — violating §5 ("fail closed when required
  configuration is absent") and the no-accidental-run rule.
- **Layer**: tests/live (harness only).
- **Reference expectation**: §5 explicit opt-in; §34.9 clearly identified
  blocked tests — a dial attempt is neither.
- **Live observation**: the verification run itself (no traffic reached
  the service; the sandbox dial timed out and the test failed closed
  AFTER the timeout — still too late).
- **Severity**: test-infrastructure (high consequence if a future
  operator flipped the switch while expecting no traffic).
- **Fix**: explicit area gate — `SP_LIVE_AREAS` must name the area; with
  no areas named, all live tests skip before ANY I/O. Account areas
  additionally require an authorized `SP_LIVE_SESSION` file (or, for
  auth/session, phone+code up-front). Verified: `SP_SDK_LIVE=1` with no
  areas → 15/15 SKIP, zero network attempts.
- **Regression test**: verified behaviorally in this phase (documented
  in docs/phase18-live-validation.md); the guard logic is executed by
  every live test's skip path.
- **Compatibility impact**: none (harness-only).

## Audit findings recorded without code changes

- The §15 rich-text validation and §19/§16 cross-account update
  validations are operator-assisted by design (they require an entity
  beyond this harness's reach); they report NOT_OBSERVED rather than
  fabricating observations.
- `ReconnectTransportForTest` (documented INTERNAL seam) is used by the
  reconnect op to force a real transport teardown; the graceful
  Disconnect/Connect path has its own test. This keeps failure injection
  honest without touching transport code (frozen).
- FileReference expiry (§22) and album grouping (§23) remain NOT_OBSERVED
  by design unless the live service exhibits them during a run; the
  harness does not manufacture expiry (no mutation of server state).
