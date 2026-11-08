# Z NETWORK — Specification Set

This directory is the normative description of what the code in this repository
actually does. Every statement here was derived from the implementation and
verified by a test; where behaviour is intentionally limited, the spec says so
instead of describing an aspiration.

**Status: development / testnet v1.0.0. This is not a Mainnet specification.**

| Document | Covers | Source of truth |
|---|---|---|
| [CONSENSUS.md](CONSENSUS.md) | Propose → prevote → precommit → commit, quorum, locking, timeouts | `internal/zconsensus/` |
| [STATE.md](STATE.md) | Accounts, validators, delegations, params, state root | `internal/zstate/`, `internal/ztypes/state.go` |
| [TRANSACTIONS.md](TRANSACTIONS.md) | Transaction format, ids, signatures, fees, nonces | `internal/ztypes/tx.go` |
| [EXECUTION.md](EXECUTION.md) | Block pipeline, gas, receipts, invariants | `internal/zexec/`, `internal/znode/produce.go` |
| [VM.md](VM.md) | ZVM instruction set, gas costs, contract model | `internal/zvm/` |
| [ECONOMICS.md](ECONOMICS.md) | Supply, rewards, staking, unbonding, governance, fees | `internal/zexec/`, `internal/ztypes/params.go` |
| [NETWORKING.md](NETWORKING.md) | Framing, handshake, message types, sync, gossip | `internal/znet/` |
| [RPC.md](RPC.md) | JSON-RPC surface, error codes, limits | `internal/zrpc/` |
| [STORAGE.md](STORAGE.md) | On-disk layout, checksums, recovery | `internal/zstore/` |
| [CRYPTOGRAPHY.md](CRYPTOGRAPHY.md) | Ed25519, SHA-256, PBKDF2, AES-GCM, domains | `internal/zcrypto/` |
| [RELEASE_PROCESS.md](RELEASE_PROCESS.md) | Reproducible packaging, manifests, checksums | `internal/zbuild/`, `scripts/` |

## Reading order

1. [CRYPTOGRAPHY.md](CRYPTOGRAPHY.md) — every hash and signature is domain-separated; nothing else in the system is safe to read without it.
2. [TRANSACTIONS.md](TRANSACTIONS.md) and [STATE.md](STATE.md) — the data model.
3. [EXECUTION.md](EXECUTION.md) — how a transaction becomes state.
4. [CONSENSUS.md](CONSENSUS.md) — how nodes agree on the order.
5. [NETWORKING.md](NETWORKING.md) and [STORAGE.md](STORAGE.md) — how agreement survives processes and restarts.
6. [VM.md](VM.md), [ECONOMICS.md](ECONOMICS.md), [RPC.md](RPC.md) — the application surfaces.

## Conventions

- All integers are unsigned 64-bit unless stated; all encodings are
  little-endian for JSON-independent binary structures, big-endian for the
  byte-level number encodings inside `zencoding`/ZVM. Where this matters, the
  document says which.
- `Z` is the native token, `uz` is 10⁻⁸ Z (1 Z = 100,000,000 uz).
- Hashes are 32-byte SHA-256 digests, displayed as 64 lowercase hex characters.
- Addresses are `z1…` Bech32 strings over 20 raw bytes.
- Anything marked **LIMITATION** is a known bound of this build, not a bug.

## Known deviations from production grade

These are repeated in [../BLOCKERS.md](../BLOCKERS.md) and are not hidden:

- No commitment to the validator set inside blocks beyond `validators_hash`
  (which is computed from the parent state's active set).
- No light-client skipping or header-chain verification; syncing trusts the
  peer's blocks but re-validates every transaction against state.
- No transport encryption in P2P; framing has a CRC32C but no MAC.
- No state pruning during normal operation; snapshots are retained per height
  policy configured by the operator.
- Governance supports parameter changes and text proposals; treasury spending
  executes, but there is no on-chain treasury dashboard.
