#!/usr/bin/env bash
# verify-conformance.sh — runs the SDK-conformance test suite only
# (adapter session blob symmetry, error mapping table, conversions).
set -uo pipefail
cd "$(dirname "$0")/../.."
go test -count=1 -v \
  -run "TestSessionBlob|TestTranslate|TestConvertUpdate|TestConvertMessage|TestPeerConversion" \
  ./internal/infrastructure/upstream/
