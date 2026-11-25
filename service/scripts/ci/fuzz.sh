#!/usr/bin/env bash
# fuzz.sh — fuzz seeds (already part of unit tests) plus BOUNDED fuzz
# campaigns on security-sensitive SDK parsers (Phase 27 §4.2 item 8, §15).
# Every campaign runs exactly FUZZTIME_SECONDS per target (default 15s);
# the exact durations are recorded in the evidence log. Unbounded fuzzing
# is explicitly forbidden in CI.
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

FUZZTIME="${FUZZTIME_SECONDS:-15}"
cd "${SRC_ROOT}"

LOG="${PHASE27_LOGS}/fuzz-campaigns.txt"
: > "${LOG}"
tool_versions >> "${LOG}"

# Security-sensitive targets (parsers/serializers/protocol boundaries).
TARGETS=(
  FuzzParsePhone          # auth boundary parsing
  FuzzAuthResponseDecode  # protocol response decoding
  FuzzMediaClassification # media type dispatch
  FuzzMessageEntities     # text entity offsets (UTF-16)
)

step_begin "fuzz-campaigns"
for target in "${TARGETS[@]}"; do
  pkg="$(grep -rl "^func ${target}(" client/ --include='*_test.go' | head -1 | xargs dirname)"
  echo "== ${target} (${pkg}) fuzztime=${FUZZTIME}s $(date -u +%H:%M:%S)" | tee -a "${LOG}"
  # Seed corpus first: deterministic check of all committed corpus entries.
  ( cd "${pkg}" && timeout 120 go test -count=1 -run "${target}" ./... ) | tee -a "${LOG}"
  # Bounded campaign.
  ( cd "${pkg}" && timeout "$((FUZZTIME + 60))" \
      go test -count=1 -run "^$" -fuzz "^${target}$" -fuzztime "${FUZZTIME}s" . ) | tee -a "${LOG}"
done
step_end "fuzz-campaigns" 0

echo "FUZZ: campaigns completed (bounded, see ${LOG})"
