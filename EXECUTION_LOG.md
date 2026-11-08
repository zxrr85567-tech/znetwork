# Z NETWORK — EXECUTION LOG

Chronological record of what was actually done, in the order it happened, with
the verification that followed. Written from the session's own commits and test
output; anything that did not run is labelled as such.

---

## Phase 0 — Environment discovery

- No prior Z Network artifacts existed on disk. Go was not installed.
- Installed Go 1.24.4 to `~/.local/go` (78,559,214 bytes, verified by use).
- Recorded findings and environment limits in `ENVIRONMENT_DISCOVERY_REPORT.md`
  (EDR-0001).
- Established the constraints that shaped everything after: no Docker, no SQLite,
  no Rust, no GitHub connector, 2 vCPU, 992 MB `/tmp`.

## Phase 1 — Foundations (wave 0–1)

- `zencoding`: canonical writer/reader with length bounds and a strict `Done()`.
- `zcrypto`: SHA-256 domain digests, Ed25519 keys/addresses, Bech32, PBKDF2,
  AES-GCM keystore. RFC 8032, BIP-173 and RFC 7914 vectors pass.
- `zmerkle`: sorted-leaf tree with odd-node promotion, proofs and a proof codec.
- `ztypes`: transactions, blocks, receipts, genesis, accounts, validators,
  delegations, proposals, params — all with canonical encodings.
- Fixed: merkle proof capacity, params aliasing, genesis reproducibility under
  reordering.

## Phase 2 — State and storage (wave 2)

- `zstate`: single Merkle tree keyed by class byte; snapshots with SHA-256
  trailers; inclusion and absence proofs.
- `zstore`: block log (`u32 len | body | sha256`), state snapshots, tail repair,
  reorg rewrite.
- Fixed: `Prove` returned the leaf hash where callers needed the anchor root; a
  leaf-hash comparison silently verified nothing.

## Phase 3 — Execution (wave 3)

- `zvm`: stack machine, gas table, assembler, journaled storage.
- `zexec`: begin/end block, transaction dispatch, fees, staking, governance,
  supply invariants.
- Fixed: arithmetic shift semantics, `SSTORE` journaling, jump bounds,
  invalid-vs-failed transaction semantics, `VerifySupply` delegation accounting.

## Phase 4 — Consensus (wave 4)

- `zconsensus`: validator sets with stake-weighted quorum, vote sets with
  conflict detection, locking, propose/prevote/precommit/commit, commit
  certificates, deterministic proposer selection.
- Fixed: `Header.Hash()` field coverage, `Commit`/`CommitSig` encoding, vote
  digest no longer signed `TimeMs`, chain id bound into the validator set.

## Phase 5 — Node and network (wave 5)

- `znode`: chain pipeline, block production from the mempool, single validation
  path (`applyTo`), idempotent commit, receipts retention.
- `znet`: TCP framing with CRC32C, symmetric handshake with chain-id check,
  gossip with SHA-256 dedup, deterministic duplicate-connection tie-break,
  reconnect loop.
- Fixed: recursive engine lock, frame CRC off-by-one, duplicate dials, commit
  certificate matching across rounds, future-round vote buffering, lock
  retention on round advance (the 4-node stall).
- Verified: 4-node consensus, `-race` clean, 10 consecutive reruns.

## Phase 6 — Interfaces (wave 6)

- `zrpc`: 24 JSON-RPC methods, error taxonomy, batching, auth for writes
  (15 tests, then 18).
- `zindexer`: NDJSON index with atomic checkpoint and re-execution repair.
- Fixed: indexer had no receipts after a fresh sync (persist `receipts.log`),
  pending nonce semantics, metrics error counting.

## Phase 7 — Binaries and SDK (wave 7)

- `cmd/znoded` (`init`, `run`, `version`), `cmd/zwallet` (13 subcommands),
  `cmd/zcli` (inspection + devnet/keygen), `internal/zsdk` Go client.
- Process-level e2e: real binaries, real processes, wallet send, commit, index on
  disk, restart recovery, two-node agreement — **6 tests green**.
- Fixed: `EncryptKey(…, 0)` rejected every caller (0 is now "default 600k").

## Phase 8 — Test-completeness wave

- Added suites for `zstore`, `zmempool`, `znet`, `zsdk`, `zbuild`, `zmetrics`,
  plus deep tests for `ztypes`, `zstate`, `zcrypto`, `znode`.
- Bugs found and fixed by these tests: `DecodeTx` trailing bytes, `ParseUZ`
  accepting `"."`/`"1."`, node binary leaks in `/tmp`.
- Coverage rose 61.0% → 63.6% overall, with the weakest packages brought up
  (`zcrypto` 51.3% → 88.2%, `znode` 66.2% → 72.5%).

## Phase 9 — Fuzzing

- Five targets: `FuzzDecode`, `FuzzDecodeTx`, `FuzzReadMessage`, `FuzzProofDecode`,
  `FuzzRun`.
- **Found:** a `PUSH`-without-operand panic in the VM (a 1-byte program crashed
  the interpreter). Fixed with a bounds check and a committed regression input.
