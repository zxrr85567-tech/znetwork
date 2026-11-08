# Consensus Specification

**Implementation:** `internal/zconsensus/` (engine, vote set, validator set)
**Driver:** `internal/znode/node.go` (`runHeight`)
**Verified by:** `internal/zconsensus/engine_test.go`, `internal/znode/multinode_test.go`

## Model

Z Network uses a single-round-trip, stake-weighted BFT consensus:

```
height H, round R:
  PROPOSE  → proposer broadcasts a block + commit certificate for H-1
  PREVOTE  → validators that accepted the proposal vote for its block id
  PRECOMMIT→ validators that saw a prevote quorum for a block lock on it
  COMMIT   → validators that saw a precommit quorum append the block
```

A block is committed when the precommit vote set for `(H, R, blockID)` holds
**more than 2/3 of total voting power**. Voting power is
`self_bond + delegated` (see ECONOMICS.md).

There is no separate view-change message. A validator that times out simply
advances to round `R+1` and re-proposes; safety comes from the lock rule below,
so liveness never trades against safety.

## Voting power and quorum

| Quantity | Definition |
|---|---|
| `power(v)` | `v.SelfBond + v.Delegated`, as recorded in the parent state |
| `total` | sum of `power(v)` over the active set |
| `quorum` | `total/3*2 + 1` — strict majority of two thirds, computed with integer arithmetic, never floats |
| active set | bonded validators with `power ≥ min_validator_stake_uz`, sorted by power descending then operator address ascending, truncated to `max_active_validators` (21 by default) |

`quorum` is recomputed from the set that was active when the **parent** block
committed, so a block's validator set is always derived from state that was
already agreed.

## Vote identity and signing

A vote is:

```
chainID  string
height   u64
round    u32
step     PREVOTE | PRECOMMIT
blockID  32 bytes
voter    Address
pubkey   PublicKey
timeMs   i64          (unsigned; not covered by the signature)
signature 64 bytes
```

Signature covers

```
Digest("ZNET/v1/consensus-vote", encoded(chainID, height, round, step, blockID, voter))
```

`timeMs` and the public key are deliberately **not** covered: a vote is valid
regardless of when it was observed, and the voter's key is bound to the address
by `VerifyAddressSignature`. A vote whose `voter` and `pubkey` disagree is
rejected before it reaches the vote set.

Votes are signed with Ed25519 over the domain-separated digest. A vote signed
for another chain id, height, round, step or block is rejected by the vote set.

## Vote sets

Each `(height, round, step)` has one `VoteSet`:

- First vote per voter wins; exact duplicates are `ErrDuplicateVote` (ignored, not an error).
- A *conflicting* vote (same voter, different block) is **not** adopted, it is
  recorded as evidence and counted once for the voter's original choice only.
- Voting power is added once per voter, never per message.
- A vote set can only conclude **once**; a second quorum cannot overwrite it.

Consequently a Byzantine validator with 30% of stake cannot force two different
blocks to reach precommit for the same height and round.

## Locking (the safety rule)

When a validator sees a prevote quorum for `blockID` at `(H, R)`, it locks:

```
locked   = true
lockedID = blockID
```

While locked, the validator **refuses to prevote** any proposal whose block id
differs from `lockedID`, unless it observes a prevote quorum for a different
block in a **later round** of the same height. Only that observation releases
the lock (`polkaID`, `polkaRound` are recorded, and the precommit quorum rule
implies the locked block can no longer be committed by anyone honest).

Implementation notes that matter:

- `AdvanceRound` does **not** clear the lock. This was a real bug fixed during
  development: clearing it on every round change allowed two blocks at one
  height to gather precommits.
- Future-round votes are buffered (capacity 4096) and replayed after the round
  advances, so a slow node does not lose votes it has already seen.
- `AdvanceHeight`/`SetHeight` clears proposals, polka state and pending votes.

## Height ownership

The **chain is the authority on height**. A node positions its engine with
`SetHeight(chain.Height()+1, activeSet)` after commit, after a failed height,
and after syncing from peers. An engine that keeps an independent counter can
drift above its chain and then reject its own proposals
(`block height does not extend the chain`); this was observed under CPU
contention in a 4-node `-race` run and is the reason the API is absolute.

## Timeouts

```
RoundTimeout(R) = min(2s × (R+1), 30s)
MaxRounds       = 64
```

A round timeout advances to `R+1` and re-proposes (if this node is the
proposer). If `MaxRounds` is exceeded the height fails; the node logs
`height failed` and retries after 200 ms, having re-derived its height from the
chain. Reaching `MaxRounds` in a healthy network is a liveness incident, and the
operator-facing signal is the `znet_rounds_advanced_total` metric.

## Proposer selection

```
seed     = Digest("ZNET/v1/proposer-seed", height || round)
proposer = set[(seed mod total_power walked over the sorted set)]
```

Selection is deterministic from public data, so every node agrees without
another round of communication.

## Commit certificates

A committed block carries `/Commit`:

```
height  u64
round   u32
blockID 32 bytes
sigs    []{ voter, signature }
```

A syncing node verifies that:

1. the signers are members of the validator set for that height,
2. the signatures cover the block id,
3. the signers' combined power exceeds two thirds.

Where a block is applied through consensus (this node participated) the
certificate is checked against the live vote set instead; both paths call the
same `Commit` verification.

## What this protocol deliberately does not do

| Omitted | Consequence | Mitigation in this build |
|---|---|---|
| Explicit view-change messages | A partitioned proposer can waste rounds | Round timeout schedule; `MaxRounds` bound |
| Vote gossip fan-out limits per peer | A peer can flood votes | Frame size bound, vote-set dedup, per-peer connection cap |
| Slashing for equivocation | Conflicting votes are recorded but not punished | Evidence is kept in the vote set; slashing is a **known gap** (BLOCKERS.md B-004) |
| Adaptive timeouts | Fixed 2s step under load | Bounded by `MaxRounds`; measured stable at 4 nodes/600 ms on one host |

## Test evidence

- `TestFourNodeNetworkReachesConsensus` — four real servers over TCP reach a
  shared height with all four validators; run repeatedly under `-race` with
  zero failures after the height-ownership fix (12 consecutive runs, then 6
  parallel batches of 4 packages, 0 data races).
- `TestTwoNodeNetwork`, `TestNonValidatorNodeSyncsFromNetwork` — agreement and
  sync at a shared height.
- `TestFourNode*` verify both the block hash and the state root, not just the
  height, so a silent state divergence cannot pass.
