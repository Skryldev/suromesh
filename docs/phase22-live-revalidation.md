# Phase 22 — Live Revalidation Runbook (NOT executed this phase)

Controlled procedure for the next live attempt. Prepared per Phase 22 §8;
execution requires the prerequisites below. It supersedes nothing in the
Phase 18 harness — it drives the SAME fail-closed, sanitized machinery
(`tests/live/`) with a stricter escalation ladder.

## Prerequisites (all mandatory)

1. **Cooldown**: ≥ 24 h since the last connection attempt to
   `im-server.splus.ir` (Phase 21 observed server-side handshake
   throttling). Longer is better; if the operator can confirm a healthy
   reference-client connection from the same egress, the endpoint is
   demonstrably admitting clients.
2. **Operator approval**: explicit go-ahead in the session (and ideally
   a stated window when the test phone is reachable for OTP relay).
3. **Credential-safe harness**: current `tests/live/` (fail-closed,
   redacted recorder; STATIC sanitizer tests green).
4. **Byte-level baseline**: `go test -run TestFirstEncryptedRPC
   ./tests/compatibility/` green (the Phase 22 vector) — proves local
   construction is reference-identical before blaming anything live.

## Escalation ladder (stop conditions at every step)

| Step | Command (opt-in env omitted here) | Pass | Stop if |
|---|---|---|---|
| 0. Health probe | one raw WS upgrade probe (curl, reference headers) | HTTP 101 within a sane time | EOF/timeout → cooldown longer; DO NOT retry-loop |
| 1. One connection | `SP_SDK_LIVE=1 SP_LIVE_AREAS=connect go test -run TestLiveLifecycleConnectClose ./tests/live/` | Ready state; clean Close | any transport failure — STOP (log class only) |
| 2. One first RPC | same suite (connect area includes initialize) | state Ready, initialize OK | connection drop → capture (below) and STOP |
| 3. Auth | `SP_LIVE_AREAS=auth` + phone/code env files; OTP interaction ONLY here, via the existing env-file mechanism, never echoed | authorized, GetMe OK | any failure — STOP |
| 4. Runbook areas | Phase 18 order: session → peers → dialogs → errors → reconnect → mutations → media → updates → soak | per-area evidence recorded | area failure → record, evaluate dependency chain |

Rules: exactly ONE connection attempt per step-1/2 execution (the
harness's connect budget is 180 s); **immediate stop on repeated
transport failures** — never iterate against a throttling server; OTP
only after steps 1–2 succeed (an initialized session exists); no
rotating egress identities; no bypassing of any throttle.

## Sanitized capture on a repeated first-RPC failure

If step 2 fails on a demonstrably healthy connection (step 0 passed
within minutes), collect BEFORE any code change:

- transport `OnError` classification + state-transition timestamps;
- engine metrics snapshot (started/completed/connlost/proto/unknown/
  env errors) — counts only;
- wall-clock durations of: WS upgrade, DH exchange, first-RPC write →
  close;
- whether the close came as a WS close frame (code?) or raw EOF;
- the Phase 22 vector test result re-run at that moment.

Storage: the harness recorder (redacted JSONL) or a sanitized driver
printout — NEVER raw keys, session bytes, packet payloads, or
credentials. A protocol-layer change is then (and only then) on the
table, per the Phase 21 §8 defect policy, with this capture as the
reproduction basis.

## Success criteria for unblocking release

Steps 1–3 PASS plus the Phase 18 critical areas (session reuse,
messaging, updates, reconnect, media, errors) PASS with sanitized LIVE
evidence — the docs/release-process.md §13 gate.
