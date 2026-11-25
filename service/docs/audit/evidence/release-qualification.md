# Release Qualification — Phase 27

Executed: 2026-11-24 · Docker-native pipeline (`./scripts/ci.sh ci`) ·
evidence: `phase27/ci-run.log` + `phase27/junit/*.xml` + per-step logs.

## Verdict: QUALIFIED WITH DOCUMENTED RESTRICTIONS

Qualified for controlled staging. Production entry remains gated on the
four documented restrictions (live Soroush Plus validation, KMS vendor
decision, OTel instrumentation, K8S TLS+migration-job fixes) — see the
report §21/§22.

## Completion criteria (§18) — final accounting

| # | Criterion | Status |
|---|---|---|
| 1 | Docker-native test orchestration implemented | DONE (`scripts/ci.sh`, compose profiles, Makefile) |
| 2 | Full workflow runs from a clean checkout | DONE (host needs only Docker+Compose+bash) |
| 3 | PostgreSQL integration tests on real Dockerized PG | DONE (11 tests + race) |
| 4 | Fresh-install migrations validated | DONE (Scenario A) |
| 5 | Migration idempotency + failure behavior tested | DONE (Scenarios B, C) |
| 6 | Existing DB upgrade validated | DONE (Scenario D) |
| 7 | Concurrent migration tested/constrained | DONE (Scenarios E + 4-process CLI race; advisory lock) |
| 8 | SDK-service conformance accounting reconciled | DONE (73 methods, 33 rows; discrepancy explained) |
| 9 | Critical AccountRunner lifecycle/concurrency tested | DONE (11 contract tests + fixes) |
| 10 | Container security assessed with actual evidence | DONE (scratch/nonroot/no-shell + scans; image-scanner noted as blocked-by-egress) |
| 11 | Production image runs in Docker | DONE (31.7 MB scratch image; e2e+smoke) |
| 12 | gRPC smoke tests against the actual service container | DONE (13 wire checks, ALL PASSED) |
| 13 | Docker-based test results + evidence generated | DONE (JUnit + logs in evidence/phase27) |
| 14 | Persian HTML report generated and validated | DONE (`fa/phase-27-release-qualification.html`) |
| 15 | Remaining blockers documented honestly | DONE (report §21–22; SEC-28 BLOCKED_ENVIRONMENT) |
| 16 | No unapproved external deployment/publication | CONFIRMED (no push, no registry, no Pages, no live account) |

## Defects discovered and resolved this phase (10 functional + 1 doc)

| ID | Defect | Found by |
|---|---|---|
| SEC-15 | Upload handler: double close of completion channel → nondeterministic process crash | code audit + new worker-completion tests |
| SEC-16 | Download handler: worker goroutine + read slot leaked on client cancel | code audit + cancel-release test |
| SEC-17 | Concurrent StartRunner could double-create upstream clients | new 16-way race test |
| — | Runner.Start leaked the constructed client on post-construction failure | new leak test |
| — | Runner.Mutate enqueue/completion race could strand callers | new cancellation/shutdown tests |
| SEC-18 | Migrations had no cross-migrator serialization | Scenario E design + 4-process race |
| — | Global unique index on phone_hash contradicted tenant-scoped uniqueness | real-PostgreSQL run |
| — | operations.result_ref NULL scanned into non-nullable string | real-PostgreSQL run |
| — | CHAR(n) blank-padding broke replay comparisons | real-PostgreSQL run |
| SEC-27 | Bootstrap tenant registered by NAME but persistence scopes by UUID (every authed request would fail) | E2E wire battery |
| — | Envelope.ReWrap docstring contradicted implementation ("DEK-only") | code audit |

## Release-policy state

- `RELEASE_BUILD=0` tolerates the dev replace (documented in go.mod);
  `RELEASE_BUILD=1` rejects it (verified in the pipeline) — the removal
  procedure in service/go.mod remains the owner's publication-time step.
- Module separation (SDK never imports service): enforced every run.
- No version/tag/release was created; nothing published.

## Residual (non-blocking, documented)

- govulncheck BLOCKED_ENVIRONMENT (vuln.go.dev 403) — re-run required
  where reachable before any production release (SEC-28).
- Image scanner (Trivy/Grype) not runnable under this egress — recorded
  as an open hardening item, never counted as a clean scan.
- Live Soroush Plus validation BLOCKED_NO_LIVE_ACCOUNT (unchanged).
