# Z NETWORK — Integration Register

Each row is an interface between two parts of the system, the contract that binds
them, and the evidence that the contract holds. "Verified by" names a test or a
recorded live run — nothing here is asserted without one.

## Internal integrations

| # | A ↔ B | Contract | Verified by |
|---|---|---|---|
| I-01 | `znode.Chain` ↔ `zstore` | every committed block is appended before the in-memory chain advances; a failed append aborts the block | `TestRestartRecoversState`, `TestSingleNodeFullLifecycle` |
| I-02 | `znode.Chain` ↔ `zexec.Engine` | execution happens against a candidate state; state is only adopted after full validation | `TestRejectsBlockWithBadStateRoot`, `TestTransactionIncludedAndExecuted` |
| I-03 | `znode.Node` ↔ `zconsensus.Engine` | the node never invents votes; the engine never touches chain or network | `zconsensus` 18 tests, `TestConsensusMessagesAreDeliveredThroughNode` |
| I-04 | `znode.Node` ↔ `znet.Server` | every inbound message is decoded with the canonical codec and re-validated through the local path | `TestFourNodeNetworkReachesConsensus`, `TestNonValidatorNodeSyncsFromNetwork` |
| I-05 | `znet` ↔ `ztypes`/`zconsensus` codecs | one byte encoding per block/tx/vote, shared by wire, disk and RPC | `TestBlocksCodec`, `TestVoteCodec`, `FuzzReadMessage` |
| I-06 | `zmempool` ↔ `zstate` | admission is a stateful check against current state; stale txs are dropped at selection | `TestNonceBounds`, `TestMempoolRespectsNonceSequence` |
| I-07 | `zrpc` ↔ `znode.Backend` | RPC reads the chain through an interface; it cannot mutate state | `zrpc` 18 tests against a fake backend + live node checks |
| I-08 | `zrpc` ↔ `znode.Chain` receipts | `ReceiptBackend` optional capability; receipts attached to `z_getTransaction` | live `z_getReceipt` returning a real deploy receipt |
| I-09 | `zindexer` ↔ `znode.OnCommit` | index receives the same block and receipts that were committed | `TestSingleNodeFullLifecycle` (tx id found on disk), indexer 7 tests |
| I-10 | `zmetrics` ↔ `znode`/`zrpc` | counters increment on real events; gauges read live state at scrape time | live `/metrics` matching height, blocks, votes |
| I-11 | `zcrypto` ↔ everything | domain-separated digests; a domain change is a protocol break | `TestDigestFramingIsUnambiguous`, domain-separation test |
| I-12 | `zstate` ↔ `zmerkle` | state leaves use `ZNET/v1/state-leaf`, nodes use `ZNET/v1/state-node` | `TestInclusionProof`, `zmerkle` 12 tests |
| I-13 | `zvm` ↔ `zexec` | storage writes are journaled; failure rolls back | `TestFailedTxLeavesNoPartialState` |
| I-14 | `ztypes.Params` ↔ `zexec` | the parameter set is the single source of economic constants; invalid values rejected | `ztypes` params tests, `zexec` fee/stake tests |
| I-15 | `znodedcmd` ↔ all of the above | wiring: store → chain → node → net → indexer → RPC → signals | `internal/e2e` (real process) |

## External integrations

| # | Surface | Contract | Verified by |
|---|---|---|---|
| E-01 | CLI ↔ node | `zwallet`/`zcli` speak JSON-RPC; no CLI writes files the node reads except genesis/keystore | live testnet run (7 transactions, 4 nodes) |
| E-02 | node ↔ browser | explorer served by `znd`, same-origin relative fetches only | `live-ui-check.js`: 0 non-relative fetches, all views rendered |
| E-03 | node ↔ Prometheus | standard text exposition 0.0.4 with TYPE lines | `TestPrometheusExposition` (structure), live `/metrics` |
| E-04 | operator ↔ node | `--home`, `--genesis`, `--monitor`, `--rpc`, `--p2p`, `--seeds`, `--moniker`, env passwords | `scripts/testnet.sh` uses only these |
| E-05 | operator ↔ release | `zrelease` + `SHA256SUMS`; refuses dirty tree | two byte-identical builds, checksum verify |
| E-06 | consumer ↔ package | zip contains only tracked files; exclusions tested | `TestTrackedFilesSkipsExcluded` |

## Integration failure modes that were found and fixed

| Symptom | Root cause | Fix |
|---|---|---|
| indexer had no receipts on a fresh sync | receipts were only kept in memory | persisted `receipts.log` + `ensureReceipts()` re-execution |
| full node diverged after catching up | sync advanced chain but not the consensus engine | `resyncEngine` positions the engine absolutely |
| 4-node network stalled at one height | engine height counter drifted above chain | `SetHeight` from chain height everywhere |
| explorer receipt panel always empty | RPC never exposed receipts | `ReceiptBackend` + `z_getReceipt` |
| `zcli devnet` failed on every key encrypt | `EncryptKey(…, 0)` treated 0 as a weak parameter | 0 means default iterations |
| e2e builds failed after several runs | 27 MB binaries leaked in `/tmp` | `TestMain` cleanup |

## Cross-cutting invariants (must hold across every integration)

1. **One encoding per object.** Wire, disk, RPC and signature all use the same
   canonical encoder. Changing it is a protocol version bump.
2. **Re-validation, not trust.** Any block from any source is re-executed against
   local state before it is adopted.
3. **Atomic adoption.** State is adopted only after the block's state root,
   receipt root and invariants check out.
4. **Chain is the height authority.** Any component that needs a height reads it
   from the chain, never from a private counter.
5. **No keys in logs or artifacts.** Verified by packaging exclusions and by
   password handling through files/env only.
