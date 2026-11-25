# Audit Artifacts

- `fa/phase-27-release-qualification.html` — the Phase 27 release
  qualification report (standalone Persian, RTL, opens directly in a
  browser; structure/anchor/external-dependency validated).
- `fa/comprehensive-audit.html` — the Phase 26 comprehensive audit
  (historical).
- `evidence/` — machine-generated evidence from actual runs:
  - `command-results.md` — definitive pipeline run, exact commands, results
  - `test-matrix.csv` — every test with layer/status/live-dependency (101 rows)
  - `sdk-conformance-matrix.csv` — 33 capability rows (R01…R33) with
    corrected totals (19/10/1/1/2 = 33)
  - `sdk-method-inventory.csv` — all 73 exported SDK Client methods
    mapped exactly once (EXPOSED 33 / GROUPED 12 / ADAPTER-INTERNAL 4 /
    SDK-SUPPORT 15 / TEST 9)
  - `conformance-reconciliation.md` — the Phase-26 accounting
    discrepancy resolved (28-vs-33)
  - `error-mapping-matrix.csv` — SDK error → domain → gRPC status/reason
  - `security-findings.csv` — 28 findings (incl. 10 defects found and
    fixed in Phase 27)
  - `benchmark-results.txt` — bounded benchmarks with environment block
  - `migration-test-results.md` — scenarios A–F + CLI battery + the
    schema defects the real-PostgreSQL run exposed
  - `docker-validation.md` — images, isolation model, runtime checks
  - `container-security-results.md` — nonroot/scratch/no-shell + scans
  - `runtime-reliability-results.md` — AccountRunner contract tests +
    fixed races/leaks
  - `release-qualification.md` — §18 completion-criteria accounting +
    verdict
  - `phase27/` — raw run logs (ci-run.log, JUnit XML, fuzz/campaign
    logs, e2e results, toolchain identity, GitHub Actions inspection)

Docker-native pipeline: `scripts/ci.sh ci` (host needs only Docker +
Compose + bash). Operations guide:
`service/docs/operations/docker-ci.md`. All scripts are offline with
respect to Soroush Plus: live validation stays a separate, explicitly
opt-in operator runbook (BLOCKED_NO_LIVE_ACCOUNT in the matrix).
