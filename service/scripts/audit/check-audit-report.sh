#!/usr/bin/env bash
# check-audit-report.sh — validates the generated Persian HTML report:
# structure, RTL, anchors, no external dependencies.
set -uo pipefail
REPORT="$(cd "$(dirname "$0")/../.." && pwd)/docs/audit/fa/comprehensive-audit.html"

fail=0
grep -q '<!DOCTYPE html>' "$REPORT" || { echo "missing doctype"; fail=1; }
grep -q '<html lang="fa" dir="rtl">' "$REPORT" || { echo "missing RTL html tag"; fail=1; }
tail -c 20 "$REPORT" | grep -q '</html>' || { echo "missing closing html"; fail=1; }
# Every internal anchor must have a target.
for anchor in $(grep -o 'href="#[a-z0-9]*"' "$REPORT" | sed 's/href="#//;s/"//'); do
  grep -q "id=\"$anchor\"" "$REPORT" || { echo "broken anchor: #$anchor"; fail=1; }
done
# No external dependencies.
grep -q 'src="http' "$REPORT" && { echo "external script found"; fail=1; }
grep -q 'href="http' "$REPORT" && true # links to GitHub are fine (not resources)
# Required sections present.
for sec in "sec1" "sec2" "sec4" "sec13" "sec14" "sec20"; do
  grep -q "id=\"$sec\"" "$REPORT" || { echo "missing section $sec"; fail=1; }
done
if [ "$fail" = "0" ]; then echo "audit report OK: $REPORT"; else exit 1; fi
