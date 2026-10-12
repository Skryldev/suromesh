# Supply-Chain & Reproducibility Threat Model (Phase 19)

A lightweight, stdlib-native model. No third-party security tooling is
introduced — the project's zero-dependency architecture IS the primary
control (nothing to fetch, nothing to trust beyond the toolchain).

## Assets to protect

A. The committed generated tree (`tl/gen`) — consumers compile it.
B. The generator (`internal/tlgen`, `cmd/tlgen`) — defines A.
C. The TL schema (`tl/schema/*.tl`) — defines B's input.
D. The reference tree (`reference/SPlusthon`) — the behavioral oracle.
E. Golden vectors (`tests/vectors/*.json`) — define "correct".
F. Credentials of any operator running the live harness.
G. Release artifacts.

## Threats and controls

| # | Threat | Likelihood | Impact | Controls (existing) |
|---|---|---|---|---|
| T1 | Compromised/altered schema input changes protocol behavior | low | high | Schema is byte-identical to the reference's data files (`cmp`-verified, Phase 19); generator verification (`-verify`) fails the gate on any drift; schema is frozen by phase rules |
| T2 | Hand-edited generated code (silent divergence from generator) | medium | high | `-verify` regenerates in memory and byte-compares; determinism proven (Phase 19 §2); release process step 3 blocks on any diff |
| T3 | Nondeterministic generation | low | high | Proven deterministic: independent clean-checkout generation is byte-identical; ordering is derived from schema parsing with explicit sort rules |
| T4 | Unexpected dependency introduction | low–med | high | `go.mod` has ZERO requirements; `go mod tidy` no-op + `go mod verify` are release-gate steps; any new require line is visible in review and would be a policy violation for this project's stdlib-only posture |
| T5 | Malicious test fixture (e.g. hostile vector JSON) | low | med | Fuzzing on every parsing boundary; fixtures are committed, diffable data with documented provenance (executed reference); live harness fails closed and never executes fixture content as code |
| T6 | Credential leakage via harness artifacts | med | high | Env-only secrets; redactor scrubs memory + disk (STATIC-tested); evidence carries hashes/counts only; `SP_LIVE_KEEP` defaults to cleanup; session files removed post-run |
| T7 | Release artifact contamination (reference/, test logs, sessions in the published tree) | med | med | docs/release-artifacts.md classification; release step 12 artifact scan; `reference/` is outside the module import graph and MUST-NOT-RELEASE |
| T8 | Local build contamination (stale artifacts changing behavior) | low | med | No build artifacts exist in the tree (scan: zero); clean-checkout proof shows the tree alone is sufficient; Go builds are hermetic w.r.t. $GOPATH for stdlib-only modules |
| T9 | Mutable external tooling (generator run with a doctored toolchain) | low | high | Generator is stdlib-only Go; `-verify` compares against the COMMITTED tree, so a doctored run on a clean checkout cannot silently diverge without producing a diff; VCS history (once created) pins everything |
| T10 | Oracle poisoning (reference tree modified to match a Go bug) | low | high | Reference tree is a separate git repository with 0 modified files verified EVERY phase gate (`git -C reference status --porcelain`); docs/licensing.md records its role |

## Residual risks (accepted, documented)

- The project root has **no VCS** yet: until committed, tamper-evidence
  for A–C, E–G relies on the copy-based clean-checkout proof rather than
  cryptographic history. Owner action required (docs/reproducibility.md).
- The module path (`suromesh`) is placeholder-grade; until the final
  path is claimed, a squatter could publish it. Do not publish anything
  before the migration (docs/versioning.md procedure).
- staticcheck (external tool) is advisory; its single finding is in the
  frozen layer and is classified, not suppressed.

## What this project deliberately does NOT do

- No dependency scanning tooling (nothing to scan — zero deps).
- No SLSA/provenance frameworks (no publishing pipeline yet; the release
  process document is the control until then).
- No code-signing (owner decision at publish time).
