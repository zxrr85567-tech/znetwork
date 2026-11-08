# Z NETWORK — Agent Prompt System

Role-specific system prompts for AI agent sessions (Arena agent models) that
continue developing and completing this repository. Each file in this directory
is designed to be pasted/loaded **as the system prompt of one agent session**;
together they form a multi-agent pipeline with separation of duties.

---

## 1. Files

| File | Role | Use it when |
|---|---|---|
| [`00_shared_context.md`](00_shared_context.md) | Shared constitution | **Appended to every role prompt** — ground rules, read-first list, definition of done |
| [`01_lead_orchestrator.md`](01_lead_orchestrator.md) | Lead / orchestrator | Starting a work cycle: choosing the next `NEXT_ACTIONS.md` item, writing task cards |
| [`02_implementer.md`](02_implementer.md) | Implementer | Executing exactly one task card with TDD |
| [`03_verifier.md`](03_verifier.md) | Verifier | Running every quality gate and producing measured evidence |
| [`04_reviewer.md`](04_reviewer.md) | Reviewer | Adversarial review of a diff before it can merge |
| [`05_archivist.md`](05_archivist.md) | Archivist / releaser | Updating status documents, packaging releases, pushing to GitHub |
| [`TASK_CARD_TEMPLATE.md`](TASK_CARD_TEMPLATE.md) | Template | The Lead's work unit handed to an Implementer |

## 2. How to run one cycle

1. **Session A — Lead.** System prompt = `01_lead_orchestrator.md` +
   `00_shared_context.md`. It reads `NEXT_ACTIONS.md`, picks one item (respecting
   dependency order N-01 → N-02 → …), and writes a filled task card.
2. **Session B — Implementer.** System prompt = `02_implementer.md` +
   `00_shared_context.md` + the filled task card. It implements with tests and
   stops at `scripts/quality-gates.sh` passing locally. Commits on a branch.
3. **Session C — Verifier.** System prompt = `03_verifier.md` + `00_shared_context.md`.
   It re-runs everything from a clean checkout and emits an evidence block.
4. **Session D — Reviewer.** System prompt = `04_reviewer.md` + `00_shared_context.md`.
   It reviews the diff and returns `APPROVE` or `REQUEST-CHANGES` with findings.
5. **Session E — Archivist.** System prompt = `05_archivist.md` + `00_shared_context.md`.
   It reconciles `PROJECT_STATE.md`, `TEST_REPORT.md`, `BLOCKERS.md`,
   `NEXT_ACTIONS.md`, `MODULE_REGISTER.md` and `EXECUTION_LOG.md` with what
   actually happened, merges to `main`, and pushes.

Single-agent mode: one session may concatenate `00_shared_context.md` with a
role prompt, but **no session may play Implementer and Reviewer on its own
diff** — that separation is the point of the system.

## 3. Backlog source

The backlog is the repository's own documents, in this priority order:

1. `BLOCKERS.md` — B1/B2 items are what "complete" means.
2. `NEXT_ACTIONS.md` — N-01 … N-10, in dependency order, each with a concrete
   first step and a "done when" clause.
3. `PROJECT_STATE.md` §2 — the capability matrix rows marked *absent*.

Never invent a new roadmap item before checking these three.

## 4. Credentials for pushing (operator note)

- The operator supplies a GitHub token **per environment** (this session:
  configured in the git remote of the workspace). Agents use it only as
  credential for `git push`/`git pull` — via the existing remote URL or an
  environment variable (`GH_TOKEN` / `ZNETWORK_GIT_TOKEN`).
- **Never write the token into any tracked file, prompt, log, commit, issue or
  CI log.** GitHub scans pushed content for tokens and will revoke them; a
  leaked token breaks every future upload. If you need it in a remote URL,
  keep it in `.git/config` (untracked) as the clone already does.
- Before pushing, run `git diff --cached` and confirm no credential-shaped
  string (`ghp_`, `gho_`, `github_pat_`) is staged.

## 5. Definition of done (global)

A repository state is "done" for a task only when **all** hold:

- [ ] Code + tests merged to `main`; `scripts/quality-gates.sh --full` passed
      in the session that produced it, output captured.
- [ ] Spec files under `specifications/` updated if behaviour changed.
- [ ] `PROJECT_STATE.md`, `TEST_REPORT.md`, `BLOCKERS.md`, `NEXT_ACTIONS.md`
      and `EXECUTION_LOG.md` reconciled — rows flipped from *absent* only with
      evidence attached.
- [ ] Pushed to `origin/main`; CI (gofmt, vet, build, test, coverage, race,
      fuzz smoke) green.
- [ ] Honest sentence at the end: what was NOT proven.
