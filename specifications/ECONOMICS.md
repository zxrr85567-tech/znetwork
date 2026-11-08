# Economics Specification

**Implementation:** `internal/zexec/engine.go` (`EndBlock`), `internal/zexec/tx_types.go`,
`internal/ztypes/params.go`
**Verified by:** `internal/zexec/*_test.go`, `internal/znode/*_test.go`

## Parameters (defaults)

Taken from `ztypes.DefaultParams()`, which is what the devnet generator embeds in
genesis. All values are integers; there is no floating point anywhere in the
protocol.

| Parameter | Key | Default | Meaning |
|---|---|---|---|
| Block reward | `block_reward_uz` | 2 Z (200,000,000 uz) | minted per block |
| Min validator stake | `min_validator_stake_uz` | 1,000 Z | floor to enter the active set |
| Unbonding period | `unbonding_blocks` | 100 blocks | delegation release delay |
| Voting period | `voting_period_blocks` | 300 blocks | governance window |
| Min proposal deposit | `min_proposal_deposit_uz` | 100 Z | required to submit |
| Max active validators | `max_active_validators` | 21 | consensus set size |
| Block gas limit | `block_gas_limit` | 10,000,000 | gas per block |
| Base tx gas | `base_tx_gas` | 21,000 | intrinsic gas per transaction |
| Gas per byte | `gas_per_byte_uz` | 16 | intrinsic gas per encoded byte |
| Min gas price | `min_gas_price_uz` | 1 uz | fee floor |

`Params` is validated on genesis load and on every governance change: every
value has an inclusive bound, and a parameter set outside those bounds is
rejected rather than clamped.

## Supply

- Total supply starts at the sum of genesis allocations and is stored in state
  metadata (`supply`).
- The **only** minting is the block reward, once per block, in `EndBlock`.
- Supply is capped by `MaxSupply`; at the cap, minting stops (partially minting
  the remainder in the block that reaches it).
- v1 has **no burn** and no fee destruction: all transaction fees are paid to
  the block proposer.

The supply invariant is enforced after every block when invariants are on
(default in `znd`):

```
liquid (sum of all account balances, including the treasury)
+ bonded (sum of validator selfBond + delegated)
+ unbonding (sum of delegation entries not yet released)
+ proposal deposits held
= total supply
```

A block that breaks this equality is rejected, the node logs the failure and
refuses to commit. `VerifySupply` is what caught two accounting bugs during
development, which is why it is on by default even though it costs time.

## Block reward distribution

For each block with reward `R` over bonded validators with total voting power `P`:

```
vReward    = R × power(v) / P
commission = vReward × commissionBps(v) / 10,000      → validator operator
toDelegators = vReward − commission
delegatorShare(d) = toDelegators × d.amount / power(v)
remainder  = toDelegators − Σ delegatorShare           → validator operator
```

Then any amount still undistributed because of per-validator truncation goes to
the **proposer**. The result is that `minted == credited` exactly: integer
division never creates or destroys a token, it only moves the remainder to a
deterministic recipient.

If there is no bonded stake at all, the reward is **not minted** — supply does
not grow for blocks nobody secured.

## Staking

### Register

`validator_register { pubkey, moniker, commissionBps }` creates or updates the
validator record. `commissionBps ≤ 10,000` and the moniker has a length bound.
The validator starts `unbonded` and enters the active set once it has bond.

### Delegate

`delegate { validator, amount }` moves `amount` uz from the delegator's balance
into the delegation record. The validator's power increases immediately; if the
validator crosses `min_validator_stake_uz` it becomes `bonded` and can be
selected for consensus at the next height.

### Undelegate

`undelegate { validator, amount }` moves `amount` from the active delegation into
an unbonding entry with `releaseHeight = height + unbondingBlocks`.
Unbonding stake:

- does **not** earn rewards,
- does **not** count toward the validator's voting power,
- is released to the delegator's balance automatically at the first block whose
  height reaches `releaseHeight`.

