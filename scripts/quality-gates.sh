#!/usr/bin/env bash
# Run every quality gate that is executable in this environment.
#
# Usage: scripts/quality-gates.sh [--full]
#   --full  also runs fuzz smoke tests and the -race suite (slower)
#
# Exit status is non-zero if any gate fails. Each gate prints one line so the
# output can be pasted into a test report without editing.
set -uo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh

FAILED=0
gate() {
  local name="$1"; shift
  printf '%-34s' "$name"
  if out=$("$@" 2>&1); then
    echo "PASS"
  else
    echo "FAIL"
    echo "$out" | sed 's/^/    /' | tail -25
    FAILED=1
  fi
}

echo "== Z Network quality gates =="
go version

gate "gofmt"            bash -c 'test -z "$(gofmt -l internal cmd 2>&1)"'
gate "go vet"           go vet ./...
gate "go build"         go build ./...
gate "unit+integration" go test -count=1 -timeout 600s ./...
gate "coverprofile"     go test -count=1 -timeout 600s -coverprofile=/tmp/zcover.out ./...

if [ "${1:-}" = "--full" ]; then
  echo "-- extended gates --"
  gate "race detector"  bash -c 'CGO_ENABLED=1 go test -count=1 -race -timeout 900s ./...'
  for f in FuzzRun FuzzProofDecode; do
    for pkg in ./internal/zvm ./internal/zmerkle; do
      if grep -q "func $f" "$pkg"/*_test.go 2>/dev/null; then
        gate "fuzz $f (10s)" bash -c "go test -run=^$ -fuzz=^$f\$ -fuzztime=10s $pkg"
      fi
    done
  done
fi

echo
if command -v go >/dev/null; then
  go tool cover -func=/tmp/zcover.out 2>/dev/null | tail -1 || true
fi
if [ $FAILED -eq 0 ]; then
  echo "ALL GATES PASSED"
else
  echo "SOME GATES FAILED"
fi
exit $FAILED
