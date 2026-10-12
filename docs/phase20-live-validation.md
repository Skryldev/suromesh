# Phase 20 — Live Validation

## Executive Summary

Phase 20's sole mission: determine with executable evidence whether the
SDK can be legitimately released and used against the **real Soroush Plus
service**. The controlling fact is unchanged since Phase 9 and was
re-verified at the phase start: **no live credentials are available to
this project** (no phone, OTP, password, or authorized account), and none
of the four owner-level release prerequisites (license, final module
path, VCS provenance, credentials) has been actioned since Phase 19.

Per §31 ("credentials are unavailable" is a stop condition) and §18
(fail closed; do not modify production code to compensate), the live
gate was therefore **not executed** and no live result is claimed. What
Phase 20 did establish, with fresh evidence:

1. **Baseline integrity** — the Phase 19 engineering baseline has not
   drifted: a fresh byte copy of the tree passes the complete gate
   battery (build, vet, gofmt 0, tidy no-op, mod verify, generator
   verification, 17/17 test packages, race ×3 with zero races);
   `reference/SPlusthon` unchanged; `tl/schema` unchanged; `tl/gen`
   deterministic (write-mode regeneration byte-identical).
2. **Live-readiness of the harness** — the credential sanitizer is
   trustworthy (STATIC redaction tests green) and the master switch
   fails closed (with `SP_SDK_LIVE=1` and no areas named: 15/15 live
   tests SKIP before any network I/O; zero live tests run).
3. **The exact blocked state** — every live area enumerated below as
   BLOCKED, ready to execute the moment operator credentials exist.

## Credential Status

```text
UNAVAILABLE
```

No credential ever entered the repository, the environment, or this
phase's process. Nothing was dialed. No SMS/OTP was requested (§12
satisfied trivially — no OTP exists to provide).

## Live Area Results (§9 order)

Dependency note: every area below depends on `connect` → `auth`; auth
depends on operator credentials. The dependency chain is broken at its
first link, which blocks everything downstream — recorded per §9 rather
than blindly continued.

| # | Area | Result | Source | Evidence |
|---|---|---|---|---|
| 1 | Connect | BLOCKED | — | credentials unavailable; harness executable (FAKE-wiring proven, Phase 18) |
| 2 | Authentication | BLOCKED | — | requires SP_LIVE_PHONE + SP_LIVE_CODE |
| 3 | Session persistence/reuse | BLOCKED | — | requires an authorized SP_LIVE_SESSION (auth first) |
| 4 | Peer resolution | BLOCKED | — | requires authorized session |
| 5 | Dialogs | BLOCKED | — | requires authorized session |
| 6 | History | BLOCKED | — | requires authorized session |
| 7 | Updates | BLOCKED | — | requires authorized session + operator-assisted window |
| 8 | Messaging | BLOCKED | — | requires authorized session + SP_LIVE_MUTATIONS=1 |
| 9 | Message mutations | BLOCKED | — | same |
| 10 | Media upload/download | BLOCKED | — | requires authorized session + SP_LIVE_MEDIA=1 |
| 11 | File references | BLOCKED | — | naturally-observed-only (NOT_OBSERVED unless expiry occurs live) |
| 12 | Albums | BLOCKED | — | requires mutations + media |
| 13 | Reconnect | BLOCKED | — | requires authorized session |
| 14 | Error handling | BLOCKED | — | requires authorized session |
| 15 | Long-running / soak | BLOCKED | — | operator-scheduled (SP_LIVE_SOAK_MINUTES) |

No LIVE/PASS is claimed anywhere. Fake-wiring results (Phase 18) remain
classified FAKE and are never used as live evidence (§21).

## Differential Classification

Not applicable this phase: no live observations exist to classify.
REFERENCE and FAKE evidence stand as documented (Phase 17/18 reports);
no discrepancy categories were triggered; no production code was
modified (§22's bug protocol was never invoked).

## Evidence Artifact

None produced — LIVE/BLOCKED. The Phase 18 evidence mechanism (redacted
JSONL recorder) is verified functional and will produce the sanitized
artifact when the runbook executes.

## Re-Verification Performed This Phase

| Check | Result |
|---|---|
| Owner gates (LICENSE / module path / VCS / credentials) | BLOCKED / PLACEHOLDER / BLOCKED / UNAVAILABLE (unchanged) |
| Sanitizer STATIC tests (redaction memory+disk, vocabularies) | PASS |
| Master switch fail-closed (SP_SDK_LIVE=1, no areas) | 15/15 SKIP, zero network I/O |
| Clean-copy baseline battery | ALL GREEN |
| Race ×3 (clean copy) | 17/17 each, zero races |
| Generator write-mode determinism | byte-identical |
| reference/SPlusthon | 0 modified files |
| tl/schema | unchanged (cmp-identical to reference data) |
