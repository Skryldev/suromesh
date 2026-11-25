# Container Security Results — Phase 27

## Runtime image (`suromesh-service:phase27-e2e`)

| Property | Value | Verification |
|---|---|---|
| Base | **scratch** (no OS userspace at all) | Dockerfile runtime stage has 2 COPY instructions |
| Size | **31.7 MB** (binary + CA bundle only) | `docker image ls` |
| User | `65532:65532` (numeric nonroot; matches k8s securityContext) | `docker image inspect --format '{{.Config.User}}'` |
| Shell | **absent** (no interpreter in image) | `docker run --entrypoint /bin/sh <img>` fails |
| Privileged | no (compose/k8s both unset) | manifest review + compose config |
| Root filesystem | read-only compatible (service writes nothing; compose `read_only: true` + tmpfs /tmp in e2e) | compose service def |
| Capabilities | k8s: `drop: [ALL]`; compose: docker default (no caps added) | manifests |
| Host mounts | none; no Docker socket anywhere in any compose file | grep over `service/deploy/**` |
| Embedded credentials | none (secrets via Compose secrets / k8s Secret; KEK never baked) | Dockerfile + compose review |
| Secret values in layers | none (KEK mounted at runtime, not copied; .e2e-kek.bin git-ignored + per-run generated) | security.sh secret-scan step |
| Dev tools in runtime | none (scratch) | image construction |
| CA certificates | present (`/etc/ssl/certs/ca-certificates.crt`, SSL_CERT_FILE set) | Dockerfile |
| Source paths in binary | stripped (`-trimpath -ldflags "-s -w"`) | build flags |
| Logs | stdout/stderr only, JSON, redaction layer active | smoke + logging tests |

## Build/CI image (`suromesh-ci-runner:phase27`)

Ephemeral, socket-less, no host ports; runs with Docker-default caps on
an isolated per-run network. Runs as root deliberately (bind-mounted
checkout + toolchain installation); it never touches anything outside
the run's project network and bind mount. Pinned tool versions recorded
in `evidence/phase27/toolchain.txt` per run.

## Test database (`postgres:17-alpine`)

- tmpfs-only storage (`--no-sync`), throwaway credentials, internal
  network, no host ports, destroyed with the run (`down -v`) and between
  pipeline steps (`reset_state`). A leaked test credential grants access
  to nothing beyond the lifetime of one run.

## Scanners (executed inside containers; outputs preserved)

- `govulncheck` v1.10.0 (Go vuln DB over network) on BOTH modules —
  raw output: `evidence/phase27/govulncheck-service.txt`,
  `govulncheck-sdk.txt`. Findings status recorded in the pipeline log of
  the final run; any GOVULN-level finding fails the security step.
- Committed-secret scan (sk_ key patterns, PEM private-key blocks,
  crypto/ tree check) — no hits outside synthetic E2E strings.
- Envelope-crypto adversarial battery — 7/7 PASS (see
  `crypto-battery.log`): every truncation, every single-byte flip,
  hostile length fields, unknown KEK version, dead provider, rotation
  semantics — all fail closed with `domain.ErrCrypto`.
- `staticcheck` v0.8.2 — clean (3 pre-existing dead symbols removed).
- Image availability constraint recorded: gcr.io (distroless),
  ghcr.io, cgr.dev blocked by this environment's egress (403) —
  Trivy/Grype registry images were NOT runnable here; the practical
  compensating controls are the scratch image (attack surface ≈ 2 files)
  + govulncheck + the secret scan. A dedicated image scanner (Trivy)
  should be added to the pipeline once registry access allows it —
  recorded as an open hardening item, NOT reported as clean-by-default.

## Findings affecting container security fixed in Phase 27

- SEC-15 upload-handler panic (process crash) — fixed + regression tests.
- SEC-16 download worker leak on cancel — fixed + regression test.
- SEC-23 non-functional local deploy compose (host-side secret
  interpolation) — rewritten with env-var contract; `compose config`
  validated.
- SEC-24 (open) k8s manifest lacks TLS listener env — documented with
  exact fix; blocks K8S installs, not the container path.
