# Docker-Native CI/CD Operations (Phase 27)

The official CI/CD execution path for SuroMesh is **Docker-native**: every
validation step runs inside containers orchestrated by Docker Compose. The
host needs only **Docker Engine + Compose + bash** (or GNU make for the
thin delegating targets). There is **no dependency on GitHub Actions, any
hosted runner, a local Go toolchain, or a locally installed PostgreSQL**.

Existing `.github/workflows/*` files belong to earlier phases and are NOT
part of this pipeline; they were inspected and reported in the Phase 27
audit (`service/docs/audit/evidence/phase27/`), and remain untouched.

## Architecture

```
host (docker + bash)
 └─ scripts/ci.sh <step>                  # orchestrator; per-run compose project
     └─ docker compose -p suromesh-ci-<runid> -f service/deploy/compose/ci.compose.yml
         ├─ ci            (suromesh-ci-runner:phase27)  — Go toolchain + pinned tools
         ├─ postgres-test (postgres:17-alpine)          — ephemeral per-run database
         └─ service       (suromesh-service:*)          — production image under test
```

Profiles map to pipeline steps:

| Step (`scripts/ci.sh X`) | Compose profile | What runs |
|---|---|---|
| `test` | `test` | gofmt, go mod integrity, build, vet, staticcheck, SDK build/vet, domain boundary, buf lint, codegen drift, buf breaking, unit tests (JUnit), bounded fuzz campaigns |
| `race` | `race` | SDK + service race-detector suites (JUnit) |
| `integration` | `integration` | persistence integration tests + migration Go scenarios vs real PostgreSQL (JUnit) |
| `migration` | `migration` | Go scenarios + migrate-CLI fresh-up/status/idempotency/schema/4-process race |
| `e2e` | `e2e` | production image build + service container boot + wire-level gRPC battery (e2eclient) |
| `security` | `security` | govulncheck (service+SDK), envelope-crypto adversarial battery, authn/authz battery, config fail-closed, committed-secret scan |
| `build` | `e2e` | production image build (multi-stage distroless) |
| `smoke` | `e2e` | container runtime qualification: non-root, no shell, stdout logs, graceful SIGTERM shutdown |
| `release` | `release` | release policy (strict replace rejection), module separation, vector integrity, docs presence |
| `ci` | all | the FULL qualification pipeline (fail-fast, one log) |
| `clean` | — | remove leftover `suromesh-ci-*` projects and ephemeral state |

Same steps via make: `make ci`, `make test`, `make race`, `make integration`,
`make migration`, `make e2e`, `make security`, `make build`, `make smoke`,
`make release`, `make clean`.

## Reproducibility rules

- **Clean checkout is sufficient**: no host Go, no host PostgreSQL, no
  manual seed steps. `git clone … && ./scripts/ci.sh ci` is the whole recipe.
- **Caches are external named volumes** (`suromesh-ci-gomod`,
  `suromesh-ci-gobuild`) shared across runs; Go caches are concurrency-safe.
  Delete them for a cold build: `docker volume rm suromesh-ci-gomod suromesh-ci-gobuild`.
- **Isolation**: every run uses its own Compose project
  (`suromesh-ci-<timestamp>-<pid>`): isolated network, containers, and
  ephemeral postgres state. A failed run cannot contaminate the next one;
  `compose down -v` runs on exit (trap) regardless of failure.
- **PostgreSQL is ephemeral**: `postgres-test` stores data in a tmpfs
  inside the container layer — destroyed with the run. No persistent test
  volume exists, so no ambiguous leftover database state is possible.
- **No hardcoded secrets**: the postgres credentials are throwaway
  test-only values on an isolated internal network (never published on
  host ports). The E2E bootstrap key and KEK file are generated from
  `/dev/urandom` per run and wiped on cleanup (`.e2e-kek.bin` is
  git-ignored and never committed).
