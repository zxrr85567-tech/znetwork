# State Specification

**Implementation:** `internal/zstate/state.go`, `internal/ztypes/state.go`
**Verified by:** `internal/zstate/state_test.go`, `internal/ztypes/types_test.go`

## The state is one Merkle tree

All chain state is a single key/value Merkle tree. The tree root after applying
block `H` is `Header.StateRoot` of block `H`, and it commits to every account
balance, nonce, validator, delegation, proposal, storage slot and metadata entry
in the chain's history up to `H`.

```
leaf   = Digest("ZNET/v1/state-leaf", key, value)
node   = Digest("ZNET/v1/state-node", left, right)
root   = merkleRoot("ZNET/v1/state", sorted leaves by key)
```

Merkle details (shared with transaction and receipt roots):

- leaves are sorted by key bytes before hashing, so insertion order never
  affects the root;
- an odd node at any level is promoted unchanged to the next level;
- a tree with zero leaves is the domain root, not the hash of an empty string;
- inclusion proofs are `{leafIndex, leafCount, steps[{sibling, siblingIsRight}]}`
  and verify against a root the caller already trusts. `leafIndex`/`leafCount`
  are informational: verification walks the step list. This is documented and
  asserted in `TestProofEncodingRejectsTampering`.

## Key space

Keys are `class byte || payload`. The class byte keeps the namespaces disjoint,
so a contract storage write can never overwrite an account record.

| Class | Prefix | Key layout | Value |
|---|---|---|---|
| Account | `0x01` | `0x01 ‖ address(20)` | `Account` |
| Storage | `0x02` | `0x02 ‖ contract(20) ‖ slot(8, BE)` | 32-byte word |
| Code | `0x03` | `0x03 ‖ codeHash(32)` | ZVM bytecode |
| Validator | `0x04` | `0x04 ‖ operator(20)` | `Validator` |
| Delegation | `0x05` | `0x05 ‖ delegator(20) ‖ validator(20)` | `DelegationRecord` |
| Proposal | `0x06` | `0x06 ‖ id(8, BE)` | `Proposal` |
| Vote | `0x07` | `0x07 ‖ id(8, BE) ‖ voter(20)` | vote record |
| Meta | `0x08` | `0x08 ‖ name` | see below |

Metadata entries:

| Name | Meaning |
|---|---|
| `supply` | total supply in uz (u64) |
| `params` | encoded `Params` |
| `chain-id` | chain identifier, bound into state |
| `genesis-hash` | genesis hash, so a node can prove which chain it is on |
| `height` | height of the state |
| `block-hash` | block hash of the state |
| `last-proposer` | proposer of the block that produced the state |
| `proposal-count` | highest governance proposal id |

## Account

```
version       u8  = 0x01
balance       u64        uz
nonce         u64        consumed by exactly one committed transaction each
codeHash      [32]byte   zero for a plain account
createdHeight u64        height at which the account first received funds
```

One account record does **not** contain its storage: storage slots are separate
leaves under the `0x02` class, so a contract with many slots does not inflate the
account record and each slot is independently provable.

## Validator

```
operator      Address
pubKey        PublicKey   (Ed25519, 32 bytes)
moniker       string      (≤ MaxMonikerLen)
commissionBps u16         (≤ 10000)
selfBond      u64
delegated     u64
status        u8          unbonded | bonded | unbonding | jailed
unbondHeight  u64
jailedUntil   u64
```

`VotingPower() = selfBond + delegated`. The active set for block `H` is computed
from the state after block `H-1`:

1. keep validators with `status == bonded`;
2. drop those with `power < min_validator_stake_uz`;
3. sort by power descending, then operator address ascending;
4. truncate to `max_active_validators`.

## Delegation

```
delegator    Address
validator    Address
amount       u64          actively delegated uz
unbonding    []{ amount u64, releaseHeight u64 }
```

`amount` earns rewards and counts toward the validator's power. `unbonding`
entries do neither until they are released: at each block, entries whose
`releaseHeight ≤ height` are returned to the delegator's balance.

## Governance proposal

```
id              u64
kind            u8    parameter change | text
title           string
description     string
paramKey        string
paramValue      u64
proposer        Address
deposit         u64
submitHeight    u64
votingEndHeight u64
status          u8    voting | passed | rejected | executed | cancelled
yes/no/abstain  u64   voting power recorded per option
```

Votes are recorded per voter in the `0x07` class, so one address cannot vote
twice on one proposal, and the tally is derived from voting power at vote time.

## Roots and proofs

| Function | Meaning |
|---|---|
| `st.Root()` | current state root (the value that goes into a block header) |
| `st.Prove(key)` | `(proof, root, ok)` — proof and the root it anchors to |
| `st.VerifyInclusion(root, key, value, proof)` | verifies against a trusted root |
| `st.AbsenceProof(prev, prevProof, next, nextProof)` | proves a key is absent between two adjacent keys |

`Prove` returns the **anchor root**, not the leaf hash. That is deliberate: a
caller verifies against a root it already trusts (for example one taken from a
committed block header), and a returned leaf hash cannot be used that way.

## Determinism requirements

The state root is part of consensus, so every node must produce identical state
from identical history. The rules that make that true:

1. **No floats.** All arithmetic is integer. Amounts are uz (u64).
2. **Sorted iteration everywhere.** Any loop that writes more than one key
   iterates in sorted key order or writes independent keys (writes commute in a
   Merkle map, but iteration order matters for anything that reads as it writes,
   such as reward distribution).
3. **Explicit map ordering.** `State.Validators()` returns validators sorted by
   operator address, never in Go map order.
4. **Failure atomicity.** A failed transaction never leaves a partial write: the
   execution engine snapshots the state and restores it on error (see
   EXECUTION.md).

## Memory and persistence

`zstate.State` is an in-memory tree with a persisted snapshot per height:
`data/states/<height>.state` with a version byte, the encoded entries and a
SHA-256 trailer. Loading verifies the digest and re-derives the root; a corrupt
snapshot is rejected rather than silently producing a different chain
(`TestStateSnapshotRejectsCorruption`).

**LIMITATION:** the in-memory tree is rebuilt from the snapshot on startup and
kept whole. There is no pruning of historical state during normal operation, so
memory grows with the number of distinct keys ever written (not with history).
A production deployment needs a persistent key-value backend; see BLOCKERS.md.

## Invariants checked after every block

When `CheckInvariants` is enabled (default in `znd`), after applying each block:

1. **Supply conservation.** `sum(account balances) + sum(bonded) + sum(unbonding)
   + treasury + paid fees + burned ≥/≤ total supply` must balance exactly
   (`VerifySupply`). A mismatch aborts the block and the node refuses it.
2. **Nonce monotonicity.** No account nonce decreases.
3. **Validator power bound.** No validator's power exceeds total supply.

An invariant failure is a consensus-stopping error, by design: a node that
silently accepts a supply bug cannot be trusted with the next block.
