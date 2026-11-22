# Deployment Architecture (Phase 24 design)

## Deployment options evaluated

| Option | Verdict | Rationale |
|---|---|---|
| **Single-node container (v1)** | chosen | Correct under the single-owner account model (ADR-007); minimal operational surface; matches expected initial scale |
| Kubernetes | ready-but-optional | Service is K8s-shaped (probes, graceful stop, secrets, read-only FS); adopted when multi-instance or fleet management is needed |
| Horizontally scaled multi-instance | future | Requires account sharding via lease table (ADR-002/007) — designed, not built in v1 |

## Container (design requirements for phase S6)

- Multi-stage build: golang builder → **distroless/static** (or
  scratch) runtime; non-root UID; read-only root filesystem;
  `CGO_ENABLED=0`.
- Image labels: org.opencontainers.image.{title,version,revision,source}.
- Health: gRPC health service (`grpc.health.v1`) — liveness (process),
  readiness (process + storage reachable). Graceful termination ≤ 30 s
  honoring SIGTERM (reliability shutdown ordering).
- Resources: requests 256 Mi/0.25 CPU, limits 1 Gi/1 CPU (tune with
  load tests); media streaming memory is O(chunk) by SDK design.
- Config: file (`configs/service.yaml`) + env overrides; secrets via
  env/secret store only. Example config documents every key
  (`configs/service.example.yaml` in S2).

## Data

- **PostgreSQL 15+**: tenants, credentials (hashes), account bindings,
  encrypted session blobs, operation records, audit log. Migrations via
  versioned SQL files applied by a dedicated migration step (never
  auto-migrate on boot without a flag).
- **Backups**: standard PG backups; session blobs are encrypted with an
  external KEK — **the KEK must be backed up separately** or backups are
  unrecoverable (documented; crypto-shredding is the deletion story).
- **Upgrades**: protobuf additive evolution + schema migration discipline;
  session blob format versioned (upgrade path mirrors the SDK's format
  versioning).

## Scaling path (future, ADR-002)

1. Vertical: bigger node, more accounts per process.
2. Horizontal: N instances, account→instance leases in PostgreSQL
   (heartbeat + fencing token); a shard-balancer assigns accounts;
   instances only serve their leased accounts. In-memory ownership is
   never claimed as distributed safety.
3. Optional read replica for metadata reads (not sessions).

## NOT created in this phase

Dockerfile, compose files, K8s manifests, migrations — all land in
roadmap phase S6 as reviewed implementation artifacts; this document is
the contract they must satisfy.
