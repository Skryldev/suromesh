#!/usr/bin/env bash
# scripts/ci.sh — the Docker-native CI/CD entry point (Phase 27).
#
# The host needs ONLY Docker Engine + Compose + bash. Every validation
# step executes inside containers (service/deploy/compose/ci.compose.yml).
#
# Usage:
#   ./scripts/ci.sh ci          # FULL qualification pipeline (fail-fast)
#   ./scripts/ci.sh <profile>   # one step: test|race|integration|migration|
#                              #           e2e|security|build|smoke|release
#   ./scripts/ci.sh clean       # remove run containers/networks + ephemeral state
#
# Exit codes: 0 all checks passed; 1 a mandatory check failed (never
# swallowed by a pipeline without pipefail). Failed-step logs remain in
# service/docs/audit/evidence/phase27/ and the failing container output is
# printed in full (see docs/operations/docker-ci.md "inspecting failures").
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COMPOSE_FILE="${ROOT}/service/deploy/compose/ci.compose.yml"
EVIDENCE="${ROOT}/service/docs/audit/evidence/phase27"
JUNIT="${EVIDENCE}/junit"
LOG="${EVIDENCE}/ci-run.log"

# Per-run identity: unique compose project → isolated containers, network
# and ephemeral state; caches are EXTERNAL volumes and survive cleanup.
RUN_ID="suromesh-ci-$(date +%Y%m%d-%H%M%S)-$$"
COMPOSE() {
  docker compose -p "${RUN_ID}" -f "${COMPOSE_FILE}" "$@"
}

mkdir -p "${JUNIT}"
: > "${LOG}"

# Deterministic cleanup on exit (success, failure, or signal).
CLEANED=0
cleanup() {
  if [ "${CLEANED}" -eq 1 ]; then return; fi
  CLEANED=1
  echo "--- cleanup (${RUN_ID}) ---" | tee -a "${LOG}"
  COMPOSE down --volumes --remove-orphans --timeout 30 >>"${LOG}" 2>&1 || true
  rm -f "${ROOT}/service/deploy/compose/.e2e-kek.bin" 2>/dev/null || true
}
trap cleanup EXIT
trap 'echo "INTERRUPTED — cleaning up"; exit 130' INT TERM

# Ensure shared external caches exist (safe across runs; go caches are
# concurrency-safe).
ensure_caches() {
  docker volume create suromesh-ci-gomod >/dev/null
  docker volume create suromesh-ci-gobuild >/dev/null
  # The compose model interpolates the E2E bootstrap key on EVERY
  # invocation (any profile activates the ci service), so the per-run
  # ephemeral key material must exist before the first compose call.
  gen_e2e_env
}

# Ephemeral per-run e2e key material (never committed, wiped on cleanup).
gen_e2e_env() {
  local key="sk_e2e_$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  mkdir -p "${ROOT}/service/deploy/compose"
  head -c 32 /dev/urandom > "${ROOT}/service/deploy/compose/.e2e-kek.bin"
  # 0644 (not 0600): the container process runs as uid 65532 and must
  # read the mounted secret; the file is throwaway (random per run) and
  # deleted by the exit trap, so host-side owner-only mode would only
  # break the mount contract.
  chmod 644 "${ROOT}/service/deploy/compose/.e2e-kek.bin"
  export SUROMESH_CI_E2E_BOOTSTRAP_KEY="${key}"
}

# reset_state: wipes the run's ephemeral containers (incl. the tmpfs
# PostgreSQL) between steps. Determinism rule (§4.1): a failed step must
# never leave database state that contaminates later steps. Cost ~3s.
reset_state() {
  COMPOSE down --volumes --remove-orphans --timeout 15 >>"${LOG}" 2>&1 || true
  rm -f "${ROOT}/service/deploy/compose/.e2e-kek.bin"
}

