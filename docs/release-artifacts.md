# Release Artifacts (Phase 19)

The intended source distribution and its classification. "Distribution"
means whatever the owner publishes once the license question
(docs/licensing.md) and the live gate (Phase 18) are resolved.

## Classification

| Class | Meaning |
|---|---|
| RELEASED | Must be in the published distribution |
| SOURCE-ONLY | In a source distribution; not part of the consumer-facing module |
| DEVELOPMENT-ONLY | Maintainer tooling |
| TEST-ONLY | Test infrastructure; never consumer-facing |
| MUST-NOT-RELEASE | Must be excluded from any public artifact |

## Item-by-item

| Item | Class | Notes |
|---|---|---|
| `client/` (non-test) | RELEASED | The consumer surface. |
| `session/` (non-test) | RELEASED | Persistence surface. |
| `tl/` (non-test, incl. `tl/gen`) | RELEASED | Wire runtime + committed generated types; `tl/gen` is required to build and pinned by the generator gate. |
| `crypto/`, `crypto/rsakeys/`, `mtproto/`, `rpc/`, `transport/` (+`ws`, `abridged`), `auth/` | RELEASED | Composition layers (docs/package-boundaries.md); importable, documented, supported. |
| `go.mod` | RELEASED | Stdlib-only; module path migration is a pre-v1.0 owner action (docs/versioning.md). |
| `README.md` | RELEASED | States live-validation status honestly. |
| `examples/` | RELEASED | Runnable, credential-free; `examples/internal/demo` is internal to the examples tree and cannot leak into the consumer surface. |
| `docs/` | RELEASED (SOURCE-ONLY acceptable) | Contracts and audits; publishing them with the source is recommended. |
| `tests/**` (unit/compat/protocol/rpc/transport/auth) | SOURCE-ONLY | Evidence suites; valuable to auditors, not needed by consumers. |
| `tests/live/**` | SOURCE-ONLY | Opt-in live harness; contains no credentials by construction. |
| `internal/wstest/` | TEST-ONLY | Server double; toolchain-blocked from external import. |
| `internal/tlgen/`, `cmd/tlgen/` | DEVELOPMENT-ONLY | Generator library + CLI. |
| `tools/vectors/` | DEVELOPMENT-ONLY | Golden-vector generator (executes the Python reference). |
| `tests/vectors/*.json` | SOURCE-ONLY | Committed fixtures (data produced by executing the reference). |
| `reference/SPlusthon/**` | MUST-NOT-RELEASE | Third-party GPL-3.0 code, included for differential testing only. Excluding it from published artifacts also avoids redistribution questions until licensing is resolved (docs/licensing.md). |
| `*.session`, `*.jsonl` (evidence), `*.log`, coverage/benchmark outputs, editor metadata | MUST-NOT-RELEASE | None exist in the tree (Phase 19 §6/§15 scans: zero hits). |

## Rules

1. The consumer-facing module surface is exactly: `client`, `session`,
   `tl`, `tl/gen`, and the composition layers listed above. Everything
   else must not be required to build against the published module —
   proven by the external consumer simulation (Phase 19 §8).
2. `reference/` never ships: not in module zips (it is outside the
   module's import graph), not in source archives prepared for release.
3. Release preparation must run the secret scan and artifact scan from
   docs/release-process.md steps 8 and 12 before tagging.
