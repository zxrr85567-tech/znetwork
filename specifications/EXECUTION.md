# Execution Specification

**Implementation:** `internal/zexec/` (`engine.go`, `tx_types.go`, `invariants.go`),
`internal/znode/produce.go` (block pipeline)
**Verified by:** `internal/zexec/*_test.go`, `internal/znode/node_test.go`

## Block pipeline

One node's view of producing block `H`, in order. Nothing is written to disk and
nothing is broadcast until the block is validated and committed.

```
1. pick height      H = chain.Height() + 1                     (chain is authoritative)
2. select txs       mempool.Select(state, MaxTxsPerBlock)      (nonce order, fees, balance)
3. sort txs         (sender, nonce, id)                        deterministic for every node
4. BeginBlock       height, time, proposer, params
5. execute          tx by tx → receipt per tx
6. EndBlock         block reward, unbonding releases, governance tally
7. invariant check  supply conservation (when enabled)
8. build header     prev hash, tx root, receipt root, state root, gas used
9. validate         full re-validation of the produced block against parent state
10. consensus       propose → vote → commit certificate
11. commit          state → block log → receipts log → index
```

The produced block is validated with the **same function** that validates blocks
received from peers (`Chain.ValidateOnly` → `applyTo`), so a producer cannot
build a block that it would reject from someone else.

## Header

```
version         u16      protocol version (1)
chainID         string
height          u64
timeMs          i64      proposer clock, monotonic floor of parent+1ms
prevHash        Hash
proposer        Address
txRoot          Hash     Merkle root over tx ids, domain "ZNET/v1/tx-root"
receiptRoot     Hash     Merkle root over receipts, domain "ZNET/v1/receipt-root"
stateRoot       Hash     state Merkle root after the block
validatorsHash  Hash     hash of the active validator set (domain "ZNET/v1/validator-set")
gasUsed         u64
gasLimit        u64
txCount         u32
```

Block hash is `Digest("ZNET/v1/block-header", encode(header))`. Because
`stateRoot`, `txRoot` and `receiptRoot` are all in the header, a block commits to
everything it did: a node cannot accept a block and compute different state, and
cannot accept a block's transactions but drop a receipt.

## Transaction execution

Every path (proposal, sync, mempool scrub) goes through `Engine.ExecuteTx`. It
never panics and always returns a receipt.

```
stage 1: stateless     chain id, sizes, address/key binding, signature,
                       gas price ≥ min, gas limit ≥ intrinsic
stage 2: staged debit  fee deducted from sender, nonce checked exactly
stage 3: dispatch      native (transfer/stake/gov/…) or contract (ZVM)
stage 4: refund        (gasLimit − gasUsed) × gasPrice returned
stage 5: receipt       status, gas, fee, events, contract address, output, error
```

Failure anywhere after stage 2 still produces a receipt with `status = failed`,
the gas it used charged, and the nonce consumed. That is the same semantics as a
reverted transaction on any production chain: it is *included*, it *cost*, and
it *changed the nonce* — it just did not change anything else.

Native transactions validate fully **before** mutating state (no partial writes),
and contract executions run against a journaled storage host whose writes are
replayed backwards on failure. `TestFailedTxLeavesNoPartialState` asserts that a
failed contract call leaves both balances and storage untouched.

## Receipts

```
txID            Hash
height          u64
index           u32        position in the block
sender          Address
type            u8
status          u8         success(1) | failed(0)
gasWanted       u64
gasUsed         u64
fee             u64        gasUsed × gasPrice
contractAddress Address    zero unless a deploy
events          []Event    {type, [[key, value], …]}
output          []byte     ZVM stack words (contract calls) or event data
error           string     empty on success
```

Receipts are logged per height in `data/receipts.log` and served by
`z_getReceipt`; a node that synced from peers re-executes blocks to rebuild them.

## Events emitted

| Event | Emitted by |
|---|---|
| `transfer` | transfer |
| `delegated`, `undelegated`, `unbonding_released` | staking |
| `validator_registered`, `validator_updated` | validator registration |
| `proposal_submitted`, `vote_cast`, `proposal_executed`, `proposal_rejected` | governance |
| `contract_deployed`, `contract_called` | contracts |
| `reward_paid` | block reward distribution |

Events are informational: consensus never depends on them, and a wrong event
cannot change state.

## EndBlock order

Order matters for determinism and is fixed:

```
1. unbonding releases     entries whose releaseHeight ≤ H are credited
2. governance tally       proposals whose votingEndHeight ≤ H are resolved
3. block reward           minted and distributed over bonded stake
```

Governance resolves before the reward so a parameter change (for example a new
`block_reward_uz`) takes effect at the block boundary it was decided, not one
block later in a way that depends on map iteration order.

## Determinism checklist

Enforced in code, verified by multi-node tests that compare block hashes and
state roots (not only heights):

- integer-only arithmetic; every addition/multiplication uses checked helpers
  (`AddU64`, `MulU64`, `mulDiv`) that fail on overflow instead of wrapping;
- all iteration over state is sorted (`Validators()`, `DelegationsFor()` sorted
  by delegator address);
- transaction order is `(sender, nonce, id)`, never arrival order;
- proposer selection is a pure function of `(height, round, validator set)`;
- time comes from the header, never from the local clock during execution;
- no randomness inside execution (`LOG`, storage, arithmetic only).

## Invariants

`internal/zexec/invariants.go` recomputes supply from all state classes after
each block:

```
sum(account balances) + sum(bonded) + sum(unbonding) + treasury-held deposits
  ≤ total supply, and the minted-per-block amount is exactly accounted for
```

`VerifySupply` returned a mismatch twice during development; both were real
accounting bugs, and both were fixed before any of this was documented. The check
is on by default in `znd` (`--no-invariants` disables it for profiling only).

## Measured behaviour

From the process-level end-to-end suite and the live testnet:

| Scenario | Result |
|---|---|
| single validator, 20 ms block time | blocks produced continuously; state root matches after restart |
| transfer 12.5 Z | recipient balance `+1_250_000_000` uz, receipt `committed`, memo preserved |
| restart without `--genesis` | resumes at ≥ prior height; balance and transaction survive |
| 4 validators, 600 ms | shared height, identical block hashes and state roots across nodes |
| contract deploy | receipt with `contract_deployed` event, 46,712 gas used |
| failed contract call | receipt `failed`, gas charged, no storage change, nonce consumed |