The 100-block delay is the protocol's only economic-security mechanism in v1:
there is **no slashing** (see BLOCKERS.md B-004), so unbonding exists to make
attacks costly in opportunity terms, not to punish equivocation.

## Fees

```
fee = gasLimit × gasPrice                     (uz)
```

- Deducted from the sender **before** execution.
- The unused portion `(gasLimit − gasUsed) × gasPrice` is refunded **after**
  execution, so an out-of-gas transaction still pays for the gas it consumed.
- The whole fee is credited to the block proposer in the same block.
- `gasPrice ≥ min_gas_price_uz` (1 uz by default) is enforced statelessly.

Mempool replacement uses `gasPrice` as the ordering signal: to replace a pooled
transaction from the same sender at the same nonce, the replacement must be
strictly more expensive.

## Governance

### Proposal

`gov_proposal { kind, title, description, paramKey, paramValue, deposit }`

- Requires `deposit ≥ min_proposal_deposit_uz`; the deposit moves from the
  proposer's balance to the system treasury account.
- `kind = parameter_change` requires `paramKey` to name an existing parameter and
  `paramValue` to be within that parameter's bounds.
- `votingEndHeight = submitHeight + voting_period_blocks`.

### Vote

`gov_vote { proposalID, option }` with `option ∈ {yes, no, abstain}`.

- Voting power is the voter's **delegated** stake at vote time; one address may
  vote once per proposal (a duplicate vote is rejected, and a second vote record
  cannot be written because the key already exists).
- Tally is kept as `yes/no/abstain` totals in voting power inside the proposal.

### Tally and execution

At the block where `height ≥ votingEndHeight`, the proposal is resolved:

```
quorum     = bonded × GovQuorumBps / 10,000     (GovQuorumBps = 3340, i.e. 33.4%)
passes     = totalVoted ≥ quorum AND yes > no AND yes > 0
```

Both inputs are integers: `bonded` is the total voting power of bonded validators
in the state at resolution time and `totalVoted = yes + no + abstain`. A passed
parameter change writes the new value into state parameters at that height; a
proposal whose parameter change cannot be applied is marked `rejected` and never
partially applied.

A rejected (or failed-to-apply) proposal leaves the deposit where submission put
it: the **system treasury account**. v1 does not refund and does not burn; the
tokens remain part of total supply and are accounted for by the supply
invariant. This is a plain design gap, recorded in BLOCKERS.md (B-011), not a
feature.

### Treasury

The treasury is a normal account at `Digest("ZNET/v1/system-treasury")` truncated
to an address. It holds proposal deposits. v1 has no spending proposal type;
adding one is an extension, not a configuration change.

## Verified live behaviour

On a 4-validator local testnet:

| Action | Result |
|---|---|
| transfer 100 Z faucet → user | committed at height 10, balance exactly +100 Z |
| transfer 5.25 Z with memo | committed at height 11 |
| delegate 10 Z to validator-2 | committed at height 12 |
| undelegate 4 Z | committed at height 13, unbonding entry with `release_height = 113` |
| governance proposal (raise gas limit) with 100 Z deposit | committed at height 24, status `voting`, `voting_end_height = 324` |
| yes vote with 6 Z delegated stake | committed at height 29, `yes_uz = 600000000` |
| contract deploy | committed at height 19, receipt with event and 46,712 gas |

Delegation state read back through `z_getDelegations` showed
`amount_uz = 600000000`, `unbonding = [{400000000, release_height 113}]`,
matching the arithmetic exactly.

## Economic properties this build does **not** have

| Missing | Consequence |
|---|---|
| Slashing / jailing on equivocation | Safety failure is not economically punished; only liveness is (no reward) |
| Inflation schedule / decreasing reward | Reward is constant per block |
| Fee market / dynamic base fee | First-price, proposer-ordered fees |
| Delegation caps per validator | A single delegator can dominate a validator |
| Treasury spending governance | The treasury accumulates and never spends |

These are tracked as blockers and are not implied to exist.
