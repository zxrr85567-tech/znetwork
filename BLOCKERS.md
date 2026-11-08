# Z NETWORK — BLOCKERS

Honest register of everything that is missing, weak, or impossible in this
environment. Nothing here is hidden behind optimistic wording: if a capability
is not real, it is listed.

**Overall status: development / testnet v1.0.0-rc1. This build must not be
presented as Mainnet-ready, audited, or production-deployed.**

Severity: **B1** = would be exploitable or consensus-breaking in production;
**B2** = functional gap that blocks a real deployment; **B3** = quality or
operability gap; **B4** = environment limitation.

---

## B-001 — No GitHub remote (B4)

`git remote -v` is empty and no authorized GitHub connector exists in this
environment. All work is committed locally in 12 commits. Consequences:

- no CI runs on push, no protected-branch review, no release hosting;
- "published" is **never** claimed for anything in this repository;
- the deliverable is the local git history plus the reproducible zip package.

*Unblocked by:* connecting an authorized GitHub connector and pushing, which
requires the account owner's credentials. Not attempted.

## B-002 — No external audit (B1)

No security review by a third party has happened. The cryptography is standard
library Ed25519/SHA-256/AES-GCM with RFC test vectors, and the consensus has unit
and multi-node tests, but **engineering evidence is not an audit**.

*Unblocked by:* a real audit engagement (out of scope: zero-budget policy).

## B-003 — No transport encryption in P2P (B1)

Frames carry a CRC32C for corruption, not a MAC or cipher. An on-path attacker
can read, drop, reorder and inject frames. Injected *consensus* content is still
signature-checked and re-validated, and injected blocks are re-executed against
local state, so integrity of state survives — but metadata, timing and traffic
volume are exposed, and an attacker can freely drop messages to cause liveness
failures.

*Unblocked by:* TLS or Noise in `znet`, with an authenticated session key bound to
the handshake public key.

## B-004 — No slashing / equivocation punishment (B1)

Conflicting votes are detected and never adopted, but a Byzantine validator that
equivocates loses nothing: its stake is not burned, it is not jailed, and there is
no evidence-submission transaction. The 100-block unbonding delay is the only
economic cost.

*Unblocked by:* storing signed conflicting votes as evidence in a block,
verifying it, and burning/jailing the offender. Not implemented; not simulated.

## B-005 — No persistent state database (B2)

State lives in an in-memory Merkle tree rebuilt from per-height snapshots. Memory
grows with distinct keys ever written, snapshots accumulate one per height, and
there is no pruning job in `znd run`. Fine for a testnet of this size; unacceptable
for a public chain.

*Unblocked by:* a disk-backed key-value store with a versioned history (the
`zstate` API is small enough to be reimplemented behind it).

## B-006 — In-memory index only (B3)

`zindexer` writes NDJSON and scans it for address queries. It is bounded
(500,000 records) and refuses foreign chain ids, but address lookups are linear.

*Unblocked by:* a real index store; SQLite is not available in this environment
as a dependency-free option, so this was a deliberate scope decision.

## B-008 — No peer scoring, ban list or rate limiting (B2)

A misbehaving peer is disconnected and can immediately reconnect. There is no
reputation, no ban, and no per-IP limit. RPC has no rate limiting either; it is
bound to loopback by default for that reason.

*Unblocked by:* a small peer manager with failure counters and a timed ban list,
plus a reverse proxy in front of RPC (documented in `deployment/`).

## B-009 — Consensus has no adaptive timeouts or explicit view-change (B2)

Timeouts are `min(2s × (round+1), 30s)` and `MaxRounds = 64`. A proposer that
keeps stalling wastes rounds rather than being replaced quickly. Liveness is
bounded but not fast under adversarial conditions.

*Unblocked by:* an explicit view-change message and a timeout schedule driven by
observed round progress.

## B-010 — Single-frame contract execution (B3)

ZVM has no cross-contract calls, no dynamic memory and 64-bit words. Contracts
cannot compose. Stated plainly in `specifications/VM.md`; it is a scope decision,
not a bug, but it means "smart contracts" here means a narrow contract layer.

## B-011 — Governance has no treasury spending and no deposit refund (B3)

Rejected proposals leave their deposit in the treasury account forever. The
treasury cannot spend. There is no proposal type to move funds.

*Unblocked by:* a spending proposal type with treasury authorisation; the state
already holds the treasury as a normal account.

## B-012 — No long-running soak or load test (B2)

The longest continuous run was ~143 blocks. Memory growth, log rotation under
sustained load, and throughput (transactions per second) are **not measured**, so
no performance number is claimed anywhere in this repository.

## B-013 — No mnemonic key derivation (B3)

Keys come from a 32-byte seed or from `crypto/rand`. There is no BIP-39 mnemonic,
no HD derivation, no hardware-wallet support. Devnet keys are deterministic test
keys and are labelled as such everywhere they are emitted.

## B-014 — Release artifacts are unsigned (B1 for any public release)

`SHA256SUMS` provides integrity, not authenticity: anyone who can rewrite the zip
can rewrite the sums. There is no signing key in this repository.

*Unblocked by:* generating a release key out-of-band and signing the manifest;
requires a key-custody decision that is out of scope here.

## B-015 — Documents are not translated / localized (B4)

All documentation is English-only.

## B-016 — Explorer is read-only and unauthenticated (B3)

It reads the local node's RPC on the same origin, holds no keys, has no write
path, and is served with a restrictive CSP. It has no login, no watchlists and no
pagination for long chains (it shows the last 10 blocks).

## B-017 — `zmetrics` counters are process-local (B3)

Counters reset on restart, and there is no aggregation across nodes. `/metrics` is
per-process Prometheus exposition; a real deployment needs a scraper and a
dashboard. No Grafana/Prometheus server was installed (none is available offline);
the exposition format is standard and was verified by test, not by scraping it
with a real Prometheus.

## B-018 — No container images (B4)

Docker is not available in this environment. Deployment material is shell
scripts and documentation, not images.

---

## Environment facts that shaped the work

| Constraint | Effect |
|---|---|
| No Go toolchain preinstalled; installed Go 1.24.4 into `~/.local` | reproducible via `scripts/bootstrap-toolchain.sh` |
| No third-party packages available offline | zero-dependency policy; hand-written Prometheus exposition, PBKDF2, Bech32 |
| No SQLite/Docker/Rust | NDJSON index, shell-based deployment, Go-only implementation |
| No GitHub connector | B-001: local git only |
| 2 vCPU, 992 MB `/tmp` tmpfs | test parallelism and e2e binaries sized accordingly |

## What would make this Mainnet-ready

In dependency order, none of which is done:

1. External audit of consensus, state transitions and cryptography (B-002).
2. Authenticated transport (B-003) and slashing with evidence (B-004).
3. Disk-backed state with pruning and a real index (B-005, B-006).
4. Peer scoring, rate limiting and RPC exposure behind a proxy (B-008).
5. Soak and load testing with published numbers (B-012).
6. Signed releases and a key-custody process (B-014).
7. Independent multi-operator testnet with real network conditions.
