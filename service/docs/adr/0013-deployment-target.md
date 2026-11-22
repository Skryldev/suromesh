# ADR-0013: Deployment target

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Deployment options evaluated in operations/deployment.md.

## Problem

Container, Kubernetes, or bare metal as the v1 target?

## Considered alternatives

1. **Bare metal** - rejected: loses packaging reproducibility.
2. **Kubernetes-only** - rejected as mandatory for v1.
3. **Container-first, Kubernetes-ready** (chosen).

## Decision

**Container-first (Docker), Kubernetes-ready** (chosen): distroless
non-root image, health probes, graceful termination, env/secret config,
read-only FS. Kubernetes manifests are provided as *examples* in S6;
K8s is not required. The deployment contract (probes, SIGTERM behavior,
resource profile) is fixed now so any orchestrator works.

## Rationale

Matches the single-process model (ADR-002) with a clean path to
K8s; avoids forcing K8s operational cost on small deployments.

## Trade-offs

Container registry and orchestration remain owner choices;
nothing auto-deploys.

## Security implications

Non-root + read-only FS shrink container-escape impact; secrets
never baked into images.

## Operational implications

Image build and publish are owner-gated phases (S6), with the
supply-chain rules already established in the repository (pinned
actions, no secrets in CI).
