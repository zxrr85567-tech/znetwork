# Z NETWORK — Release Manifest

## v1.0.0-rc1

| Field | Value |
|---|---|
| Version | `v1.0.0-rc1` |
| Protocol version | 1 |
| Commit | recorded in `releases/RELEASE_MANIFEST.md` of each build |
| Toolchain | `go1.24.4 linux/amd64` |
| Built | generated at build time |
| Package | `znetwork-v1.0.0-rc1.zip` |
| Package SHA-256 | see `releases/SHA256SUMS` |
| Files packaged | see `releases/*.manifest.json` |
| Hosted at | **nowhere** — no remote is connected (BLOCKERS B-001) |

> This file is the **template**. Running `scripts/release.sh <version>` writes the
> filled-in copy to `releases/RELEASE_MANIFEST.md` with the commit, build time,
> hash and file count of that build. The `releases/` directory is build output and
> is not tracked in git; regenerate it before publishing.

## Status statement

**This is a development/testnet release candidate. It is not Mainnet, it has not
been audited, and it must not be used to hold value.**

Verified capabilities: BFT consensus across 4 validators, full transaction
lifecycle, smart contracts, staking and governance, JSON-RPC, explorer, metrics,
restart recovery, reproducible packaging.

Known absences, in order of severity: no external audit (B-002), no transport
encryption (B-003), no slashing (B-004), no disk-backed state (B-005), no soak or
load numbers (B-012), unsigned artifacts (B-014). The complete register is
[`../BLOCKERS.md`](../BLOCKERS.md).

## What is inside

- `internal/` — 22 packages: encoding, crypto, merkle, types, state, store,
  indexer, VM, execution, consensus, p2p, node, mempool, RPC, SDK, devnet,
  metrics, packaging.
- `cmd/` — `znoded`, `zwallet`, `zcli`, `zrelease`.
- `specifications/` — 11 normative documents.
- `security/` — threat model.
- `scripts/` — quality gates, fuzz campaign, testnet launcher, release, env,
  toolchain bootstrap.
- `web/explorer/` — single-file explorer + headless live-UI check.
- Root documents — environment discovery, project state, test report, blockers,
  module/dependency/integration registers, next actions, execution log.

## What is deliberately excluded

`data/` (chain data), `bin/` (build output), `*.log`, `*.zip`, `*.key.json`
(keystores). Enforced by `internal/zbuild` and tested by
`TestTrackedFilesSkipsExcluded`.

## Verify the package

```bash
sha256sum -c SHA256SUMS
unzip -q znetwork-v1.0.0-rc1.zip -d znetwork
cd znetwork
source scripts/env.sh          # or install Go 1.24 yourself
go build ./...
go test ./...                  # 216 tests
scripts/quality-gates.sh --full
```

Rebuilding the package from the same commit produces a byte-identical archive
(verified twice during this release; see `../TEST_REPORT.md` §8).

## Reproduce the gates

`go test ./...` — 18 packages, 216 tests, all passing.
`CGO_ENABLED=1 go test -race ./...` — clean.
`scripts/fuzz-campaign.sh 20` — 5 targets, ~3.7M executions, no crashes.
