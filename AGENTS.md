# AGENTS.md — Instructions for AI agent sessions (Arena agent models)

This repository is built and maintained by AI agent sessions. Every agent that
works on Z NETWORK **must** load one of the role prompts in [`prompts/`](prompts/)
as its system prompt before touching code. This file is the entry point; the
role prompts are the specification of how we work.

---

## 1. Quick start (every session, before anything else)

```bash
git pull --ff-only                              # never diverge from origin/main
cat PROJECT_STATE.md NEXT_ACTIONS.md BLOCKERS.md # honest ground truth
cat AGENTS.md prompts/README.md                  # how we work
```

Then load the role prompt for the job you were assigned:

| Role | System prompt | Responsibility |
|---|---|---|
| Lead / orchestrator | [`prompts/01_lead_orchestrator.md`](prompts/01_lead_orchestrator.md) | Picks the next task, writes task cards, dispatches, integrates |
| Implementer | [`prompts/02_implementer.md`](prompts/02_implementer.md) | TDD implementation of exactly one task card |
| Verifier | [`prompts/03_verifier.md`](prompts/03_verifier.md) | Runs every gate; produces measured evidence only |
| Reviewer | [`prompts/04_reviewer.md`](prompts/04_reviewer.md) | Adversarial review of the diff (safety, encoding, spec) |
| Archivist / releaser | [`prompts/05_archivist.md`](prompts/05_archivist.md) | Keeps documents truthful, packages releases, pushes |

All roles share [`prompts/00_shared_context.md`](prompts/00_shared_context.md) —
read it once per session; it is the constitution that overrides any temptation
to take shortcuts.

## 2. Non-negotiable rules (one-paragraph summary)

1. **Evidence over claims.** Never write a number, status, or "passes" that a
   command in this session did not produce. If it did not run, say so.
2. **Zero third-party dependencies.** `go.mod` must stay dependency-free.
   Hand-roll what you need (the repo already hand-rolls PBKDF2, Bech32 and
   Prometheus exposition).
3. **`specifications/` is normative.** Behaviour change without a spec change
   is a bug. Constants quoted in the spec are read from the code.
4. **No floats in the protocol.** Integer micro-units, overflow-checked math.
5. **Quality gates before every commit:** `gofmt`, `go vet`, `go build`,
   `go test`, `-race` for concurrency work. `scripts/quality-gates.sh --full`
   before a merge to `main`.
6. **This is a testnet.** Never write Mainnet-ready, audited, or production
   claims. `BLOCKERS.md` is the source of truth about what is missing.
7. **Never commit credentials.** Tokens arrive per-session via environment
   (see `prompts/README.md` §4); they live in git remotes or env vars only,
   never in a tracked file.

## 3. Workflow

```
operator assigns a role
        │
        ▼
  LEAD ──writes task card (prompts/TASK_CARD_TEMPLATE.md)──► IMPLEMENTER
        ▲                                                      │
        │                                                      ▼
        │                                                   VERIFIER (gates + evidence)
        │                                                      │
        │                                                      ▼
        │                                                   REVIEWER (diff verdict)
        │                                                      │
        └────── status update ◄── ARCHIVIST (docs truthful) ◄───┘
                          │
                          ▼
              commit → push origin/main → CI green
```

A task is **done** only when: implementation + tests exist, all gates pass
with captured output, review verdict is `APPROVE`, documents that describe
the changed area are updated, and CI on `origin/main` is green.

## 4. Environment

```bash
source scripts/env.sh          # pinned Go toolchain (scripts/bootstrap-toolchain.sh installs it)
scripts/quality-gates.sh       # fast gates
scripts/quality-gates.sh --full  # + race + fuzz smoke + coverage
scripts/testnet.sh up --validators 4 --block-time 600   # live network on loopback
```
