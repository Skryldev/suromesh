# Phase 21 Audit — Release Preparation, Live Bootstrap & CI/CD

## Identity

```text
Project: SuroMesh
Repository: github.com/Skryldev/suromesh
Module: github.com/Skryldev/suromesh
License: MIT
```

Migration evidence: `go list -m` → `github.com/Skryldev/suromesh`; zero
`splusgo`/`SPlusGo` occurrences outside `reference/` (all classified
during migration: Go imports, docs, package comments, tests, examples,
CI — each rewritten; the persisted-FileSession format magic
`SPLUSGO1` → `SUROMESH` migrated as SuroMesh-owned format, same 8-byte
layout, zero external users; StringSession wire format untouched for
reference compatibility). `reference/SPlusthon` untouched (0 modified
files, verified).

## Live authentication

```text
AUTHENTICATION: BLOCKED
SESSION REUSE: BLOCKED
```

The authorized phone was never transmitted (the flow never reached
SendCode; no SMS/OTP was requested or consumed — see
docs/phase21-live-validation.md). Blocking cause: the service endpoint
became unreachably throttled after the initial validation attempts
(server closes every WebSocket upgrade mid-handshake); probing was
stopped per §7.

## Live validation

```text
CONNECT:            PARTIAL  (upgrade accepted once, 39.3 s; unstable after)
DH KEY EXCHANGE:    PASS     (live auth-key establishment)
AUTH:               BLOCKED
SESSION:            BLOCKED
PEERS:              BLOCKED
DIALOGS:            BLOCKED
HISTORY:            BLOCKED
UPDATES:            BLOCKED
MESSAGING:          BLOCKED
MUTATIONS:          BLOCKED
MEDIA:              BLOCKED
FILE REFERENCES:    BLOCKED
ALBUMS:             BLOCKED
RECONNECT:          BLOCKED
ERRORS:             BLOCKED
LONG-RUNNING:       BLOCKED
```

First-encrypted-RPC drop: recorded as INCONCLUSIVE (single occurrence;
differential analysis of salt/msg_id/seq_no/init-request against the
reference found no divergence; §22 forbids speculative patching).

## Engineering gates

```text
BUILD:               PASS
VET:                 PASS
GOFMT:               PASS (0 files)
TEST:                PASS (17/17 packages, twice)
RACE:                PASS (×3 consecutive, 17/17 each, zero races)
FUZZ:                PASS (46 seed campaigns, 17/17 packages)
STATICCHECK:         PASS-with-1-frozen (tl/gzip.go:38 SA4006, unchanged)
GENERATOR:           PASS (-verify current; write-mode byte-identical)
REFERENCE INTEGRITY: PASS (0 modified files; excluded from VCS)
API COMPATIBILITY:   PASS (73 Client methods, contract suite green)
EXTERNAL CONSUMER:   PASS (final module path; build+run OK;
                          internal import rejected by toolchain)
SECRET AUDIT:        PASS (no credentials in tree/workflows/evidence;
                          phone never entered the repository)
CLONE SIMULATION:    PASS (git-tracked files only → 17/17, reference-free)
```

## CI/CD

- **Cross-platform build workflow**: `.github/workflows/build.yml` —
  gate job (ubuntu/windows/macos: tidy/verify, build, vet, gofmt,
  tests, race, fuzz seeds, generator verify) + cross-build matrix
  (windows/amd64, linux/amd64+arm64, darwin/amd64+arm64, CGO_ENABLED=0,
  trimmed binaries, artifact upload). Official actions only
  (checkout@v4, setup-go@v5, upload-artifact@v4), `permissions:
  contents: read`, no secrets, no live tests.
- **PyPI workflow**: NOT CREATED — inspection found no Python package
  (no pyproject/setup.*; only `tools/vectors/generate.py`, a maintainer
  tool). §18 forbids inventing one; §30 lists this as a stop
  condition. Owner path documented in docs/ci-cd.md.
- **Docker workflow**: NOT CREATED — no Dockerfile, no container
  configuration, and no production `main` package (the module is a
  library; examples are fake-server demos). §19/§30 honored. Owner path
  documented in docs/ci-cd.md.
- **Security posture**: least privilege; no `set -x`; no secrets echo;
  live tests structurally absent from CI (fail-closed harness).
- **Artifact handling**: GitHub Actions artifacts, 14-day retention,
  no push/publish anywhere.

## Repository readiness for owner push

`git init` complete; 337 files staged; `.gitignore` excludes
`reference/` (GPL oracle stays local-only), `*.session`, `.env*`,
`*.pem`, `*.key`, evidence JSONL, coverage/build/editor artifacts.
`git status` shows no credentials or runtime state. The owner's flow:

```sh
git commit -m "SuroMesh release candidate"
git remote add origin https://github.com/Skryldev/suromesh.git
git push -u origin master
```

No remote, push, tag, or release was created by the agent. The
reference tree remains available locally (its own git repository,
untouched) for future differential work but will not be part of the
pushed repository.

## Bugs Found

### BUG-21-1 — schema drift test depended on the git-excluded reference tree

- **ID**: BUG-21-1
- **Symptom**: after excluding `reference/` from VCS (GPL
  MUST-NOT-RELEASE), the pushed-repository simulation failed
  `TestSchemaFilesMatchReference` (it read
  `reference/SPlusthon/.../api.tl` at test time).
- **Root cause**: the drift guard predates the VCS decision and coupled
  the test suite to the local-only oracle tree.
- **Layer**: test-infrastructure.
- **Fix**: committed provenance snapshots
  (`tests/testdata/schema_reference/{api,mtproto}.tl`, byte-identical
  to the reference data, cmp-verified) and repointed the test at them;
  intentional schema changes must now update both locations in one
  commit (documented in the test).
- **Regression test**: the reference-free clone gate — 17/17 packages
  green from git-tracked files alone.
- **Compatibility impact**: none.

### Live observations (not bugs; classifications per §21)

1. WS upgrade instability/throttling — LIVE_SERVICE_BEHAVIOR /
   ENVIRONMENT_FAILURE (probe accepted once in 39.3 s; later EOFs;
   probing stopped per §7).
2. First-encrypted-RPC connection drop — INCONCLUSIVE (one occurrence;
   no reference divergence found; no patch per §22).

### Test-harness adjustment (evidence-based, not a defect fix)

Live connect budget 30 s → 180 s in `tests/live` (measured link needs
~40 s for the WS handshake alone). Documented in code with the
measurement.

## Frozen Layer

```text
No frozen-layer changes. tl/gen byte-identical after write-mode
regeneration; tl/schema unchanged (cmp-identical to the committed
provenance snapshots AND the reference); reference/SPlusthon 0 modified
files; public API unchanged (73 methods; migration was
import-path-only); no protocol code was modified — the INCONCLUSIVE
live observation produced no patch, by design.
```

## Final verdict

```text
RELEASE QUALIFIED: BLOCKED
```

Exact blocker: **live validation incomplete** — the service endpoint
throttled the validation channel before an initialized session (and
therefore SendCode/auth and every downstream area) could run. The
identity, licensing, module-path, VCS, CI/CD, and engineering
objectives of Phase 21 are COMPLETE; the live runbook remains to be
executed when the endpoint admits the client again (see
docs/phase21-live-validation.md § next-session procedure).
