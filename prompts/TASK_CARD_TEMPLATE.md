# TASK CARD — <TASK-ID>: <one-line title>

> The Lead fills every field. The Implementer must stop and return the card if
> any required field is missing. Append `prompts/00_shared_context.md` after
> `prompts/02_implementer.md` and this card when dispatching.

| Field | Value |
|---|---|
| Task ID | `<N-01 | B-003 | M-2026-11-08-01 | …>` |
| Source | `NEXT_ACTIONS.md N-xx` / `BLOCKERS.md B-xxx` / new (justify) |
| Backlog reason | <1–3 lines: why this now, quoting the backlog's own "Why"> |
| Risk class | `docs` / `ui` / `rpc` / `storage` / `network` / `consensus-safety` / `state-transition` / `crypto` |
| Owner packages | `<internal/znet>, <internal/znode>` (from MODULE_REGISTER.md) |
| Spec files that must stay in sync | `<specifications/NETWORKING.md>` |
| Depends on | <task IDs that must merge first, or "none"> |
| Dispatches to (parallel) | <none — default; justify if parallel> |

## First step (concrete, from the backlog)

<Copy/adapt from NEXT_ACTIONS.md — e.g. "extend
`internal/znode/multinode_test.go` with a `znet.Server` wrapper that can drop
messages to a chosen peer".>

## Work items (in order)

1. <failing test / spec edit / implementation unit>
2. …
3. …

## Done when (verifier-grade, each line must be provable by one command)

- [ ] <e.g. "partition scenario: no double commit at one height, 20/20 `-race` runs">
- [ ] <e.g. "healed partition converges within N rounds">
- [ ] `scripts/quality-gates.sh --full` PASS on the branch
- [ ] `specifications/<x>.md` §<y> reflects the new behaviour
- [ ] No new dependency: `git diff main...HEAD -- go.mod` empty

## Scope exclusions (explicit — anything here is out of bounds)

- <e.g. "do not change vote message encodings", "no refactors in zstore",
   "no doc edits outside the listed spec files (Archivist owns status docs)">

## Evidence the Verifier must produce

- <e.g. "paste live testnet height + tx hash", "paste 20/20 harness count",
  "paste VmRSS trend across 100k-block replay">

## Risk notes / threat model links

- <T-xx IDs from security/THREAT_MODEL.md touched, or "none">

## Accepted trade-offs (record what we are NOT solving here)

- <e.g. "ban list is in-memory only; persistence tracked under B-008 follow-up">
