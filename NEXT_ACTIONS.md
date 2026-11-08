# Z NETWORK — NEXT ACTIONS

Ordered by dependency. Each item states the concrete first step, so it can be
started without re-deriving context. Nothing here is in progress right now.

---

## N-01 — Automated adversarial network test (highest value / lowest cost) — DONE 2026-11-08

**Closing evidence:** `internal/znode/partition_test.go`
(`TestAdversarialPartitionHarness`, card in `prompts/cards/`) drives a live
4-validator network through all three scenarios: (1) a black-hole partition of
{0,1}|{2,3} via the new `znet.Server.SetFilter` hook — no height advances and
no fork appears while neither group holds 2/3 power; (3) a byzantine peer feeds
conflicting prevotes signed by validator 0 over the real wire path — every
node records independently verifiable evidence, a fresh conflicting pair is
rejected with `ErrEquivocation` before counting, and no chain ever contains a
forged block; (2) after healing, all four converge past the stall on one
consistent chain. **20 consecutive `-race` runs passed** (123.9 s total,
commit `16d000a`); `scripts/quality-gates.sh --full` green (218 tests,
63.3% coverage). One evidence-quality fix rode along: `Equivocation.First`
now retains the full signed vote (it previously kept only the block id).

**Remaining gap (honest):** single-host only — no real latency, clock skew or
independent operators (see N-10); the filter exercises an application-layer
drop, not actual packet loss (transport encryption remains B-003).

## N-02 — Slashing with evidence (BLOCKERS B-004)

**Why:** equivocation is detected and ignored, but costs nothing.

**First step:** add `ztypes.EquivocationEvidence{voteA, voteB}`, a new transaction
type `evidence_submit`, and an `zexec` handler that verifies two signed votes from
the same validator at the same height/round/step with different block ids, then
burns a configured fraction of self-bond and jails the validator.

**Done when:** a Byzantine validator in the N-01 harness loses stake, and the
supply invariant still balances.

## N-03 — Authenticated transport (BLOCKERS B-003)

**First step:** wrap `znet.Peer.Conn` in a session that runs an X25519 + payload
encryption handshake (or TLS with a self-signed cert pinned by the handshake
public key), keeping `ReadMessage`/`WriteMessage` signatures unchanged.

**Done when:** `TestTwoServersExchangeMessages` still passes over the encrypted
transport and a test asserts plaintext is not visible on the wire.

## N-04 — Persistent state store (BLOCKERS B-005)

**First step:** define the `zstate` backend interface (`Get/Put/Delete/Iterate` +
snapshot/rollback), implement it in memory (current behaviour) and add a second
implementation over a single file with an on-disk index; make `zstate.New()`
select via a backend parameter.

**Done when:** the full suite passes against both backends and a 100,000-block
synthetic replay shows bounded memory.

## N-05 — Soak and load testing (BLOCKERS B-012)

**First step:** run `scripts/testnet.sh up` for one hour, sampling `/metrics`
every 10 s into a CSV, then report height/latency/memory trend and any round
timeouts.

**Done when:** a one-hour run has zero consensus failures and a working memory
baseline is published. Do not publish TPS until it is measured.

## N-06 — RPC hardening (BLOCKERS B-008)

**First step:** token-bucket rate limiter per remote address on `/` and a peer
manager with a timed ban list in `znet`.

**Done when:** a loop of 10,000 requests is throttled without dropping legitimate
traffic, and a banned peer cannot reconnect for the ban window.

## N-07 — Release signing (BLOCKERS B-014)

**First step:** produce a minisign/age keypair out of band (not in this repo), sign
`SHA256SUMS`, and document verification in `specifications/RELEASE_PROCESS.md`.

**Blocked by:** key-custody decision (owner action, not an engineering task).

## N-08 — Governance treasury spending (BLOCKERS B-011)

**First step:** add `GovProposalSpend{recipient, amount}` with the same tally path;
execute the transfer from the treasury account on pass.

## N-09 — Explorer pagination and search depth

**First step:** add `z_getBlocks` cursor paging in the UI and an address-history
view backed by `zindexer.ByAddress`.

## N-10 — Independent multi-operator testnet

**Why:** every network measurement so far is on one host, which cannot expose
clock skew, real latency, or independent operator error.

**First step:** document the join procedure (`genesis.json` + seed address +
`znd run`), then have a second operator join. Requires infrastructure outside
this environment (zero-budget policy; see BLOCKERS).

---

## Maintenance tasks that should not be forgotten

- `gofmt`/`go vet` are gated; keep them gated.
- The fuzz corpus in `testdata/` must stay committed — it is the regression set.
- `specifications/` is normative: **changing behaviour without updating the spec
  is a bug.** Every constant quoted there was read from the code.
- Coverage of `zsdk` (33.5%) is low by design (network paths tested end to end),
  but new SDK methods should come with an e2e assertion.
- `znd run` does not prune snapshots; a long-running node grows in `data/states/`.
  Add `--prune-states N` when N-04 lands.

---

## Handover note (v1.0.0-rc3)

Artifacts for continuing this work outside this environment:

| File | Use |
|---|---|
| `releases/znetwork-full-history.bundle` | `git clone znetwork-full-history.bundle znetwork` — complete history |
| `releases/znetwork-v1.0.0-rc3.zip` | plain source snapshot, 109 files |
| `releases/SHA256SUMS` | verify both before trusting them |

Both are built from the final commit of this repository (its hash is recorded in
`releases/znetwork-v1.0.0-rc3.manifest.json`) and were verified: the bundle clones
and builds, the zip extracts, builds, and passes `internal/znode` + `internal/e2e`.
`releases/` itself is untracked build output — regenerate with
`scripts/release.sh <version>` after making changes so the manifest hash always
matches the code it describes.
