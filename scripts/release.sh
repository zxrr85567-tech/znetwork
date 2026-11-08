#!/usr/bin/env bash
# Build a reproducible source package after running every quality gate.
#
#   scripts/release.sh v1.0.0        full gates, then package
#   scripts/release.sh v1.0.0 --fast single test pass, then package
#
# Nothing is packaged if a gate fails.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh

VERSION="${1:?usage: scripts/release.sh <version> [--fast]}"
MODE="${2:-}"

echo "== quality gates"
if [ "$MODE" = "--fast" ]; then
  scripts/quality-gates.sh
else
  scripts/quality-gates.sh --full
fi

echo "== packaging $VERSION"
go run ./cmd/zrelease --version "$VERSION" --out releases

echo "== artifacts"
ls -la releases/
echo
echo "SHA256SUMS:"
cat releases/SHA256SUMS
