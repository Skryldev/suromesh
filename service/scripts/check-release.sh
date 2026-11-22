#!/usr/bin/env bash
# Release-policy check for the SuroMesh Service module (Phase 25 §1).
#
# Fails when service/go.mod contains a local replace directive. Release
# builds (phase S8, tagged builds) MUST run this with RELEASE_BUILD=1;
# development builds tolerate the isolated `replace ... => ../` directive
# documented in service/go.mod.
#
# Usage:
#   scripts/check-release.sh              # dev mode: warns, exits 0 on ../ replaces
#   RELEASE_BUILD=1 scripts/check-release.sh   # strict: any replace fails
set -euo pipefail

GOMOD="$(cd "$(dirname "$0")/.." && pwd)/go.mod"

if ! grep -Eq '^[[:space:]]*replace[[:space:]]' "$GOMOD"; then
  echo "release policy OK: no replace directives in $(dirname "$GOMOD")/go.mod"
  exit 0
fi

if [[ "${RELEASE_BUILD:-0}" == "1" ]]; then
  echo "RELEASE POLICY VIOLATION: replace directive(s) present in $GOMOD" >&2
  echo "Remove the development replace (see the comment in service/go.mod)" >&2
  exit 1
fi

echo "DEV OK: replace directive present (development only); RELEASE_BUILD=1 would fail"
exit 0
