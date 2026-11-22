# Audit Artifacts (Phase 26)

- `fa/comprehensive-audit.html` — the full Persian audit report
  (standalone, RTL, opens directly in a browser). Validate with
  `bash scripts/audit/check-audit-report.sh`.
- `evidence/` — machine-generated evidence from actual runs:
  - `command-results.md` — exact commands and outputs
  - `test-matrix.csv` — every test with layer/status/live-dependency
  - `sdk-conformance-matrix.csv` — SDK capability → service RPC traceability
  - `error-mapping-matrix.csv` — SDK error → domain → gRPC status/reason
  - `security-findings.csv` — findings with severity/status
  - `benchmark-results.txt` — measured benchmark outputs

Scripts: `scripts/audit/run-audit.sh` (full offline battery),
`scripts/audit/verify-conformance.sh` (adapter conformance only),
`scripts/audit/check-audit-report.sh` (report validation).
All scripts are offline: they never contact the live Soroush Plus service.
