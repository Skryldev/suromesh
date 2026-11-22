# Implementation Roadmap (Phase 24 design)

Phases are small, independently verifiable, and each ends with green
gates. Nothing here executes until the architecture review is accepted.
Versioning: service v0.x until the release-readiness gates pass; SDK
remains independently versioned.

| Phase | Scope | Verifiable outcome |
|---|---|---|
| **S1 — module bootstrap** | `service/go.mod` (nested module, SDK dependency via temporary relative replace), package skeleton (domain, app, adapter, transport, infra), lint gates, CI job | `go build ./... && go vet ./...` green in service/; SDK untouched |
| **S2 — contracts** | `.proto` finalized from the proposal, buf lint + breaking CI, codegen into `service/gen/`, generated docs | buf checks green; generated code committed with generation script |
| **S3 — SDK adapter** | `UpstreamGateway` implementation, error-translation tables, session blob import/export, in-process fake upstream for tests | adapter test suite green against real SDK + fake (T-ADAPT) |
| **S4 — persistence & crypto** | PostgreSQL repos, migrations, envelope encryption (DEK/KEK), operation records | T-SESSION green incl. rotation + crypto-shred; storage failure tests |
| **S5 — gRPC server core** | interceptors (authn/authz/telemetry/recovery/validation), health, graceful shutdown, AccountRunner + read RPCs (UserService, DialogService, ConnectionService) | T-HANDLER/T-ISOL green; E2E happy-path connect+getMe |
| **S6 — messaging & media** | MessagingService + DialogService paged RPCs, idempotency records, MediaService streaming, quotas/limits | messaging/media pyramids + T-IDEM green |
| **S7 — streaming** | UpdateService (StreamUpdates, StreamAccountEvents), fan-out hub, backpressure policies, CatchUp RPC | T-STREAM + load smoke green |
| **S8 — deployment artifacts** | Dockerfile (distroless, non-root), example K8s manifests, config reference, runbooks | image builds; E2E in container; probes verified |
| **S9 — service-docs completion** | full content build (this phase's skeleton), API reference from proto docs, language guides | docs CI green; link-checked |
| **S10 — release readiness** | security review, load tests, release/compat strategy execution, live-validation gate (inherited policy: LIVE evidence or documented BLOCKED) | release audit document |

## Release & compatibility strategy

- **Service versioning**: SemVer v0.x during S1–S9 (no stability
  promise); v1.0.0 only after: contract frozen (buf clean against
  release), all documented RPCs implemented + tested, security review
  passed, load targets met, live-validation status recorded honestly.
- **Contract releases**: proto artifacts tagged; generated language
  clients published only when the owner approves publication (never
  assumed).
- **Compatibility promises**: v1 additive-only (ADR-004); deprecation
  policy: announce ≥1 minor before removal, dual-serve during window.
- **SDK coupling**: service releases pin exact SDK versions; SDK security
  fixes trigger service patch releases.
- **Audit trail continuity**: the service inherits the project's
  evidence vocabulary (FAKE/LIVE, honest BLOCKED states) in its own
  audit documents.
