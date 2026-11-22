# SuroMesh Service (design phase)

A production-grade **gRPC microservice** that exposes the SuroMesh SDK
(`github.com/Skryldev/suromesh` — MTProto 2.0, Layer 182) to
applications in any gRPC language, with multi-tenant account isolation,
encrypted session persistence, and OpenTelemetry observability.

**Status: S1–S5+ IMPLEMENTED** (Phases 25 + master spec execution) — the
nested Go module now includes:

- **Protobuf contracts** (`api/`, buf-linted; generated code in `gen/`)
  — nine v1 services: Authentication, Account, Session, Connection,
  User, Dialog, Messaging, Update (server-streaming), Media
  (client/server-streaming).
- **Domain + application layers** (pure domain; account runner actor
  with serialized mutations, concurrent reads, bounded update fan-out).
- **SDK adapter** (`internal/infrastructure/upstream`) — the real SuroMesh
  SDK integration incl. session state export/import (encrypted at rest).
- **Persistence** (PostgreSQL, pgx: tenants/credentials/accounts/
  encrypted sessions/idempotency/audit + versioned migrations).
- **Envelope encryption** (per-session DEK, external KEK provider; file
  provider for dev/self-hosted; KMS fails closed pending owner decision).
- **gRPC server** (authn/authz interceptors, health, recovery, deadline
  enforcement, reflection off by default) with all nine services wired.
- **Deployment artifacts** (Dockerfile, docker-compose with PostgreSQL,
  Kubernetes manifests, Makefile).

Not yet implemented: string-session import (S3+), observability export
(OTLP wiring), multi-replica leasing. Live-service validation remains
NOT PERFORMED — credentials unavailable (tracked since Phase 18).

## S1 engineering documents

- [`docs/development.md`](docs/development.md) — build/test/validate commands, layout, boundary rules
- [`docs/architecture.md`](docs/architecture.md) — as-built runtime architecture (state machine, SDK boundary)
- [`docs/configuration.md`](docs/configuration.md) — configuration keys, defaults, precedence, reserved keys
- Example configuration: [`configs/service.example.env`](configs/service.example.env)

## Design documentation (Phase 24)

| Document | Contents |
|---|---|
| [`docs/architecture/system-architecture.md`](docs/architecture/system-architecture.md) | system context, layers, decisions index, tenancy model |
| [`docs/architecture/capability-audit.md`](docs/architecture/capability-audit.md) | SDK capability audit feeding every contract |
| [`docs/architecture/domain-model.md`](docs/architecture/domain-model.md) | entities, invariants, state machines, ports |
| [`docs/architecture/reliability.md`](docs/architecture/reliability.md) | failure matrix F1–F11, deadlines, idempotency, shutdown |
| [`docs/architecture/sdk-integration.md`](docs/architecture/sdk-integration.md) | the adapter boundary (no protocol duplication) |
| [`docs/api-design/protobuf.md`](docs/api-design/protobuf.md) | contract design, RPC table, error mapping |
| [`docs/api-design/proto/suromesh/service/v1/service.proto`](docs/api-design/proto/suromesh/service/v1/service.proto) | proposed `.proto` (design artifact) |
| [`docs/security/security-architecture.md`](docs/security/security-architecture.md) | authn/authz, secrets, quotas, audit |
| [`docs/operations/deployment.md`](docs/operations/deployment.md) | container-first deployment, data, scaling path |
| [`docs/testing-strategy.md`](docs/testing-strategy.md) | full test pyramid incl. isolation and failure injection |
| [`docs/implementation-roadmap.md`](docs/implementation-roadmap.md) | S1–S10 phases + release/compat strategy |
| [`docs/adr/`](docs/adr/) | ADR-0001 … ADR-0014 |
| [`../service-docs/`](../service-docs/) | independent Docusaurus website for the service |

## Hard architectural rules

1. The SDK stays an independent library — no gRPC dependencies enter it.
2. No protocol logic duplication: the service drives the SDK through a
   single adapter seam.
3. Domain layer never imports SDK or protobuf packages.
4. One SDK client per account, single-owner per process (ADR-007);
   in-memory ownership is never claimed as distributed safety.
5. Session material encrypted at rest (envelope encryption, ADR-008);
   OTP never persisted; secrets never logged.
6. Raw MTProto invoke is not part of the v1 public API (ADR-003).
