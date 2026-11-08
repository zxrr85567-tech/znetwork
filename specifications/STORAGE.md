# Storage Specification

**Implementation:** `internal/zstore/store.go`, `internal/zindexer/indexer.go`
**Verified by:** `internal/zstore/store_test.go` (10 tests), `internal/zindexer/indexer_test.go` (7 tests)

## Directory layout

`znd --home DIR` creates:

```
DIR/
  validator.key.json      encrypted validator key (0600), never overwritten by init
  genesis.json            genesis used at bootstrap (optional if blocks exist)
  passphrase              only in the generated testnet layout, 0600
  data/
    blocks.log            append-only canonical block log
    receipts.log          append-only receipt log
    states/<height>.state state snapshots, one per retained height
  index/
    tx-index.ndjson       append-only transaction index
    index-checkpoint.json atomic index checkpoint
  logs/                   (testnet launcher only)
```

`data/`, `bin/`, `*.log` and `*.zip` are git-ignored. No key material is ever
written outside a keystore file, and keystores are 0600.

## Block log

`blocks.log` is the chain's source of truth on disk:

```
record = u32 bodyLen || body || sha256(body)[32]
body   = encoded block (same canonical encoding used on the wire and in RPC)
```

| Rule | Value |
|---|---|
| Max record | 8 MiB (`MaxRecordLen`) — a corrupt length field cannot cause a huge allocation |
| Integrity | SHA-256 over the body, verified on every read |
| Append | one `write` of the whole record; no seeking |
| Recovery | a truncated tail is detected, reported, and can be repaired with `TruncateTail(validLen)` |

Read semantics (`ReadBlocksFile`):

- Returns every valid block **up to the first bad record**, plus the valid byte
  length.
- A truncated final record is `ErrTruncatedTail` (a crash during append), not
  corruption of earlier blocks.
- A checksum mismatch is a hard error: the reader stops at the last good record
  and reports it, and never silently skips a record.
- A hostile length (larger than the remaining file or than `MaxRecordLen`) is
  refused.

`RewriteBlocks` rebuilds the file atomically (temp file + rename) when a reorg or
a repair requires it, and the result stays appendable.

## Receipt log

`receipts.log` stores execution results so a restarted node can serve
`z_getReceipt` without re-executing history:

```
record = u32 bodyLen || body || sha256(body)[32]
body   = u64 height || u32 count || encoded receipts
```

A corrupt or truncated tail is **ignored** on read (receipts are derived data,
not consensus data), and the log is rewritten when the chain repairs gaps.
`Chain.ensureReceipts()` re-executes blocks to fill any height whose receipts are
missing — this is what makes a freshly synced full node able to answer receipt
queries, which was a real bug fixed during development.

## State snapshots

`states/<height>.state`:

```
u8 version || u32 entryCount || (keyLen u32 || key || valLen u32 || val)* || sha256(everything before)
```

On load the digest is verified **and** the Merkle root is recomputed; a snapshot
that hashes correctly but produces a different root than the block header's
`StateRoot` is refused. `TestStateSnapshotRejectsCorruption` flips a byte in the
middle of a snapshot and asserts the load fails.

`PruneStates(keep)` keeps the newest `keep` snapshots; `DeleteStatesAbove(h)`
removes snapshots above a fork point. Neither is called by `znd run` today, so
snapshots accumulate one per height — a deliberate trade (fast restart, more
disk) recorded in BLOCKERS.md.

## Transaction index

`index/tx-index.ndjson` is an append-only newline-delimited JSON file of
`{tx_id, height, index, type, sender, status, fee, time_ms}` records;
`index/index-checkpoint.json` records the indexed height and is written
atomically (temp + rename).

- `Open(dir, chainID)` refuses a directory written for a different chain id.
- `Sync(chain)` indexes any heights missing after a restart.
- `ByAddress` scans the file and filters — adequate for a testnet, **not** a
  database index (BLOCKERS.md B-006).
- Record count is capped at 500,000 (`maxRecords`); beyond that the indexer
  refuses new records rather than growing without bound.

## Crash consistency

| Failure | On restart |
|---|---|
| Crash while appending a block | tail record fails checksum ⇒ truncated tail reported ⇒ node trims to the last good block |
| Crash while appending receipts | tail ignored, receipts re-derived from blocks |
| Crash while writing a snapshot | temp file never renamed ⇒ snapshot absent ⇒ node rebuilds state by replaying blocks from the newest good snapshot |
| Crash while writing the index checkpoint | checkpoint is renamed atomically, so it is either old or new |
| Power loss between block append and snapshot | replay from snapshot to head re-derives identical state (root must match or the block is rejected) |

The e2e suite proves the important case end to end: a node is stopped and
restarted **without** `--genesis`, recovers its chain id from stored blocks,
resumes at or above its previous height, and still reports the same balance and
transaction — `TestSingleNodeFullLifecycle`.

## Bootstrapping order

```
if blocks.log has blocks:
    chain id <- blocks (header chain id), genesis hash <- state/block
    load newest snapshot, replay blocks after it, verify each StateRoot
else:
    require genesis (--genesis or <home>/genesis.json)
    write genesis block 0, snapshot state at height 0, append block 0
```

A node that finds both stored blocks and a genesis file for a **different**
chain refuses to start, rather than mixing two chains in one directory.

## Limits

| Resource | Bound | Rationale |
|---|---|---|
| block record | 8 MiB | bounds a corrupt length field |
| blocks per sync message | 128 | bounds one peer's work |
| receipts per block | `MaxTxPerBlock` | protocol bound |
| index records | 500,000 | bounds disk growth on a testnet |
| snapshots | one per height (unpruned) | fast restart; operator can prune |
