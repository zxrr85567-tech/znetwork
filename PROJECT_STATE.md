# Z NETWORK — PROJECT STATE

**As of:** 2026-11-08
**Version:** v1.0.0-rc1
**Status:** development / testnet. **Not Mainnet. Not audited.**
**Repository:** local git, `<repo>` (no remote — BLOCKERS.md B-001)

---

## 1. One-paragraph state

Z Network is a complete, tested, integrated single-repository blockchain
ecosystem: a BFT consensus node with a p2p network, a deterministic execution
engine with a small smart-contract VM, a Merkle-backed world state with
snapshots and proofs, staking and governance economics, an append-only block
store, a transaction index, a JSON-RPC API with 24 methods, a Go SDK, three
command-line tools, a node-served block explorer, Prometheus metrics,
reproducible packaging, and a specification set that matches the code. It runs a
4-validator local testnet, commits real transactions end to end, survives
restarts, and passes its full test, race and fuzz suites. It is explicitly a
development/testnet release: it has no external audit, no transport encryption,
no slashing, and no disk-backed state database.

## 2. Verified capability matrix

| Capability | State | Evidence |
|---|---|---|
| Ed25519 signatures, SHA-256 domain digests | working | RFC vectors, 88.2% coverage |
| Transaction lifecycle (sign → mempool → block → state → index → explorer) | working end to end | `internal/e2e`, live testnet |
| BFT consensus, 4 validators | working | `TestFourNodeNetworkReachesConsensus`, live run |
| Two-process network agreement | working | `TestTwoNodeNetwork` |
| Full-node sync + re-execution | working | `TestNonValidatorNodeSyncsFromNetwork` |
| Restart recovery without genesis | working | `TestSingleNodeFullLifecycle` |
| Smart contracts (deploy + call) | working, single-frame | live deploy with receipt events |
| Staking: delegate / undelegate / unbonding | working | live decode + `z_getDelegations` |
| Governance: propose / vote / tally | working | live proposal with yes vote recorded |
| JSON-RPC (24 methods) | working | 18 tests + live checks |
| Explorer | working | headless live UI check |
| Metrics (`/metrics`) | working | live counters matched height |
| Reproducible packaging | working | byte-identical rebuild |
| Block explorer write path | does not exist (read-only by design) | — |
| Transport encryption | **absent** | BLOCKERS B-003 |
| Slashing | **absent** | BLOCKERS B-004 |
| Disk-backed state | **absent** (snapshots instead) | BLOCKERS B-005 |
| External audit | **absent** | BLOCKERS B-002 |
| Public network / Mainnet | **absent and not claimed** | — |

## 3. Numbers (measured, not estimated)

| Metric | Value |
|---|---|
| Go packages | 25 (22 internal, 4 commands) |
| Go source files / lines | 67 files, ~21,950 lines (including tests) |
| Test functions | 216 |
| Statement coverage | 63.2% overall |
| Fuzz targets / campaign | 5 targets, 3,726,218 executions, 0 crashes |
| Race suite | 18/18 packages clean, 0 data races |
| Git commits | 12 at the time of measurement (packaging commit follows) |
| Live network | 4 validators, 600 ms blocks, 143 blocks in one run |
| Transactions verified live | 7 (transfer ×2, delegate, undelegate, deploy, propose, vote) |
| Release package | ~265 KB zip, 97 tracked files |

## 4. What exists on disk

| Path | Contents |
|---|---|
| `internal/` | 22 packages (see MODULE_REGISTER.md) |
| `cmd/` | `znoded`, `zwallet`, `zcli`, `zrelease` |
| `specifications/` | 11 normative documents + index |
| `security/` | threat model (T-01…T-42) |
| `scripts/` | env, quality gates, fuzz campaign, testnet launcher, release, toolchain bootstrap |
| `web/explorer/` | single-file explorer, harness generator, live UI check |
| `releases/` | packaged artifact, manifest, SHA256SUMS, release manifest |
| root | `ENVIRONMENT_DISCOVERY_REPORT.md`, `PROJECT_STATE.md`, `NEXT_ACTIONS.md`, `EXECUTION_LOG.md`, `BLOCKERS.md`, `TEST_REPORT.md`, `MODULE_REGISTER.md`, `DEPENDENCY_GRAPH.md`, `INTEGRATION_REGISTER.md` |

## 5. What is deliberately not here

- No token, no token sale, no public network, no fund handling.
- No third-party dependencies, no vendored code, no generated code.
- No mock "demo data" presented as chain data: the numbers in this document came
  from processes that ran.
- No claims of audit, certification or Mainnet readiness.

## 6. Where to start reading

1. [BLOCKERS.md](BLOCKERS.md) — the honest limits.
2. [TEST_REPORT.md](TEST_REPORT.md) — what was actually run.
3. [specifications/README.md](specifications/README.md) — how it works.
4. [MODULE_REGISTER.md](MODULE_REGISTER.md) — what each package owns.
5. [NEXT_ACTIONS.md](NEXT_ACTIONS.md) — what would happen next.