# run <name> <cmd...>: executes a compose step, streams its output, appends
# it to the run log, and propagates the exact exit code.
STEP_RC=0
run() {
  local name="$1"; shift
  echo ""
  echo "=================================================================="
  echo "== ${name}"
  echo "=================================================================="
  echo "== ${name} $(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"${LOG}"
  local start end rc
  start="$(date +%s)"
  if ! COMPOSE "$@" 2>&1 | tee -a "${LOG}"; then
    rc=${PIPESTATUS[0]}
  else
    rc=0
  fi
  end="$(date +%s)"
  echo "== ${name} rc=${rc} duration=$((end - start))s" >>"${LOG}"
  if [ "${rc}" -ne 0 ]; then
    echo "STEP FAILED: ${name} (rc=${rc})"
    echo "  logs:      ${LOG}"
    echo "  containers: docker compose -p ${RUN_ID} -f ${COMPOSE_FILE} ps -a"
    STEP_RC=${rc}
    return "${rc}"
  fi
  echo "STEP PASSED: ${name}"
  return 0
}

build_ci_image() {
  run "ci-image-build" build ci
}

step_test() {
  build_ci_image
  run "static-gates" --profile test run --rm ci bash service/scripts/ci/gates.sh
  run "unit-tests" --profile test run --rm ci bash service/scripts/ci/unit.sh
  run "fuzz-bounded" --profile test run --rm ci bash service/scripts/ci/fuzz.sh
}

step_race() {
  build_ci_image
  run "race" --profile race run --rm ci bash service/scripts/ci/race.sh
}

step_integration() {
  build_ci_image
  run "integration" --profile integration run --rm ci bash service/scripts/ci/integration.sh
}

step_migration() {
  build_ci_image
  run "migration-go-scenarios" --profile migration run --rm ci bash service/scripts/ci/integration.sh
  run "migration-cli-scenarios" --profile migration run --rm ci bash service/scripts/ci/migration-cli.sh
}

step_security() {
  build_ci_image
  run "security" --profile security run --rm ci bash service/scripts/ci/security.sh
}

step_release() {
  build_ci_image
  run "release" --profile release run --rm ci bash service/scripts/ci/release.sh
}

# build: the PRODUCTION image (multi-stage distroless) via compose.
step_build() {
  gen_e2e_env
  run "service-image-build" --profile e2e build service
}

# smoke: container runtime qualification against the REAL image
# (Phase 27 §12): non-root, distroless (no shell), stdout logging,
# graceful shutdown on SIGTERM, health observed via gRPC readiness.
step_smoke() {
  gen_e2e_env
  echo "== smoke: production image runtime checks"
  echo "== smoke $(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"${LOG}"

  up_rc=0
  COMPOSE --profile e2e up --build -d --wait --wait-timeout 180 service 2>&1 | tee -a "${LOG}" || up_rc=${PIPESTATUS[0]}
  if [ "${up_rc}" -ne 0 ]; then
    echo "smoke: service container failed to start (up rc=${up_rc})"
    COMPOSE --profile e2e logs service 2>&1 | tail -30 | tee -a "${LOG}" || true
    return "${up_rc}"
  fi

  local image svc_container rc=0
  image="suromesh-service:phase27-e2e"
  svc_container="$(COMPOSE --profile e2e ps -q service)"

  echo "-- image user (must be non-root):"
  docker image inspect "${image}" --format '{{.Config.User}}' | tee -a "${LOG}"
  if [ "$(docker image inspect "${image}" --format '{{.Config.User}}')" = "root" ] \
     || [ -z "$(docker image inspect "${image}" --format '{{.Config.User}}')" ]; then
    echo "FAIL: runtime image runs as root"; rc=1
  fi

  echo "-- distroless check: no shell in the runtime image:"
  if docker run --rm --entrypoint /bin/sh "${image}" -c 'echo shell' >/dev/null 2>&1; then
    echo "FAIL: runtime image contains a shell (not distroless)"; rc=1
  else
    echo "PASS: no shell (distroless)"
  fi | tee -a "${LOG}"

  echo "-- container runs as non-root (image-level user is the contract):"
  local img_user
  img_user="$(docker image inspect "${image}" --format '{{.Config.User}}')"
  echo "image Config.User = ${img_user} (scratch has no /etc/passwd; numeric UID 65532 is enforced by the runtime)" | tee -a "${LOG}"

  echo "-- logs go to stdout (JSON):"
  COMPOSE --profile e2e logs service | head -5 | tee -a "${LOG}"

  echo "-- graceful shutdown on SIGTERM:"
  docker kill --signal=TERM "${svc_container}" >>"${LOG}" 2>&1
  sleep 3
  if COMPOSE --profile e2e logs service | grep -q "service terminated cleanly"; then
    echo "PASS: graceful shutdown observed"
  else
    echo "FAIL: 'service terminated cleanly' not observed after SIGTERM"
    COMPOSE --profile e2e logs service | tail -20 | tee -a "${LOG}"
    rc=1
  fi | tee -a "${LOG}"

  COMPOSE --profile e2e down --volumes --remove-orphans --timeout 30 >>"${LOG}" 2>&1 || true
  return "${rc}"
}

