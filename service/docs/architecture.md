# Runtime Architecture (S1 as-built)

This document records what S1 actually implements; the full product
design remains in `docs/architecture/` (Phase 24). Where this document
and the design disagree later, the design wins and this file is updated
at each phase.

## Layers (as-built)

| Package | Responsibility | May import |
|---|---|---|
| `cmd/service` | wiring: config → logging → resources → app; signals | all internal |
| `internal/app` | runtime state machine: `Created→Starting→Running→Stopping→Stopped/Failed`; Run/Stop semantics | config, domain, application, platform |
| `internal/application` | ordered resource lifecycle: StartAll (failure → reverse cleanup), StopAll (reverse, joined errors, ctx-budgeted) | domain |
| `internal/config` | typed configuration, env parsing, validation, reserved-key fail-closed | — |
| `internal/domain` | pure model (ids, states, sentinels) | **nothing** (CI-enforced) |
| `internal/ports` | driven-port interfaces (`UpstreamGateway`, future repositories) | domain |
| `internal/platform/logging` | slog setup: levels/formats, request-id propagation, secret redaction handler | config |
| `internal/infrastructure/gateway` | SDK adapter skeleton: option mapping (pure), deterministic `ErrDisabled` operations | config, domain, ports, **SDK** |

## State machine

```text
Created → Starting → Running → Stopping → Stopped
                │                    │
                └── (failure) ───────┴──→ Failed
```

- `Run(ctx)`: at most once. Startup failure stops the started prefix in
  reverse and lands in `Failed` with the joined error.
- `Stop(ctx)`: idempotent request + await terminal; before `Run` it is
  a deterministic `ErrInvalidState`; after terminal it returns the
  recorded result. Concurrent callers share one shutdown (verified:
  resource Stop entered exactly once).
- Shutdown budget: `shutdown.timeout` config, further bounded by the
  caller's remaining context budget; deadline exceeded → `Failed` with
  `DeadlineExceeded`.

## SDK boundary (S1 shape)

`internal/infrastructure/gateway` derives SDK `client.Config` options
by pure mapping (no I/O, no session material, no client construction)
and returns `domain.ErrDisabled` for every upstream operation. S3
replaces the stubs with real SDK calls behind the same
`ports.UpstreamGateway` interface — no application-layer change.

## What does NOT exist yet (by phase plan)

gRPC listener (S5), protobuf codegen (S2), persistence/PostgreSQL (S4),
encryption/KEK (S4), account runners (S5), business RPCs (S5-S7),
streaming (S7), deployment artifacts (S8).
