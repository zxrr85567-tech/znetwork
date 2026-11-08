#!/usr/bin/env bash
# Run every fuzz target for a fixed wall-clock budget and print a table.
# Usage: scripts/fuzz-campaign.sh [seconds-per-target]
set -uo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
T="${1:-20}"
printf '%-22s %-22s %s\n' TARGET PACKAGE RESULT
fail=0
for row in "FuzzDecode:./internal/zencoding" "FuzzDecodeTx:./internal/ztypes" \
           "FuzzReadMessage:./internal/znet" "FuzzProofDecode:./internal/zmerkle" \
           "FuzzRun:./internal/zvm"; do
  name="${row%%:*}"; pkg="${row##*:}"
  if out=$(go test -run='^$' -fuzz="^${name}$" -fuzztime="${T}s" "$pkg" 2>&1); then
    execs=$(printf '%s' "$out" | grep -o 'elapsed: .*execs: [0-9]*' | tail -1 | grep -o 'execs: [0-9]*')
    printf '%-22s %-22s PASS %s\n' "$name" "$pkg" "${execs:-}"
  else
    printf '%-22s %-22s FAIL\n' "$name" "$pkg"
    printf '%s\n' "$out" | tail -12 | sed 's/^/    /'
    fail=1
  fi
done
exit $fail