- **Toolchain pinning** (see `service/docker/Dockerfile.ci`): Go 1.26
  (matches `go.mod`), buf v1.75.0, protoc-gen-go v1.36.5 +
  protoc-gen-go-grpc v1.5.1 (exact versions of the committed generated-code
  headers — codegen drift checks are meaningful only under this rule),
  staticcheck v0.8.2, govulncheck v1.10.0, gotestsum v1.13.0.
- **Timeouts everywhere**: every test/campaign/scan is wrapped in explicit
  `timeout` budgets; fuzz campaigns run exactly `FUZZTIME_SECONDS` (15s)
  per target, recorded in the evidence log. Nothing runs unbounded.

### Image update procedure

1. Bump the tool version ARG/tag in `service/docker/Dockerfile.ci`
   (and re-verify protoc generator versions against the committed
   `service/gen/**` headers — they must match exactly).
2. `docker compose -f service/deploy/compose/ci.compose.yml build ci`
3. Re-run `./scripts/ci.sh test` and record the new toolchain identity
   (the gates step prints it into `evidence/phase27/toolchain.txt`).
4. For the postgres image: bump `postgres-test.image` only together with
   a full `./scripts/ci.sh migration` + `integration` run (schema
   behavior can differ across major versions).

## Inspecting failures

- Full pipeline log (every step, streamed): `service/docs/audit/evidence/phase27/ci-run.log`
- Structured test results (JUnit XML): `service/docs/audit/evidence/phase27/junit/*.xml`
- Step outputs (fuzz, migration CLI, crypto battery, e2e lines):
  `service/docs/audit/evidence/phase27/*.txt|*.log`
- Containers of a (still running) failed run:
  `docker compose -p suromesh-ci-<runid> -f service/deploy/compose/ci.compose.yml ps -a`
  then `docker compose -p … logs <service>`.
- Remove everything from an aborted run: `./scripts/ci.sh clean`
  (or `docker compose -p suromesh-ci-<runid> down -v --remove-orphans`).

Exit-code contract: every mandatory check propagates its real exit code —
no pipelines without `pipefail`, no `|| true` on mandatory checks, no
converting failures into skips. Genuinely environment-blocked checks (e.g.
live Soroush Plus validation) are NOT in the default pipeline at all; they
are documented as operator opt-ins in `docs/testing-strategy.md`.

## Migration operations (operator cheat sheet)

```bash
# 1. Start an isolated PostgreSQL (dev database; NOT production):
docker compose -f service/deploy/compose/ci.compose.yml --profile migration \
  up -d postgres-test

# 2. Apply migrations (idempotent, advisory-locked):
docker compose -f service/deploy/compose/ci.compose.yml --profile migration \
  run --rm ci bash -c \
  'go build -o /tmp/migrate ./cmd/migrate && \
   SUROMESH_STORAGE_POSTGRES_DSN="$TEST_DATABASE_DSN" /tmp/migrate up'

# 3. Check migration status:
docker compose -f service/deploy/compose/ci.compose.yml --profile migration \
  run --rm ci bash -c \
  'SUROMESH_STORAGE_POSTGRES_DSN="$TEST_DATABASE_DSN" /tmp/migrate status'

# 4. Inspect the resulting schema:
docker compose -f service/deploy/compose/ci.compose.yml --profile migration \
  run --rm ci psql "$TEST_DATABASE_DSN" -c '\dt'

# 5. Full migration qualification (scenarios A–E + CLI race):
./scripts/ci.sh migration

# 6. Reset the disposable test database (safe: ephemeral by design):
docker compose -f service/deploy/compose/ci.compose.yml --profile migration \
  down --volumes --remove-orphans
```

There is no `down`/destructive migration path — the runner is
**roll-forward only** (recovery from a failed migration = fix the migration
and re-run; scenario C in `migration_scenarios_test.go` pins the rollback +
recovery semantics). Production reset/destructive operations are
deliberately absent from every default command.
