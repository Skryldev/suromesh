# GitHub Actions Workflow Inventory (Phase 27 inspection — §14 policy)

Phase 27 policy: the official CI/CD path is Docker-native
(`scripts/ci.sh` + `service/deploy/compose/ci.compose.yml`). No GitHub
Actions workflow was created, modified, or used for this phase. The four
pre-existing workflows from earlier phases were INSPECTED and are
reported here verbatim-in-summary; they remain untouched.

| Workflow | Trigger | Purpose | Phase 27 assessment |
|---|---|---|---|
| `.github/workflows/build.yml` | push/PR | Cross-platform Go gate (ubuntu/windows/macos): tidy/verify, build, vet, gofmt, tests, race, fuzz seeds, generator -verify, artifact cross-builds | Redundant with the Docker-native pipeline; untouched. Its local-equivalent steps all run inside `scripts/ci.sh test/race` now. |
| `.github/workflows/service.yml` | push/PR (service/**) | Service-module gate: fmt, vet, tidy, tests, race, domain boundary, release policy (dev+strict), smoke | Redundant with Docker-native `test/race/release` steps; untouched. |
| `.github/workflows/docs.yml` | push/PR (website/tools) | Docs production build, link enforcement, TS check, example compilation | Docs-specific; outside Phase 27 scope; untouched. |
| `.github/workflows/docs-deploy.yml` | workflow_dispatch ONLY | Owner-controlled GitHub Pages deploy | Publication is owner-controlled; Phase 27 did not run it. |

Observations (no action taken):
- All four use least-privilege `permissions: contents: read` (deploy adds
  pages scopes) and official, version-pinned actions only.
- The live-test prohibition holds in all of them ("Live Soroush Plus
  tests NEVER run here").
- Divergence risk: the GitHub gates do not include staticcheck, buf
  lint/breaking, the migration scenarios, govulncheck, or the E2E wire
  battery — those exist only in the Docker-native pipeline. If the owner
  ever re-enables hosted CI, mirror the Docker steps or replace these
  files with thin `docker compose run` shims.

Confirmation: `git status` shows no modifications under
`.github/workflows/` from this phase; the Docker-native pipeline does
not depend on them.
