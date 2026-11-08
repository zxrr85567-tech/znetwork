# Z NETWORK — Module Register

Every package in the module, what it owns, and its verified state. Generated from
`go list` and checked against the code; test counts and coverage are from the
run recorded in [TEST_REPORT.md](TEST_REPORT.md).

Module: `znetwork` · Go 1.24 · **zero third-party dependencies** · 25 packages
(22 `internal/`, 4 `cmd/` including `zrelease`).

## Foundations

| Package | Owns | Depends on | Tests | Coverage | State |
|---|---|---|---|---|---|
| `internal/zencoding` | canonical binary writer/reader, length bounds, strict `Done()` | — | 14 + fuzz | 86.0% | complete |
| `internal/zcrypto` | SHA-256 domain digests, Ed25519 keys/addresses/signatures, Bech32, PBKDF2, AES-GCM keystore | — | 20 | 88.2% | complete |
| `internal/zmerkle` | sorted-leaf Merkle tree, inclusion/absence proofs, proof codec | zcrypto, zencoding | 12 + fuzz | 94.9% | complete |
| `internal/zmetrics` | process counters and Prometheus exposition | — | 4 | 87.2% | complete |

## Data and state

| Package | Owns | Depends on | Tests | Coverage | State |
|---|---|---|---|---|---|
| `internal/ztypes` | transaction, block, header, receipt, genesis, account, validator, delegation, proposal, params encodings + validation | zcrypto, zencoding | 18 + fuzz | 70.6% | complete |
| `internal/zstate` | Merkle-backed world state, key classes, proofs, snapshot encode/decode | zcrypto, zmerkle, ztypes | 11 | 79.4% | complete |
| `internal/zstore` | block log, receipt log, state snapshots, tail repair, rewrite | zcrypto, zencoding, zstate, ztypes | 10 | 74.5% | complete |
| `internal/zindexer` | append-only transaction index, checkpoint, address queries | zcrypto, ztypes | 7 | 82.5% | complete (linear queries) |

## Execution

| Package | Owns | Depends on | Tests | Coverage | State |
|---|---|---|---|---|---|
| `internal/zvm` | ZVM instruction set, gas, assembler, journaled storage host | ztypes | 16 + fuzz | 76.6% | complete (single-frame) |
| `internal/zexec` | block begin/end, transaction dispatch, fees, staking, governance, invariants | zcrypto, zstate, ztypes, zvm | 22 | 76.4% | complete |

## Consensus, network, node

| Package | Owns | Depends on | Tests | Coverage | State |
|---|---|---|---|---|---|
| `internal/zconsensus` | validator sets, vote sets, locking, propose/prevote/precommit, commit certificates, proposer selection | zcrypto, ztypes | 18 | 77.5% | complete |
| `internal/znet` | TCP framing, handshake, message codecs, gossip dedup, reconnect | zconsensus, zcrypto, zencoding, ztypes | 13 + fuzz | 79.0% | complete (no transport crypto) |
| `internal/znode` | chain pipeline, block production, sync, consensus driver, receipt retention | zconsensus, zcrypto, zexec, zmempool, zmerkle, zmetrics, znet, zstate, zstore, ztypes | 19 | 72.5% | complete |
| `internal/zmempool` | admission, nonce windows, replacement, eviction, selection | zstate, ztypes | 12 | 76.2% | complete |

## Interfaces

| Package | Owns | Depends on | Tests | Coverage | State |
|---|---|---|---|---|---|
| `internal/zrpc` | JSON-RPC service (24 methods), error codes, metrics endpoint, explorer embedding | zcrypto, zexec, zmempool, zmetrics, zstate, ztypes | 18 | 73.8% | complete |
| `internal/zsdk` | Go client, tx builders, signing helpers, `WaitForTx` | zcrypto, ztypes, zvm | 6 | 33.5% | complete (network paths via e2e) |
| `internal/zdevnet` | deterministic validator/user devnet generator | zcrypto, ztypes | — | exercised via e2e/znode | complete |
| `internal/zbuild` | deterministic zip packaging, manifests, checksum verify | — | 5 | 72.5% | complete |

## Commands

| Command | Owns | Depends on | State |
|---|---|---|---|
| `cmd/znoded` → `internal/znodedcmd` | `init`, `run`, `version`; wires store, chain, net, indexer, RPC, metrics, signals | zcrypto, zindexer, zmetrics, znet, znode, zrpc, zstore, ztypes | complete |
| `cmd/zwallet` → `internal/zwalletcmd` | key generation, address, balance, send, delegate, undelegate, register-validator, propose, vote, deploy, call, sign | zcrypto, zsdk, ztypes | complete |
| `cmd/zcli` → `internal/zclicmd` | inspection (`status`, `block(s)`, `tx`, `account`, `delegations`, `validators`, `proposals`, `params`, `supply`, `mempool`), offline tools (`decode`, `genesis hash/check`), `devnet`, `keygen` | zcrypto, zdevnet, zsdk, ztypes | complete |
| `cmd/zrelease` | packaging with dirty-tree guard | zbuild | complete |

## Test-only and auxiliary

| Path | Owns | State |
|---|---|---|
| `internal/e2e` | builds real binaries, drives real processes (lifecycle, restart, two-node network) | complete |
| `web/explorer/` | single-file explorer UI, harness generator, live UI check | complete |
| `scripts/` | env, quality gates, fuzz campaign, testnet launcher, release, toolchain bootstrap | complete |

## Deliberate absences

| Not present | Why | Blocker |
|---|---|---|
| P2P encryption layer | scope and time; message layer is transport-agnostic | B-003 |
| Slashing / evidence | not implemented, not simulated | B-004 |
| Disk-backed state DB | zero-dependency policy; in-memory + snapshots instead | B-005 |
| SQL index | no SQLite available offline | B-006 |
| Prometheus/Grafana stack | not installable offline; standard exposition verified by test | B-017 |
| Docker images | Docker unavailable | B-018 |
| Light client | header-chain verification would need a new sync mode | not started (see BLOCKERS) |
