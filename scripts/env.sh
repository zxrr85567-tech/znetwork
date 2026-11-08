#!/usr/bin/env bash
# Source this to obtain the pinned toolchain environment.
export PATH="$HOME/.local/go/bin:$PATH"
export GOMODCACHE="$HOME/.local/gomodcache"
export GOPATH="$HOME/.local/gopath"
export GOTOOLCHAIN=local
export CGO_ENABLED=0
