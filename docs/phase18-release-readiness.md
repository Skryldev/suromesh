# Phase 18 — Release Readiness

## Executive Summary

Phase 18 set out to determine whether the Go SDK behaves correctly
against the real Soroush Plus service. Because **no real credentials are
available** (and none have ever been), the phase executed its §34 branch:
the complete **Live-Readiness Track** was delivered — an executable,
fail-closed, sanitized live harness with proven plumbing — and every
live validation cell is reported as BLOCKED. No production code changed.
The verdict is unambiguous and honest:

```text
RELEASE BLOCKED — LIVE CREDENTIALS REQUIRED
```

## Credential Status

```text
UNAVAILABLE
```

No phone, OTP, password, session key, or account exists for this
project. Nothing was dialed, nothing was authenticated, nothing was
fabricated.

## Live Test Results

Full matrix in docs/phase18-live-validation.md. Summary: **14 capability
areas** — Reference PASS (executed-Python differential evidence, Phases
1–17), Fake PASS (deterministic fake servers over the real protocol
stack), **Live BLOCKED** for every row. Harness plumbing: 13 elements
PASS (8 FAKE + 5 STATIC).

## Reference Differential Results

No live observations exist to differ. The standing position
(docs/reference-compatibility.md) is unchanged: REFERENCE-PARITY for
ported semantics with executed-reference evidence; documented
GO-IDIOMATIC-DEVIATIONs; INTENTIONAL-SCOPE and SCHEMA-BLOCKED items
unchanged; UNVERIFIED items remain exactly the live-only behaviors.

## Bugs

Two test-infrastructure defects found and fixed during harness
construction, both with regression tests (docs/phase18-audit.md):

- BUG-18-1 — recorder left in-memory evidence unredacted (secret-leak
  class; fixed before any credential could ever flow through).
- BUG-18-2 — master switch alone could trigger a real network dial;
  replaced by the explicit `SP_LIVE_AREAS` gate (verified 15/15 SKIP
  with zero network attempts).

No production bugs: there were no live observations to reveal any, and
§35 forbids calling a discrepancy a bug without evidence.

## Security

The sanitization pipeline is the deliverable: environment-only
credentials, redactor registered with every secret (memory AND disk,
pinned by test), hash-prefix evidence for all content, FNV fingerprints
for auth keys, no SMS wasted without an up-front code, no accidental
network traffic without a named area.

## Resource / Memory

The live media op preserves the O(part) contract by construction
(streaming hash sink + pattern reader, STATIC-tested). Live measurements
await the soak area.

## Concurrency

`-race` × 3 consecutive full runs: 17/17 packages, zero races (the new
`tests/live` package included). All 46 fuzz seed corpora green.

## Protocol Evidence

None (LIVE/BLOCKED). The evidence schema is implemented and pinned:
constructor IDs/names, pts/pts_count, message IDs, peer kinds, sizes,
error classes, latencies — content only as SHA-256 prefixes.

## Known Limitations

All live rows of the matrix are unverified. Until credentials exist,
v1.0 remains blocked (docs/versioning.md: live validation is the single
hard requirement on the v1 path).

## Release Verdict

```text
RELEASE BLOCKED — LIVE CREDENTIALS REQUIRED
```

Critical live categories blocked: authentication, session reuse,
messaging, updates, reconnect, media upload/download, error handling.
When credentials become available: run the operator runbook
(tests/live/README.md) areas in order — connect → auth → session →
peers → dialogs → errors → reconnect → mutations → media → updates →
soak — then re-issue this matrix. If all critical rows PASS with the
hard gate green and no unresolved correctness defect, the §33 conditions
for `RELEASE QUALIFIED` are met.
