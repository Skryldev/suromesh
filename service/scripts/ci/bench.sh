#!/usr/bin/env bash
# bench.sh — bounded benchmark campaigns inside the CI container
# (Phase 27 §15). Every campaign uses -benchtime with an explicit bound;
# raw outputs are appended to evidence/phase27/benchmark-results.txt with
# the toolchain identity (environment reproducibility block).
set -euo pipefail
# shellcheck source=common.sh
source "$(dirname "$0")/common.sh"

OUT="${EVIDENCE}/benchmark-results.txt"
{
  echo "=== SuroMesh Phase 27 benchmarks — Docker-native run ==="
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  tool_versions
  echo "container cpu/mem limits: see compose ci.compose.yml (ci: mem 4g; cpu unpinned — host shares)"
  echo "host kernel: $(uname -sr), arch: $(uname -m)"
  echo
} > "${OUT}"

cd "${SERVICE_ROOT}"
step_begin "service-benchmarks"
# Bounded: 1s per benchmark (default -benchtime), 5-minute hard timeout.
timeout 300 go test -run '^$' -bench . -benchtime 1s -count=1 \
  ./internal/application/ ./internal/infrastructure/cryptography/ \
  | tee -a "${OUT}"
step_end "service-benchmarks" 0

cd "${SRC_ROOT}"
step_begin "sdk-benchmarks"
# Representative SDK hot paths, bounded per-benchmark (-benchtime 300ms)
# with a generous ceiling: the full client suite includes heavy update
# benchmarks that exceed short walls on shared CI hosts.
# The update-engine family (BenchmarkUpdate*) waits up to 30 s PER
# ITERATION on async delivery and cannot fit any CI wall; its behavior
# is covered by the SDK's own update suites. All other hot paths are
# measured here, bounded at 300 ms per benchmark.
timeout 900 go test -run '^$' -bench 'BenchmarkInvokePing|BenchmarkConcurrentInvoke|BenchmarkEntityConversion|BenchmarkSendMessage|BenchmarkEditMessage|BenchmarkDeleteMessages|BenchmarkForwardMessages|BenchmarkSearchMessageConversion|BenchmarkParsePhone|BenchmarkUploadSinglePart|BenchmarkDownloadSingleChunk|BenchmarkDuplicateDetection|BenchmarkMessageKeyLookup|BenchmarkAuthStateRead|BenchmarkCrossPathConversion' -benchtime 300ms -count=1 \
  ./client/ | grep -E "Benchmark|^ok" | tee -a "${OUT}"
step_end "sdk-benchmarks" 0

echo "BENCH: completed (results in ${OUT})"
