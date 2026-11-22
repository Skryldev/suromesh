# Testing Strategy (Phase 24 design)

No test requires live Soroush Plus credentials by default; live tests
are isolated, opt-in, fail-closed (SP environment pattern inherited
from the SDK).

## Test pyramid per roadmap phase

| ID | Layer | What | Tooling |
|---|---|---|---|
| T-PROTO | Contract | `.proto` compile + buf lint/breaking vs last release | buf CI (S2) |
| T-HANDLER | Transport | per-RPC handler unit tests: validation, authz wiring, error translation (table-driven) | Go test + generated stubs |
| T-APP | Application | use-case tests against fake ports (repos, gateway) | Go test |
| T-ADAPT | SDK adapter | gateway implementation against the SDK's **real stack + `internal/wstest`** fake server — impossible to import (internal), so the service ships its own in-process fake in the service module (reusing the SDK's public demo patterns from `examples/`) | Go test |
| T-SESSION | Persistence | session encrypt/decrypt round trips, corruption, key rotation, crypto-shred | Go test + test KEK |
| T-ISOL | Security | tenant isolation: every RPC probed cross-tenant (expect NOT_FOUND), permission matrix pinned, existence-oracle probes | Go test |
| T-AUTHN | Security | key lifecycle: create/rotate/revoke; constant-time compare; redaction middleware | Go test |
| T-STREAM | Streaming | StreamUpdates/StreamAccountEvents/StreamHistory: cancellation, slow-consumer disconnect, per-stream sequence, reconnection semantics | Go test with fake time |
| T-IDEM | Reliability | idempotency dedupe: replay of SUCCEEDED/AMBIGUOUS, retention window | Go test |
| T-FAIL | Reliability | failure matrix F1–F11 via failure injection (storage down, upstream disconnect, crash-recovery) | Go test |
| T-RACE | Concurrency | `-race` full suite ×3; runner serialization tests; fan-out hub races | Go test |
| T-E2E | System | gRPC client → service → fake upstream, full flows (bind → auth → send → receive → media) | Go test (testcontainers Postgres) |
| T-SEC | Security | secret scan, mTLS mode, reflection off, header handling | CI |
| T-LOAD | Load | streaming fan-out at N accounts × M consumers; media throughput; latency under queue pressure | go test -bench + profiling (S7) |
| T-LIVE | Live | opt-in only: SP_SDK_LIVE-style env gates; fail-closed; sanitized evidence | inherited pattern |

## Principles

- **Fake upstream at the SDK boundary, not at the wire**: T-ADAPT runs
  the real SDK against an in-process fake, so service tests exercise
  the true error/ambiguity semantics rather than mocks that encode
  wishes.
- **Contract tests are generated** where possible: protobuf round-trip
  and error-detail assertions generated from the same tables the
  handlers use.
- **The SDK's own discipline is inherited**: fake evidence labeled
  FAKE, live labeled LIVE, never mixed; no credential material in any
  fixture; secrets only via environment.
- **Coverage gates**: domain + application layers ≥ 85% lines; adapter
  ≥ 80%; no coverage theater — the matrix above is the gate, not %.

## CI shape

- `service-ci.yml`: lint (vet, staticcheck, gofumpt) → build → unit
  pyramid (T-PROTO..T-IDEM, T-RACE) → T-E2E (Postgres service
  container) → T-SEC scan.
- Live T-LIVE never runs on push/PR; `workflow_dispatch` + env-gated,
  following the established repository pattern.
