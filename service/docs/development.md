# Development Guide (S1)

How to build, test, and validate the SuroMesh Service module.

## Layout

```text
service/
├── cmd/service/            entry point (config → logging → bootstrap → signals)
├── internal/app/           runtime state machine (Run/Stop semantics)
├── internal/application/   ordered resource-lifecycle engine
├── internal/config/        typed configuration (env over defaults, validated)
├── internal/domain/        pure model (NO SDK/protobuf/gRPC imports — CI-checked)
├── internal/ports/         driven-port interfaces (gateway, future repositories)
├── internal/platform/      logging (slog: levels, request-id, redaction)
├── internal/infrastructure/gateway/   SDK adapter skeleton (deterministic S1 stubs)
├── configs/                example configuration
├── scripts/                release-policy check
└── docs/                   engineering documents (+ Phase 24 design under docs/)
```

## Commands (from `service/`)

```bash
go build ./...             # compile
go vet ./...               # static analysis
go test -count=1 ./...     # unit + lifecycle tests
go test -race -count=1 ./...   # race detector
test -z "$(gofmt -l .)"    # formatting
go run ./cmd/service       # local run (set SUROMESH_LISTEN_PLAINTEXT_DEV=true)
bash scripts/check-release.sh               # release policy (dev tolerated)
RELEASE_BUILD=1 bash scripts/check-release.sh   # strict (release builds)
```

## SDK dependency

`service/go.mod` requires `github.com/Skryldev/suromesh` with an
**isolated development replace** (`=> ../`). The release policy:

- `scripts/check-release.sh` (dev mode) warns;
- `RELEASE_BUILD=1 scripts/check-release.sh` (CI strict step, release
  builds) **fails** when any replace directive exists;
- removal procedure is documented in `go.mod` itself.

The SDK root module remains untouched: no gRPC dependencies, no public
API changes (verified per gate run).

## Boundaries

- `internal/domain` imports nothing from the SDK/protobuf/gRPC/infra —
  enforced by `TestDomainDependencyBoundary` (go list -deps).
- Only `internal/infrastructure/gateway` may import SDK packages.
- The application layer depends on `internal/ports` interfaces only.

## Conventions

- No global mutable state; explicit constructor injection.
- No DI framework.
- Every operation that is intentionally unimplemented returns the
  explicit `domain.ErrDisabled` — never a silent success.

## S2–S5 additions (as-built)

- **Protobuf**: contracts in `api/suromesh/service/v1/*.proto`; lint +
  generate via buf (`make proto-lint`, `make proto`). Generated code in
  `gen/` — never hand-edit; CI fails on drift.
- **Upstream adapter**: `internal/infrastructure/upstream` — session
  state blob (JSON v1: key/DC/authorization/update frontier), error
  translation (SDK → domain taxonomy), by-id message retrieval over the
  public Invoke surface, update conversion (raw ctors → domain events).
- **Account runtime**: `internal/application` — Runner (serialized
  mutations via bounded queue + pump goroutine; concurrent reads via
  semaphore; update hub with bounded per-subscriber queues and
  slow-consumer drop), Manager (duplicate creation rejected; tenant-
  scoped Get; StopAll joins).
- **Persistence**: `internal/infrastructure/persistence` — pgx/v5,
  embedded migrations (schema_migrations ledger), per-port repo types
  with compile-time conformance. Integration tests run only with
  TEST_DATABASE_DSN (CI: postgres service container).
- **Cryptography**: `internal/infrastructure/cryptography` — envelope
  encryption (per-session DEK, KEK via provider, AES-256-GCM, key
  versions, ReWrap rotation, crypto-shred by row deletion). File
  provider for dev/self-hosted; KMS fails closed (owner decision).
- **gRPC**: `internal/grpcapi/{server,interceptors,handlers,errors}` —
  interceptor chain (recovery → authn → per-method permission →
  deadline), health exempt from authn, ErrorInfo reason mapping,
  client/server-streaming handlers with bounded buffers.
