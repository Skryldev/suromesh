# SuroMesh Service — Phase 27 Command Evidence (final)

Executed: 2026-11-24 (definitive pipeline run) · Docker Engine 29.9.3 ·
Compose v5.7.1 · linux/amd64 · toolchain pinned in
`suromesh-ci-runner:phase27` (Go 1.26.11, buf 1.75.0, protoc-gen-go
1.36.5, protoc-gen-go-grpc 1.5.1, staticcheck 0.8.2, govulncheck 1.10.0,
gotestsum 1.13.0) · PostgreSQL `postgres:17-alpine` (tmpfs, ephemeral).

Primary evidence: `phase27/ci-run.log` (full streamed log),
`phase27/junit/*.xml` (machine-readable), per-step logs in `phase27/`.

## 1. Pipeline step results (`./scripts/ci.sh ci`)

| Step | Key checks | Result |
|---|---|---|
| static-gates | gofmt, go mod tidy/verify (no drift), go build, go vet, staticcheck ./... , SDK build+vet, domain boundary, buf lint, buf generate (no drift), buf breaking vs Phase-27 baseline | PASS |
| unit-tests | service: 141 tests (9 packages) with real PG reachable; SDK: 637 tests | PASS |
| fuzz-bounded | seeds of 4 security-sensitive targets + bounded campaigns (15 s/target, exact durations logged) | PASS |
| race | SDK `-race` 637 tests, 0 races; service `-race` 141 tests against real PostgreSQL | PASS |
| integration | 6 persistence tests (tenant isolation, sessions, idempotency + concurrent claims, audit) + 5 migration scenarios A–E on postgres:17-alpine | PASS |
| migration (CLI) | fresh up, status, idempotent rerun, structural schema validation (tables + FK counts), 4-PROCESS concurrent migrate up (exactly-once) | PASS |
| security | govulncheck service+sdk (**BLOCKED_ENVIRONMENT**: vuln.go.dev 403 — loudly recorded, not a pass), envelope adversarial battery 7/7, authn/authz battery, config fail-closed, committed-secret scan | PASS (with recorded block) |
| service-image-build | multi-stage scratch image, 31.7 MB | PASS |
| e2e-wire-tests | 13 wire checks against the real service container (gRPC WaitForReady) | PASS (13/13) |
| smoke | image user 65532:65532; no shell in image; stdout JSON logs; SIGTERM → "service terminated cleanly" | PASS |
| release | release policy dev-OK + strict-mode REJECTS replace; module separation; vector JSON integrity; required docs present | PASS |

## 2. Key commands (exact)

```bash
./scripts/ci.sh ci                 # full pipeline (the definitive run)
./scripts/ci.sh test|race|integration|migration|e2e|security|build|smoke|release
./scripts/ci.sh clean
# benchmarks (bounded):
docker compose -f service/deploy/compose/ci.compose.yml --profile test \
  run --rm ci bash service/scripts/ci/bench.sh
# media memory evidence:
docker run --rm -v "$PWD":/src -w /src \
  -v suromesh-ci-gomod:/go/pkg/mod -v suromesh-ci-gobuild:/root/.cache/go-build \
  golang:1.26-alpine go test -run 'TestMemoryEvidence' -v ./client/
```

## 3. Measured highlights

- Media bounded memory (SDK, fake transport, discard/synth mode):
  upload 32 MiB → heap growth 449 KiB; download 32 MiB → 149 KiB
  (budget 8 MiB) — `client/media_memory_evidence_test.go`.
- SDK race suite: 637 tests, 0 data races.
- Migration concurrency: 8-goroutine + 4-process races — exactly-once
  application, consistent ledger (advisory lock).
- Envelope adversarial: every truncation, every byte flip, hostile
  length fields, dead provider — all typed ErrCrypto, zero panics.
- E2E wire: 13/13 (incl. honest-failure contract and raw-phone
  non-disclosure) after fixing SEC-27.

## 4. Environment adaptations (documented, not silent)

- egress blocks proxy.golang.org's storage backend, gcr.io, ghcr.io,
  cgr.dev, vuln.go.dev → GOPROXY=goproxy.cn (checksum-verified mirror),
  runtime base = scratch, govulncheck recorded BLOCKED_ENVIRONMENT.
- Compose v5.7.1 quirk: relative paths resolve one level shallower than
  the spec in this environment — compose file carries calibrated paths
  with an explanatory comment.

## 5. Totals reconciliation

- Service tests: 141 unit (incl. integration when PG present; without
  TEST_DATABASE_DSN they skip with an explicit message — never counted
  as PASS).
- SDK tests: 637 (17 skipped by design: live/memory gates).
- Phase-27 added: 30 matrix rows (reliability 11, media 3, crypto 7,
  migration 5 + CLI battery 1, memory 2, e2e battery 1) — matrix total
  101 rows; live validation remains BLOCKED_NO_LIVE_ACCOUNT (1 row).
- A blocked/skipped/unexecuted test is never counted as PASS anywhere
  in these artifacts.