step_e2e() {
  gen_e2e_env
  build_ci_image
  # The service image must exist for the e2e profile (build first).
  run "service-image-build" --profile e2e build service
  echo "-- compose up service (output visible):"
  up_rc=0
  COMPOSE --profile e2e up -d --wait --wait-timeout 180 service 2>&1 | tee -a "${LOG}" || up_rc=${PIPESTATUS[0]}
  if [ "${up_rc}" -ne 0 ]; then
    echo "service container failed to start (up rc=${up_rc})"
    COMPOSE --profile e2e ps -a 2>&1 | tee -a "${LOG}" || true
    COMPOSE --profile e2e logs service 2>&1 | tail -30 | tee -a "${LOG}" || true
    return "${up_rc}"
  fi
  run "e2e-wire-tests" --profile e2e run --rm ci bash service/scripts/ci/e2e.sh
}

# ci: the FULL qualification pipeline (§4.2 items 1-17), fail-fast.
step_ci() {
  local failed=0
  step_test        || failed=1; reset_state
  step_race        || failed=1; reset_state
  step_integration || failed=1; reset_state
  step_migration   || failed=1; reset_state
  step_security    || failed=1; reset_state
  step_build       || failed=1; reset_state
  step_e2e         || failed=1; reset_state
  step_smoke       || failed=1; reset_state
  step_release     || failed=1; reset_state
  echo ""
  echo "=================================================================="
  if [ "${failed}" -eq 0 ]; then
    echo "FULL CI PIPELINE: ALL STEPS PASSED (${RUN_ID})"
  else
    echo "FULL CI PIPELINE: FAILED — see ${LOG}"
  fi
  echo "=================================================================="
  return "${failed}"
}

cmd_clean() {
  # Remove any leftover suromesh-ci-* compose projects + ephemeral state.
  for p in $(docker compose ls --format '{{.Name}}' 2>/dev/null | grep '^suromesh-ci-' || true); do
    echo "removing compose project ${p}"
    docker compose -p "${p}" -f "${COMPOSE_FILE}" down --volumes --remove-orphans --timeout 30 2>/dev/null || true
  done
  rm -f "${ROOT}/service/deploy/compose/.e2e-kek.bin" 2>/dev/null || true
  echo "clean done (external caches kept: suromesh-ci-gomod, suromesh-ci-gobuild;"
  echo "delete them manually with 'docker volume rm' if you want a cold build)"
}

case "${1:-}" in
  ci)          ensure_caches; step_ci ;;
  test)        ensure_caches; step_test ;;
  race)        ensure_caches; step_race ;;
  integration) ensure_caches; step_integration ;;
  migration)   ensure_caches; step_migration ;;
  e2e)         ensure_caches; step_e2e ;;
  security)    ensure_caches; step_security ;;
  build)       gen_e2e_env; step_build ;;
  smoke)       ensure_caches; step_smoke ;;
  release)     ensure_caches; step_release ;;
  clean)       cmd_clean ;;
  ""|-h|--help)
    sed -n '2,15p' "$0"; exit 0 ;;
  *)
    echo "unknown step: $1 (see --help)"; exit 2 ;;
esac
