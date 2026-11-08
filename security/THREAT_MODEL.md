# Z NETWORK — Threat Model

**Scope:** the code in this repository (node, consensus, execution, storage,
networking, RPC, wallet tooling).
**Out of scope:** operating-system compromise, physical access, social
engineering, and any deployed infrastructure (there is none).

This is a threat model written by the implementer, not an audit. Every
"mitigation" below is a property of the code with a test or a live verification
behind it; every "residual" is either a blocker or an accepted risk.

---

## Assets

| Asset | Why it matters |
|---|---|
| Validator private keys | sign votes; a stolen key can equivocate |
| User private keys / keystores | control funds |
| Chain state | balances, stakes, contracts |
| Consensus safety | two chains at one height destroys the value of the ledger |
| Consensus liveness | no blocks means no service |
| Transaction index / receipts | auditability and user trust |
| Release artifacts | supply-chain integrity |

## Adversaries

| Adversary | Capability assumed |
|---|---|
| A1 External network attacker | can read, drop, delay, reorder, inject TCP traffic |
| A2 Byzantine validator | up to < 1/3 of stake; can equivocate, withhold, stall |
| A3 Malicious client | sends arbitrary RPC and P2P messages |
| A4 Malicious contract author | deploys arbitrary bytecode |
| A5 Malicious delegator/proposer | manipulates staking or governance within the rules |
| A6 Supply-chain attacker | attempts to inject a dependency or modify a release |
| A7 Local attacker with file access | reads/writes files in the node's home directory |

---

## Threats and treatment

### Consensus and identity

| ID | Threat | Mitigation (verified) | Residual |
|---|---|---|---|
| T-01 | Two blocks committed at one height | BFT quorum > 2/3 + locking rule; `AdvanceRound` keeps the lock; unit tests for conflicting votes; `TestFourNodeNetworkReachesConsensus` compares block hashes | Requires < 1/3 Byzantine; **no slashing**, so an attacker pays only opportunity cost (B-004) |
| T-02 | Signature malleability / forgery | Ed25519 (RFC 8032, non-malleable), stdlib only, RFC vector test | none known |
| T-03 | Cross-network replay of a transaction | chain id inside `txID`; handshake rejects a peer on another chain id | none |
| T-04 | Same-network replay | nonce inside `txID`; exact nonce match at execution | none |
| T-05 | Vote replay across heights/rounds | vote digest covers chainID, height, round, step, block id; vote-set dedup | none |
| T-06 | Vote from a non-member or with mismatched key | membership checked against the set derived from parent state; address/key binding verified | none |
| T-07 | Engine height drift → node rejects its own proposals (liveness) | engine positioned absolutely from chain height (`SetHeight`) at commit, after failure and after sync | found only under CPU contention before the fix; now 12/12 `-race` runs clean |
| T-08 | Stalled proposer wastes rounds | round timeout `min(2s×(R+1),30s)`, `MaxRounds 64` | no explicit view-change; recovery is slower than ideal (B-009) |
| T-09 | Equivocating validator is not punished | conflicting votes recorded, never adopted | **no slashing** (B-004) |

### Networking

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-10 | Frame confusion / message smuggling | length bounded (4 MiB, checked before allocation), CRC32C, and the decoded message must re-encode to exactly the consumed bytes (`FuzzReadMessage`, 245k+ execs) | none known |
| T-11 | Eavesdropping / traffic analysis | none — plaintext TCP | **open** (B-003) |
| T-12 | Message injection / tampering in transit | consensus content is signature-checked and re-validated; blocks are re-executed locally | injected transactions are only as safe as their signatures; an attacker can drop traffic to cause liveness failures |
| T-13 | Gossip amplification / echo storm | SHA-256 dedup cache; never relay back to the sender | none |
| T-14 | Peer floods the node with work | per-message bounds (128 blocks), re-execution cost, idle timeout | no rate limiting or ban list (B-008) |
| T-15 | Duplicate connections / connection exhaustion | deterministic tie-break on initiator key; reconnect loop bounded by seed list | no connection cap beyond seeds |
| T-16 | Peer lies about height or serves bad blocks | blocks re-validated and re-executed; a bad block is rejected | a peer can waste a node's time |

