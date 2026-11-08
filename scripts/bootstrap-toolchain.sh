#!/usr/bin/env bash
# Installs the pinned Go toolchain used to build Z Network. No other toolchain is required.
set -euo pipefail
GO_VERSION="1.24.4"
DEST="${HOME}/.local"
GOBIN="${DEST}/go/bin/go"
if [ -x "$GOBIN" ] && "$GOBIN" version 2>/dev/null | grep -q "go${GO_VERSION}"; then
  echo "toolchain: go${GO_VERSION} already present at ${DEST}/go"
  exit 0
fi
echo "toolchain: installing go${GO_VERSION} into ${DEST}"
mkdir -p "$DEST"
TMP="$(mktemp -d)"
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o "${TMP}/go.tgz"
# SHA-256 published at https://go.dev/dl/?mode=json for go1.24.4.linux-amd64.tar.gz
EXPECTED=77e5da33bb72aeaef1ba4418b6fe511bc4d041873cbf82e5aa6318740df98717
ACTUAL=$(sha256sum "${TMP}/go.tgz" | cut -d' ' -f1)
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "toolchain: CHECKSUM MISMATCH" >&2
  echo "  expected $EXPECTED" >&2
  echo "  actual   $ACTUAL" >&2
  echo "  refusing to install; remove ${TMP} and retry" >&2
  exit 1
fi
echo "toolchain: checksum verified ($ACTUAL)"
rm -rf "${DEST}/go"
tar -C "$DEST" -xzf "${TMP}/go.tgz"
rm -rf "$TMP"
"$GOBIN" version
