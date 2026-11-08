# Transaction Specification

**Implementation:** `internal/ztypes/tx.go`
**Verified by:** `internal/ztypes/types_test.go` (including `FuzzDecodeTx`)

## Envelope

A transaction is versioned, self-identifying and canonical:

```
version  u8  = 0x01
chainID  string
type     u8
sender   Address(20)
pubkey   PublicKey(32)
nonce    u64
gasLimit u64
gasPrice u64
timeMs   i64
memo     string   (≤ MaxMemoLen)
payload  bytes    (≤ MaxPayloadLen)
```

`zencoding` writes every variable-length field as `u32 length ‖ bytes` and every
fixed field as raw bytes. There is exactly one encoding for any transaction:
`DecodeTx` rejects truncated input **and** trailing bytes (`r.Done()`), which is
verified by `FuzzDecodeTx` — any byte string that decodes must re-encode to
itself.

## Identity and signature

```
body   = the encoding above
txID   = Digest("ZNET/v1/tx-id", body)
sigMsg = txID                          (32 bytes)
sig    = Ed25519_sign(privateKey, Digest("ZNET/v1/transaction", sigMsg))
```

Consequences, and why they are deliberate:

- **The chain id is inside the id**, so a signed transaction cannot be replayed
  on another network (threat T-03).
- **The nonce is inside the id**, and execution requires
  `tx.Nonce == account.Nonce`, so a transaction cannot be replayed on the same
  network (threat T-04).
- **The sender address and public key are both inside the id**, and
  verification requires the address to be derived from that key. A mismatched
  pair is rejected before signature verification.
- **Ed25519 is non-malleable**, so `(txID, sig)` has one valid form.

`Sign` refuses to sign if `tx.Sender != key.Address()`, so a caller cannot
accidentally produce a transaction that no node will accept.

## Transaction types

| Value | Name | Payload |
|---|---|---|
| 1 | `transfer` | `to Address, amount u64` |
| 2 | `delegate` | `validator Address, amount u64` |
| 3 | `undelegate` | `validator Address, amount u64` |
| 4 | `validator_register` | `pubkey, moniker, commissionBps` |
| 5 | *(reserved)* | previously validator update; never issued |
| 6 | `gov_proposal` | `kind, title, description, paramKey, paramValue, deposit` |
| 7 | `gov_vote` | `proposalID u64, option u8` |
| 8 | `contract_deploy` | `code bytes` |
| 9 | `contract_call` | `contract Address, calldata bytes, value u64` |

Payloads are themselves versioned (`payload tag u8` first) and decoded with the
same trailing-byte rejection as the envelope.

## Fees and gas

```
fee           = gasLimit × gasPrice            (uz, overflow-checked)
intrinsic gas = baseTxGas + 16 × len(tx.Encode())
```

Defaults: `baseTxGas = 21,000`, `gasPerByte = 16`, `minGasPrice = 1 uz`.

Rules enforced at validation (before execution, so a bad transaction cannot cost
a block producer anything):

1. `gasLimit ≥ intrinsicGas`
2. `gasLimit ≤ blockGasLimit`
3. `gasPrice ≥ minGasPrice`
4. `fee` does not overflow u64
5. sender balance ≥ `fee` (checked at execution, when the balance is authoritative)

`fee` is **deducted up front** and the unused portion
`(gasLimit − gasUsed) × gasPrice` is refunded after execution, so a transaction
that runs out of gas still pays for what it used. Gas that is paid for but not
consumed is returned; nothing is silently burned except the explicit burn
parameter (see ECONOMICS.md).

## Nonce discipline

- The pool accepts `account.Nonce ≤ nonce ≤ account.Nonce + MaxFutureNonce` and
  requires `gasPrice` strictly greater than any pooled transaction with the same
  nonce to replace it.
- Execution requires an **exact** match `tx.Nonce == account.Nonce`; then the
  account nonce is incremented by one.
- A transaction from the mempool that no longer matches (because another node's
  block consumed the nonce) is dropped at selection time, not at insertion, so
  the pool stays correct across view changes.

## Size limits

| Limit | Value | Where enforced |
|---|---|---|
| `MaxMemoLen` | 256 bytes | `Tx` validation and `DecodeTx` |
| `MaxPayloadLen` | 1 MiB (1 << 20) | envelope and payload decoding |
| `MaxFutureNonce` | 64 | mempool admission per sender |
| transaction size | implicit: envelope + payload + memo + fixed fields | `znet` frame limit (4 MiB) is far above it |

**LIMITATION:** contract code and calldata are bounded only by
`MaxPayloadLen` (1 MiB) and by gas, not by a dedicated tighter cap. A 1 MiB
deploy is valid but costs `16 × size` intrinsic gas plus deployment gas, so it
is expensive rather than impossible. Tighter per-type caps are recorded in
BLOCKERS.md as a hardening item.

## Stateless validation order

Stateless checks run first and are cheap; stateful checks run in the pool and
again at execution. A block producer therefore cannot be griefed by a
transaction that fails a cheap check only after expensive work.

```
1. version, type, sizes, field bounds
2. sender != zero, pubkey != zero, address derives from pubkey
3. signature verifies over the domain-separated digest
4. gas limits and fee overflow
5. payload decodes canonically
6. (stateful) nonce window, balance ≥ fee
```

## Test evidence

- `TestTxEncodingIsCanonical` — round trip, and mutated fields change the id.
- `FuzzDecodeTx` — 1,028,896 executions over arbitrary bytes in the recorded
  campaign: no panics, and every accepted encoding re-encodes identically
  (this is what caught trailing-byte acceptance).
- `TestDecodeTxRejectsTrailingBytes`, `TestSignRejectsWrongSender`,
  `TestFeeOverflowRejected`, `TestCrossChainSignatureRejected`.
