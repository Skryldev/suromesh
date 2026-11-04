# Reproducibility (Phase 19)

Everything needed to build, test, and verify this project comes from the
repository tree itself. This document distinguishes repository inputs,
generated artifacts, developer tools, test-only infrastructure,
environment-dependent behavior, and release-only behavior, and records
the executable evidence collected in Phase 19.

## Clean-checkout proof (executed, Phase 19)

A byte copy of the repository tree (excluding `reference/.git`) was made
to a scratch directory and the complete gate battery was run **there**:

```text
go build ./...        OK
go vet ./...          OK
gofmt -l .            0 files
go mod tidy           no-op
go mod verify         all modules verified
go test -count=1 ./...   17/17 packages ok
go run ./cmd/tlgen -verify   47 files / 1684 constructors / LAYER 182, current
reference tree        0 modified files
go test -race -count=1 ./...   17/17 ok × 3 consecutive runs, zero races
```

No file outside the tree was consulted; no environment variables are
required for any of the above (the live harness is opt-in and skips
without configuration by design).

## Repository inputs (source-controlled, authoritative)

| Input | Role |
|---|---|
| `tl/schema/api.tl`, `tl/schema/mtproto.tl` | The TL schema (Layer 182). Byte-identical to the reference generator's data files — provenance documented in docs/licensing.md. |
| `internal/tlgen`, `cmd/tlgen` | The generator (library + CLI). Source-controlled; output is committed. |
| `tl/gen/**` (47 files) | **Generated output, committed intentionally.** Rule: consumers must never need the generator to build; the committed output is verified current by `go run ./cmd/tlgen -verify` in every gate. |
| `client`, `session`, `rpc`, `mtproto`, `crypto`, `transport`, `auth`, `tl` | Production source. |
| `tests/**`, `internal/wstest`, `tools/vectors` | Test infrastructure, incl. the live harness (opt-in) and the vector generator that executes the Python reference. |
| `examples/**` | Runnable documentation. |
| `docs/**` | Contracts, audits, phase reports. |
| `go.mod` | `module github.com/Skryldev/suromesh`, `go 1.26`, **zero requirements** (stdlib-only; no go.sum exists). |

## Generated artifacts

Only one class exists: `tl/gen`. It is committed on purpose (§1 rule:
prove the artifact is intentionally source-controlled — it is; the
generator is dev tooling and is NOT needed by consumers). Determinism is
proven in Phase 19: fresh generation in a clean checkout produced
**byte-identical** output (`diff -r` of both `tl/gen` trees: identical;
47 files, 3,545,712 bytes, 1,684 constructors).

## Developer tools (not consumed by builds or tests)

`go run ./cmd/tlgen` (verification/generation), `staticcheck` (external,
optional — one documented frozen finding), and the Python interpreter for
`tools/vectors/generate.py` (only when regenerating golden vectors; the
committed JSON is authoritative otherwise).

## Test-only infrastructure

`internal/wstest` (in-process MTProto server double), `tests/live` (the
opt-in live harness — never runs without `SP_SDK_LIVE=1` + a named area),
and all `*_test.go` files. None are importable by external consumers
(toolchain-enforced `internal/` rule; verified by a negative compile
probe in Phase 19 §8).

## Environment-dependent behavior

- `mtproto.Rand` and `crypto/rsakeys.ProductionRand` default to
  `crypto/rand`/stdlib randomness; tests may swap them for determinism.
  Production builds need no environment configuration.
- `tests/live/**` reads operator configuration exclusively from
  environment variables (docs: tests/live/README.md) and fails closed.
- No timestamps, machine paths, user names, or absolute paths are
  embedded in source or generated output (verified by scanning for the
  build directory name in all committed files: zero hits outside
  reference/).

## Release-only behavior

Version stamping, module-path migration, artifact selection, and
publishing steps are defined in docs/release-process.md; none are
triggered by ordinary builds or tests.

## Known gaps (release prerequisites, not build defects)

1. **The project root is not under version control.** Only
   `reference/SPlusthon` is a git repository. Until the source tree is
   committed to VCS, "repository state" has no tamper-evident history and
   no tag exists to reproduce. This is an owner action; the clean-copy
   proof above demonstrates the tree is self-contained meanwhile.
2. **License unresolved** — see docs/licensing.md.