- **Found:** frame length prefixes could over/under-declare by a byte. The fuzz
  target now asserts consumed-length exactness.
- Final campaign: 3,726,218 executions, zero crashes.

## Phase 10 — The stall that mattered

- Under `-race` with CPU contention, a 4-node network stalled roughly 1 run in 6,
  with logs showing `building height 7 on head 2`.
- Root cause: the consensus engine kept its own height counter, incremented in
  three places; under load it drifted above the chain and then rejected its own
  proposals forever.
- Fix: `SetHeight(height, set)` positions the engine absolutely; the chain is the
  only height authority at commit, after failure, and after sync.
- Verified: 12 consecutive `-race` multi-node runs with 0 failures, then 6
  parallel batches (24/24 package runs) with 0 data races.

## Phase 11 — Operations surface

- `zmetrics` + `/metrics` (hand-written Prometheus 0.0.4, no dependency).
- Fixed: `votes_cast_total` and `heights_completed_total` were declared but never
  incremented; after wiring, a live validator reported height 25, blocks 25,
  votes 25, heights 25.
- `znd --moniker` added so multi-node logs and metric labels are distinguishable.
- `scripts/testnet.sh`: one-command 4-validator network with a status table.

## Phase 12 — Explorer and live verification

- Single-file explorer served by `znd` (`/explorer/`), same-origin only, strict
  CSP, no external resources.
- `web/explorer/build-harness.js` + `live-ui-check.js` run the page's **own**
  JavaScript against a live node: 4 head cards, 10 block rows, 12 block fields,
  account balance, transaction execution and events, 0 non-relative fetches.
- Fixed on the way: `z_getTransaction` never returned receipts, so the execution
  panel could never populate. Added `ReceiptBackend` + `z_getReceipt`; verified
  against a real deploy receipt.

## Phase 13 — Live end-to-end on four validators

- Fresh devnet, 4 validators, 600 ms blocks.
- Seven transactions committed and read back: transfer ×2, delegate, undelegate
  (unbonding entry `release_height 113`), contract deploy (event + 46,712 gas +
  output `0000000000000007`), governance proposal, yes vote (`yes_uz 600000000`).

## Phase 14 — Packaging and documentation

- `zbuild` + `cmd/zrelease`: deterministic zip, manifest, verified `SHA256SUMS`,
  dirty-tree refusal.
- Two independent builds of the same commit produced **identical SHA-256**
  (`763d57ea…`) — reproducibility is measured, not asserted.
- Wrote `specifications/` (11 documents), `security/THREAT_MODEL.md` (T-01…T-42),
  `MODULE_REGISTER.md`, `DEPENDENCY_GRAPH.md`, `INTEGRATION_REGISTER.md`,
  `TEST_REPORT.md`, `BLOCKERS.md`, `PROJECT_STATE.md`, `NEXT_ACTIONS.md`, this log,
  and the release manifest.
- Writing the specs corrected the docs against the code in four places (governance
  quorum, deposit handling, payload cap, ZVM output) — recorded in the commit
  message rather than quietly fixed.

---

## Phase 15 — Agent prompt system and adversarial harness (2026-11-08, Arena sessions)

- Added `AGENTS.md` + `prompts/` (shared constitution, lead / implementer /
  verifier / reviewer / archivist role prompts, task-card template) and pushed
  them: `origin/main` now exists, CI (gofmt, vet, build, test, coverage, race,
  fuzz smoke) green on every push — **B-001 closed** with evidence.
- **N-01 done.** `internal/znode/partition_test.go` +
  `znet.Server.SetFilter` (test hook, nil in production):
  - partition {0,1}|{2,3} on a live 4-validator line network: drop counter
    proves the cut engaged; neither group advances a height (no 2/3 power);
    no fork at any common height;
  - byzantine peer speaks the real `MsgVote` wire path with conflicting
    prevotes signed by validator 0: all four engines record evidence whose
    both votes verify independently; a fresh conflicting pair is rejected
    with `ErrEquivocation` **before** the power is added; forged block ids
    never appear in any chain;
  - heal: all four converge past the stall on one consistent chain;
    connections survived (black hole, not link down).
- Evidence-quality fix found by the harness: `Equivocation.First` retained
  only a block id although the comment promised self-contained evidence —
  `Engine.signed` now stores the full first vote (no consensus rule, wire
  format or counting change).
- Measured: 20/20 consecutive `-race` runs of the harness (123.9 s),
  `quality-gates.sh --full` green, 218 tests, 63.3% coverage, `go.mod`
  untouched (zero dependencies).
- Honest limits of this phase: single host; application-layer drops, not
  packet loss; one Byzantine validator, not adaptive; no encryption (B-003),
  no slashing (B-004).

---

## Things that did not happen (stated plainly)

- No GitHub push: no connector, no remote (B-001) — *superseded 2026-11-08:
  the repository now has a GitHub remote with green CI; B-001 closed.*
- No external audit (B-002).
- No slashing, no transport encryption, no disk-backed state, no light client.
- No soak test, no load test, no measured TPS.
- No container images (no Docker).
- No claim of Mainnet readiness anywhere in this repository.
