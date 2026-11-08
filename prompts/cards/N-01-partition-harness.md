# TASK CARD — N-01: Adversarial network test harness (partition + equivocation)

| Field | Value |
|---|---|
| Task ID | `N-01` |
| Source | `NEXT_ACTIONS.md N-01` (highest value / lowest cost) |
| Backlog reason | Consensus safety and liveness are verified only on happy paths; no test partitions a live network or runs a Byzantine validator (TEST_REPORT §9.2). |
| Risk class | `consensus-safety` (test-only surface + one test hook in `znet`) |
| Owner packages | `internal/znet` (filter hook + unit test), `internal/znode` (harness test) |
| Spec files that must stay in sync | `specifications/NETWORKING.md` (message-delivery section) |
| Depends on | none |
| Dispatches to (parallel) | none |

## First step (from the backlog)

Extend the multi-node tests with a `znet.Server` wrapper that can drop messages
to a chosen peer, then implement the three scenarios. **Amendment by Lead:**
`AttachNetwork` takes the concrete `*znet.Server`, so the "wrapper" is realised
as a `MessageFilter` hook on `znet.Server` (nil in production), and the harness
lives in a new file `internal/znode/partition_test.go` in the same package,
plus a focused unit test in `internal/znet`.

## Work items (in order)

1. `znet`: `MessageFilter` type + `Server.SetFilter`; drop silently in
   `Peer.Send` (outbound) and `readLoop` (inbound). Nil filter = current
   behaviour, byte-for-byte.
2. `znet` unit test: filter drops both directions; `SetFilter(nil)` heals.
3. `znode/partition_test.go` harness: 4 validators, line topology, cut between
   groups {0,1} and {2,3} via per-server filters keyed on the remote handshake
   public key; drop counters prove the cut is engaged.
4. Scenario 1 — **safety under partition**: after the cut settles, no node's
   height advances (neither group has 2/3 power) and no two nodes hold
   different blocks at any common height.
   *Amendment (found by Implementer during item 5):* `Engine.signed` keeps only
   the first vote's block id, so `Equivocation.First` arrives without a
   signature even though the code comment promises evidence that is
   "self-contained and independently verifiable". The map must retain the full
   first vote. This changes no consensus rule, no wire format and no vote
   counting — evidence bookkeeping only — and is required by the Done-when
   below.
5. Scenario 3 — **Byzantine equivocation during the stall** (engines pinned):
   a test peer dials all four nodes and feeds conflicting prevote pairs signed
   by validator 0 (two distinct block ids, same height/round/step). Assert:
   (a) every node records `Evidence()` with both signed votes retained and
   distinct; (b) a synchronous second conflicting vote is rejected with
   `ErrEquivocation` — the rejection happens before power is added, so power
   is counted once; (c) no chain ever contains either forged block id.
6. Scenario 2 — **liveness after heal**: remove the filters; all four converge
   to a common head, heights advance ≥ 5 past the stall, all heights agree
   across nodes.
7. `specifications/NETWORKING.md`: document the filter as a test hook (no
   protocol change).

## Done when (verifier-grade)

- [ ] `TestAdversarialPartitionHarness` passes **20 consecutive**
      `go test -race -run TestAdversarialPartitionHarness ./internal/znode/ -count=20` runs.
- [ ] `go test -race ./internal/znet ./internal/znode` green (includes the new
      znet filter unit test).
- [ ] `scripts/quality-gates.sh --full` PASS on the branch.
- [ ] `git diff main...HEAD -- go.mod` empty (zero deps preserved).
- [ ] `specifications/NETWORKING.md` updated; no other spec claims changed.

## Scope exclusions

- No transport encryption (B-003), no slashing/evidence transaction (B-004),
  no peer scoring/ban list (B-008) — this card is test infrastructure only.
- No changes to consensus rules, encodings, or wire format; the filter is a
  local drop hook, not a protocol feature.
- No edits to status documents (Archivist owns them after the verifier report).

## Evidence the Verifier must produce

- Pasted output of the 20× `-race` run (pass count), the full
  `quality-gates.sh --full` tail, and the new test counts / coverage delta.

## Risk notes / threat model links

- T-2x family: an on-path attacker can drop messages (B-003); this harness
  exercises exactly that capability at the application layer.
- The filter must never engage in production: default is `nil`, verified by
  the znet unit test and by the existing happy-path suite running unfiltered.

## Accepted trade-offs

- Cross-group gossip marks messages as deduped before the drop, so a message
  seen only during a partition is not re-relayed after heal; recovery instead
  happens via round timeouts and the block-sync path (asserted by scenario 2).
- Handshakes are not filtered (the hook sits above the raw connection);
  connections stay established across the cut, matching a black-hole
  partition rather than a link-down partition.