### Execution and state

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-17 | Malicious contract crashes the node | `FuzzRun` (1.3M execs) found and fixed a `PUSH` bounds panic; no panics remain reachable in the interpreter path tested | formal verification absent |
| T-18 | Contract runs forever / consumes a block | `MaxSteps 200,000` + per-instruction gas charged before execution + block gas limit | none |
| T-19 | Failed transaction leaves partial state | native txs validate before mutating; contract storage is journaled and rolled back | none known; asserted by tests |
| T-20 | Silent supply inflation | `VerifySupply` after every block (on by default) | none — mismatch aborts the block |
| T-21 | Non-canonical encoding creates two ids for one object | `Done()` rejects trailing bytes; `FuzzDecodeTx` asserts re-encode equality | none |
| T-22 | Numeric overflow | checked `AddU64`/`MulU64`/`mulDiv` everywhere; fee and gas overflow rejected before use | none known |

### Keys and wallet

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-23 | Keystore brute-force | PBKDF2-HMAC-SHA256 600,000 iterations, min 10-char password, fresh 32-byte salt | password strength is the user's responsibility |
| T-24 | Keystore downgrade (crafted file with weak KDF) | `Decrypt` rejects iterations < 600,000 and unknown algorithms; tested | none |
| T-25 | Wrong password oracle | GCM tag makes wrong password and corruption indistinguishable (`ErrDecrypt`) | none |
| T-26 | Keystore identity swap | address is GCM additional authenticated data; tested | none |
| T-27 | Seed leaks into a file or log | seed is never serialised; test asserts it does not appear in the keystore; wallet takes passwords from `--password-file` or env, never argv | logs from third-party tools outside our control |
| T-28 | Production keys used in tests | devnet keys are derived from labelled seeds, written under git-ignored `data/`, and labelled TEST ONLY | operator error remains possible |
| T-29 | Weak randomness | all randomness from `crypto/rand`; `ErrNoEntropy` aborts rather than falling back | none |

### RPC and clients

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-30 | Massive request bodies | 1 MiB body limit, 16 KiB header limit, server timeouts | none |
| T-31 | Batch amplification | max 20 requests, 1,000 results | none |
| T-32 | Unauthorised write | only `z_sendRawTransaction` writes; bearer token when `ZND_RPC_TOKEN` is set | token is a shared secret in an env var; no per-user auth (B-008) |
| T-33 | Explorer loads hostile content | single inline document, restrictive CSP `default-src 'none'`, no external resource, no framing | none |
| T-34 | RPC exposed to the internet | binds `127.0.0.1` by default; deployment material documents a reverse proxy | operator error |
| T-35 | Query scanning blocks the node | limits clamped (`z_getBlocks` ≤ 1000, mempool ≤ 200); per-call chain lock held briefly | no rate limiting |

### Supply chain and release

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-36 | Malicious dependency | zero third-party modules; `go.mod` has no `require` | stdlib compromise (out of scope) |
| T-37 | Release artifact modified | deterministic zip, per-file SHA-256 manifest, `SHA256SUMS` re-verified after writing, byte-identical rebuild verified | **unsigned** — integrity only (B-014) |
| T-38 | Chain data or keys ship in a package | packaging excludes `data/`, `bin/`, `*.zip`, `*.log`, `*.key.json`; tested | none |
| T-39 | Package built from uncommitted state | `zrelease` refuses a dirty tree (observed refusing during this work) | `--allow-dirty` exists for developers and is visible in the command |

### Storage

| ID | Threat | Mitigation | Residual |
|---|---|---|---|
| T-40 | Corrupt block log silently truncates history | per-record SHA-256; reader stops at the last good record and reports; tail repair is explicit | a maliciously rewritten log can only be detected by comparing with peers |
| T-41 | Corrupt snapshot produces different state | digest verified and StateRoot recomputed; mismatch refuses the snapshot | none |
| T-42 | Local attacker edits files (A7) | keystores are 0600 and authenticated; chain data has no MAC, so A7 can rewrite history on one node | unavoidable without a trusted anchor; peers provide cross-check |

---

## Accepted risks (with eyes open)

1. **Plaintext P2P** — accepted for a development network, blocker for Mainnet.
2. **No slashing** — accepted for now; means Byzantine behaviour is not
   economically deterred.
3. **Unaudited** — accepted only in the sense that the system is labelled a
   testnet everywhere.
4. **Loopback-by-default RPC** — the node is not intended to be internet-facing.
5. **In-memory state** — a crash loses nothing (blocks and snapshots are on
   disk) but restart time grows with chain length.

## How to verify the claims

```bash
scripts/quality-gates.sh --full        # all automated evidence
scripts/fuzz-campaign.sh 20            # the fuzzing table
go test -run 'TestKeystore|TestDecodeTx|Fuzz' ./...   # the specific attacks above
```

Cross-reference: [../BLOCKERS.md](../BLOCKERS.md) for every open item, and
[../TEST_REPORT.md](../TEST_REPORT.md) for the measured results.
