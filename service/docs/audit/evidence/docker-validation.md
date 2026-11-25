# Docker Validation Results — Phase 27

Environment: Docker Engine 29.9.3 (API 1.56) · Compose v5.7.1 ·
linux/amd64 · host kernel 7.0.0-43-generic. All runs via
`scripts/ci.sh` (per-run Compose project `suromesh-ci-<ts>-<pid>`).

## Images

| Image | Base | Size | User | Contents |
|---|---|---|---|---|
| `suromesh-ci-runner:phase27` | `golang:1.26-alpine` | ~3.0 GB (toolchain + pinned tools) | root (ephemeral CI worker on bind-mounted checkout) | go, buf 1.75.0, protoc-gen-go 1.36.5, protoc-gen-go-grpc 1.5.1, staticcheck 0.8.2, govulncheck 1.10.0, gotestsum 1.13.0, GNU grep, psql 18.7 client, git, jq, gcc (race) |
| `suromesh-service:phase27-e2e` | **scratch** | **31.7 MB** | **65532:65532 (nonroot)** | static binary (`CGO_ENABLED=0 -trimpath -ldflags "-s -w"`) + `ca-certificates.crt` ONLY |
| `postgres:17-alpine` (test) | docker.io | 424 MB | postgres | tmpfs data (`--no-sync`), throwaway credentials, internal network only |

## Build qualification

- Multi-stage build from a clean context: PASS (`service-image-build` step).
- Development replace directive (`service/go.mod → ../`) resolves in-image
  because the build mirrors the checkout layout (`/sdkroot/{service,sdk pkgs}`).
- Release builds remove the replace (policy gate `release-policy-strict`
  proves strict mode REJECTS it — required behavior, PASS).
- No source, no reference tree, no dev tools in the runtime image
  (scratch stage has exactly two COPY entries).
- Registry constraint discovered: this environment's egress blocks
  gcr.io / ghcr.io / cgr.dev (403). Consequence: runtime stage uses
  `scratch` (stricter than distroless-static) and the CI toolchain uses
  the checksum-verified mirror `goproxy.cn` with `direct` fallback.
  Both documented as environment adaptations, not project changes.

## Runtime qualification (smoke profile)

| Check | Method | Result |
|---|---|---|
| Non-root runtime | `docker image inspect .Config.User` = `65532:65532` | PASS |
| No shell in runtime | `docker run --entrypoint /bin/sh <img>` fails | PASS (scratch: no interpreter at all) |
| Distroless-grade minimality | image listing: binary + CA certs only | PASS |
| stdout/stderr JSON logs | `compose logs service` | PASS |
| Database connectivity + migrations | service boots through `storage ready (migrations applied, schema verified)` against postgres-test | PASS |
| Graceful shutdown | `docker kill --signal=TERM` → log line `service terminated cleanly` | PASS |
| gRPC serving (plaintext dev flag, container network only) | e2e readiness check via WaitForReady | PASS |

## Orchestration properties verified

- Per-run isolation: unique compose project per `ci.sh` invocation
  (network + containers + ephemeral state); `trap`-driven
  `down --volumes --remove-orphans` on exit — success, failure, or INT/TERM.
- Cross-step determinism: `reset_state` between full-pipeline steps wipes
  the ephemeral postgres, so a failed step cannot contaminate later ones
  (adopted after a real cross-step contamination was observed mid-phase).
- Ephemeral-only test data: postgres `tmpfs` (1g) + `--no-sync`; no
  persistent test volume exists anywhere in the CI stack. External named
  volumes hold ONLY the Go module/build caches (concurrency-safe).
- Secrets: none committed. The e2e KEK (`.e2e-kek.bin`, 32 raw bytes)
  and bootstrap key are generated per run from urandom, mounted via
  Compose secrets, chmod 600, deleted on cleanup; `.gitignore` covers them.
- Health-gated dependencies: `ci` waits on `postgres-test: service_healthy`
  (`pg_isready`), scripts additionally run explicit psql readiness loops.
- Timeouts: every script wraps its tools in explicit `timeout` budgets;
  fuzz campaigns run exactly `FUZZTIME_SECONDS` (15s) per target.
- Exit-code contract: `set -euo pipefail` everywhere; step exit codes
  propagate; no pipeline swallows failures.

## Failure log locations

- Full run log: `evidence/phase27/ci-run.log` (all steps, streamed).
- Per-step JUnit: `evidence/phase27/junit/*.xml`.
- Compose project listing of a failed run:
  `docker compose -p suromesh-ci-<runid> -f service/deploy/compose/ci.compose.yml ps -a`.
- Cleanup: `./scripts/ci.sh clean`.
