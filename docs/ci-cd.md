# CI/CD (Phase 21)

Project identity: **SuroMesh** · module `github.com/Skryldev/suromesh` ·
repository `github.com/Skryldev/suromesh`.

## Workflow inventory

### `.github/workflows/build.yml` — cross-platform Go build (PRESENT)

Triggers: `push`, `pull_request`. Least privilege (`permissions:
contents: read`). Two jobs:

1. **gate** — runs on ubuntu/windows/macos-latest with Go `1.26.x`
   (consistent with `go.mod`): module tidy/verify, build, vet, gofmt
   check (`gofmt -l .` must be empty), full tests, race, fuzz seed
   corpora, generator `-verify`.
2. **cross-build** — matrix over windows/amd64, linux/amd64,
   linux/arm64, darwin/amd64, darwin/arm64 with `CGO_ENABLED=0`:
   cross-vet + trimmed example binaries, uploaded as GitHub Actions
   artifacts (14-day retention).

Only official actions are used, deliberately version-pinned:
`actions/checkout@v4`, `actions/setup-go@v5`,
`actions/upload-artifact@v4`. No third-party actions, no external
services.

## PyPI workflow — NOT CREATED (inspection outcome, Phase 21 §18)

The repository was inspected before designing the workflow, as the
phase requires. Findings:

- No `pyproject.toml`, `setup.py`, `setup.cfg`, or Python package
  layout exists anywhere in the project tree.
- The only Python file is `tools/vectors/generate.py` — a maintainer
  tool that executes the (local-only, git-excluded) reference
  implementation to regenerate golden vectors. It is not a package and
  has no consumers.
- SuroMesh is a Go module. There are no Go→Python bindings and no
  native component intended for Python distribution.

Per §18 ("Do NOT invent a Python package if the repository does not
contain one") and the §30 stop condition ("PyPI package structure does
not actually exist"), `pypi.yml` was **not created**. Creating it would
fabricate a capability the repository does not have.

**Owner path if a Python distribution is later desired**: create the
package (bindings or CLI) as its own reviewed change, add
`pyproject.toml` with a pinned build backend, then add a workflow that
builds sdist+wheel and uploads them as artifacts; publication stays
owner-controlled (`twine`/`pypa/gh-action-pypi-publish` with a manual
gate). None of that exists today and none of it was invented.

## Docker workflow — NOT CREATED (inspection outcome, Phase 21 §19)

Findings:

- No `Dockerfile`, `docker/`, or container configuration exists.
- The module is a **library**: `go list ./...` reports no production
  `main` package. The only `main` packages are the five demo examples
   (fake-server demos, not a runtime) and the generator CLI.
- There is no server, daemon, or CLI binary whose runtime an image
  should carry; packaging the reference tree or test fakes into an
  image would violate the artifact policy (docs/release-artifacts.md).

Per §19 ("If a Docker image is appropriate…") an image is not
appropriate — there is nothing to run — and per §30 ("Docker packaging
assumptions are unclear") the workflow was **not created**.

**Owner path if a runtime binary is later added**: add the `main`
package as its own reviewed change, then a multi-stage `Dockerfile`
(static build, distroless/scratch-alpine runtime, non-root, OCI labels
with version/commit/source) and a Buildx workflow building
linux/amd64+arm64, exporting artifacts without pushing. Publication
remains owner-controlled.

## Live-test separation (§21)

No workflow executes live Soroush Plus tests. The live harness
(`tests/live/`) fails closed: it requires `SP_SDK_LIVE=1` **and** an
explicit `SP_LIVE_AREAS` list plus operator-supplied credentials via
environment/secret files — none of which CI provides. CI runners have
no credentials, so even `-run TestLive` would skip before any network
I/O. If the owner later wants a manual live workflow, it must use
`workflow_dispatch` with repository secrets mapped to the documented
environment variables and must never echo them (no `set -x`). See
tests/live/README.md.

## Secret handling in CI (§20)

Workflows contain no phone numbers, OTPs, passwords, auth keys, session
files, or API hashes, and never print `secrets.*`. `.gitignore` blocks
`*.session`, `.env*`, `*.pem`, `*.key`, and evidence JSONL from ever
entering the repository. The one `api_id/api_hash` pair in the codebase
is the upstream client's public built-in default (docs/security.md
classification), not a credential.

## Release procedure relationship

CI proves repeatability; the deterministic release procedure and the
mandatory live gate live in docs/release-process.md. Publication of any
artifact (Go module tag, future PyPI package, future image) is
owner-controlled and never automatic.
