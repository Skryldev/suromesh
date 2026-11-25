#!/usr/bin/env bash
# release.sh — release qualification (Phase 27 §4.2 item 17):
#   - module release policy (replace directives rejected for release)
#   - SDK/service module separation (service must never be imported by SDK)
#   - documentation presence for the release bundle
#   - vector integrity (SDK golden vectors parse as JSON)
# Container-image validation lives in the smoke profile (scripts/ci.sh
# smoke — needs the Docker engine, not this container).
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

cd "${SRC_ROOT}"

step_begin "release-policy-dev"
( cd service && RELEASE_BUILD=0 bash scripts/check-release.sh ) | tee "${PHASE27_LOGS}/release-policy-dev.txt"
step_end "release-policy-dev" 0

step_begin "release-policy-strict-rejects-replace"
# The development replace MUST fail a strict release build (documents the
# removal procedure in service/go.mod).
if ( cd service && RELEASE_BUILD=1 bash scripts/check-release.sh ) >"${PHASE27_LOGS}/release-policy-strict.txt" 2>&1; then
  echo "strict release policy did NOT reject the replace directive"
  step_end "release-policy-strict-rejects-replace" 1; exit 1
fi
echo "strict mode rejects the development replace as required"
step_end "release-policy-strict-rejects-replace" 0

step_begin "module-separation"
# The SDK must never depend on the service (one-way dependency rule).
if grep -rn "Skryldev/suromesh/service" \
    --include='*.go' client/ session/ transport/ mtproto/ tl/ crypto/ auth/ rpc/ internal/ \
    2>/dev/null | grep -v _test.go | grep -q .; then
  echo "SDK imports the service module (boundary violation):"
  grep -rn "Skryldev/suromesh/service" --include='*.go' client/ session/ transport/ mtproto/ tl/ crypto/ auth/ rpc/ internal/ | grep -v _test.go
  step_end "module-separation" 1; exit 1
fi
echo "module separation verified (SDK never imports service)"
step_end "module-separation" 0

step_begin "vector-integrity"
# SDK golden vectors must be valid JSON (the generator regenerates them;
# CI verifies structural integrity offline).
for f in tests/vectors/*.json; do
  jq -e . "${f}" >/dev/null || { echo "invalid JSON: ${f}"; exit 1; }
done
echo "$(ls tests/vectors/*.json | wc -l) vector files structurally valid"
step_end "vector-integrity" 0

step_begin "release-docs-presence"
for f in service/README.md service/docs/operations/deployment.md \
         service/docs/operations/docker-ci.md service/docs/security/security-architecture.md; do
  [ -f "${SRC_ROOT}/${f}" ] || { echo "missing required doc: ${f}"; exit 1; }
done
step_end "release-docs-presence" 0

echo "RELEASE: ALL CHECKS PASSED"
