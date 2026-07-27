# Ghost Real-World Eval Suite — Design

## Goal

Ghost has quantitative benchmarks (`internal/bench`, LongMemEval-S) for retrieval quality, but nothing that evaluates the *whole* system the way a real user experiences it: does session-start injection actually help, does search surface the right memory when it matters, do `ghost resolve`/`ghost supersede` behave sensibly over a real multi-session arc, and — critically — where does using Ghost's MCP tools actually feel bad? This eval suite produces one honest report combining quantitative scores with qualitative "frustration points" flagged by the agents that hit them, not just pass/fail.

This is a one-off diagnostic tool (a Workflow script + supporting docs), not a permanent CI gate. It runs on demand against an isolated scratch environment and produces a dated report.

## Isolation

Every eval run gets a scratch dir `/tmp/ghost-eval/<run-id>/` with:
- `data/` — becomes `XDG_DATA_HOME` for that run's `ghost` processes (so `internal/config.DataDir()` resolves to `<run-id>/data/ghost/`, containing an isolated `ghost.db`)
- `config/` — becomes `XDG_CONFIG_HOME`, seeded with a `ghost/config.yaml` copying only the `api.key` value from the real `~/.config/ghost/config.yaml` (resolve/supersede require `ANTHROPIC_API_KEY` — no fallback provider exists yet). Nothing else is copied.

All `ghost` invocations in the eval (CLI commands run directly by the harness, and the `ghost mcp` subprocess used by live tester agents) run with `XDG_DATA_HOME`/`XDG_CONFIG_HOME` pointed at that scratch dir.

**Live agents do not run as Workflow-launched `agent()` subagents.** A Workflow/Agent-tool subagent inherits the parent session's already-established MCP connections at startup — a worktree-local `.mcp.json` does not override this (confirmed by direct experiment, see below), so an `agent()` call has no way to reach an isolated `ghost` instance. It would also never fire the session-start hook, which is the actual mechanism the storyline module (below) needs to test.

Instead, each live-agent unit (one replay transcript, one storyline session, one stress scenario) is a **headless `claude -p` subprocess**, launched via Bash from inside an orchestrating `agent()` call:
```
claude -p \
  --mcp-config <scratch-run>/mcp.json \
  --strict-mcp-config \
  "<the live agent's task prompt for this unit>"
```
`<scratch-run>/mcp.json` declares a single `ghost` server whose `command` is `ghost-wrapped <run-id> <ghost-bin> mcp` — so the subprocess's `ghost` MCP server is the scratch-wrapped binary, not the real one. `--strict-mcp-config` ensures *only* this server is visible; the session's own already-configured `ghost` connection is excluded entirely. Because `claude -p` starts a real session, the session-start hook fires normally, so `ghost_project_context` injection is exercised for real — not called manually by a scripted agent.

At the end of a run, the entire `/tmp/ghost-eval/<run-id>/` dir is deleted. Nothing persists in the real DB, config, or Obsidian vault.

**Isolation was verified before any live-agent phase runs at scale**, via a throwaway experiment on 2026-07-27:
- First attempt: a Workflow `Agent` subagent launched in a worktree with a scratch-wrapped `.mcp.json` called `ghost_memory_save` — the row landed in the **real** production DB (`~/.local/share/ghost/ghost.db`), and the scratch DB directory was never even created. **Confirmed broken.** The leaked row (and its parent `projects` row) was deleted from the real DB after verification.
- Second attempt: `claude -p --mcp-config <scratch>/mcp.json --strict-mcp-config "<prompt>"` calling the same tool — the row landed correctly in the scratch DB (`<scratch-run>/data/ghost/ghost.db`); the real DB received nothing. **Confirmed working.**
- A CLI-only fallback (shelling out to `ghost-wrapped ... <cli-args>` for the save itself) was considered and rejected: `ghost`'s CLI surface (`mcp, hook, reflect, resolve, supersede, obsidian, bench, upgrade, version` — confirmed from `cmd/ghost/main.go`'s command switch) has no write path for saving memories/decisions outside the MCP server. The `claude -p` mechanism above is the only viable isolation strategy for live-agent phases, and is what all later plan tasks must use.

**Cost caveat:** `ghost resolve`/`ghost supersede` and reflection all call the real Anthropic API (Haiku) using the copied key — each eval run spends real API credits. This is expected and acceptable but worth remembering when deciding how many runs/storylines to execute.

## Test Modules

### Finding: `messages`/`conversations` are dead tables

Before designing the replay module, we confirmed that `ghost reflect`'s only transcript-shaped input, `Store.GetRecentExchanges` (queries `messages` joined to `conversations`), is fed by `Store.CreateConversation`/`AppendMessage` — and neither has a caller anywhere in the current codebase outside their own tests (`git grep` confirms only `internal/provider/provider.go` and `internal/memory/store.go`). The real DB has 76 conversations / 645 messages, but all dated March 2026, before commit `9aff8a7` ("strip Ghost to MCP-only server (v0.8.0), #149") removed the chat subsystem that used to write them. In the current, shipped codebase, **`ghost reflect` never sees a raw transcript turn** — `RecentExchanges` is always empty in production. Reflection is a *consolidator over already-saved memories*, not an extractor from conversation history. The actual save decision is made entirely by whichever agent calls `ghost_memory_save` during a real session, steered by the MCP server's tool instructions.

This means "feed a transcript through ghost's reflection/extraction path" (the original Module 1) describes a path that doesn't exist in production — testing it would exercise dead code and tell us nothing about real-world behavior. Module 1 is replaced by the two modules below.

### 1a. Live save-decision replay (live agent, one per real project transcript)

Input: real Claude Code session transcripts from `~/.claude/projects/-home-wayne-git-*/*.jsonl`, one project at a time.

Process: launch a fresh `claude -p` session per transcript (see Isolation), wired to a scratch-isolated `ghost` MCP server seeded empty for that project. Feed the session the transcript's real turns as the work it is doing (not as passive text to summarize) — normal coding-session framing, so it makes its own `ghost_memory_save`/`ghost_decision_record` calls exactly as a live agent would. Diff what it chooses to save against what's actually saved in the *real* ghost DB for the same project (pseudo-ground-truth — memories a human+agent pair actually judged worth keeping at the time).

Output: per-project recall (real memories the replay agent failed to save) and precision (memories it saved that aren't in the real set — noise or over-saving) plus example mismatches for the report.

This is a live-agent module, not a deterministic diff — it belongs alongside the storyline/stress modules in orchestration, not as a cheap pre-pass.

### 1b. Consolidation quality (no live agents)

Input: a real project's current memory set, copied into a scratch DB.

Process: run `ghost reflect --tier haiku` in dry-run mode against the seeded scratch DB. Grade the proposed consolidation: did it drop anything that looks important, merge things that shouldn't be merged, or mis-scope a memory to `_global`?

Output: per-project notes on consolidation quality plus example problems for the report.

This module is deterministic and cheap relative to the live modules — no MCP tool interaction, just a `ghost reflect` dry-run + diff.

### 2. Storyline module (live agents, synthetic multi-session projects)

3-4 synthetic fake projects, each realized as a *chain* of independent `claude -p` sessions (see Isolation) — one per simulated "session," launched sequentially via Bash from an orchestrating `agent()` call. Each `claude -p` invocation is a brand-new session with zero memory of prior sessions, which is exactly the mechanism this test needs: session N+1 knows only the storyline's fixed task script for its stage (what to work on this session) and the project_id to use — it has no transcript from session N. Because each is a real `claude -p` session, the session-start hook fires for real, so whatever context it has about earlier sessions comes *only* from what Ghost's session-start injection actually surfaces when it starts. This is the direct, realistic test of whether injection carries forward what matters.

Candidate storylines:
- **(a) Service migration** — multi-week fake project naturally producing decisions, gotchas, and at least one abandoned early approach (exercises `ghost_decision_record`, `ghost supersede`).
- **(b) Recurring bug-hunt / on-call** — repeated investigate-fix-close cycles, producing resolved-evidence memories (exercises `ghost resolve`).
- **(c) Config-heavy infra project** — lots of near-duplicate small facts, stressing dedup/consolidation and search precision under clutter.
- **(d) Long-lived project with a reversed early decision** — an early architectural choice is later contradicted by a new one, stressing `ghost supersede` and time-decay/recency ranking.

Each storyline agent uses ghost's MCP tools the way a real coding agent would — no scripted tool-call sequences, just real work with ghost wired in normally.

### 3. Stress-test scenarios (live agents, narrow/isolated)

A handful of scenarios targeting specific edge cases that don't reliably emerge from a storyline:
- Session-start injection under a large pre-seeded memory set (does it degrade or truncate sensibly rather than silently drop something critical?)
- A deliberately noisy/duplicate-heavy project (search precision under clutter, isolated from a storyline's narrative)
- A prompt-injection probe embedded inside stored memory content (does Ghost's "stored content is data, not instructions" framing hold for the agent consuming it — mirrors the same class of risk already called out for global preference memories)

## Orchestration

Built as a Workflow script (per user's explicit opt-in) with phases:

1. **Replay (live)** — one live agent per real project transcript (module 1a), run concurrently (`pipeline`), each returning recall/precision metrics + example mismatches + its own frustration-point report (it's a live-agent module).
2. **Consolidation** — one `ghost reflect --tier haiku` dry-run + grading pass per real project's seeded memory set (module 1b), run concurrently — deterministic, no live agent.
3. **Storyline** — for each storyline, a `pipeline()` of fixed session-stage prompts (session 1, session 2, ... session N), run as independent chained agent calls per item per storyline. Each stage agent gets only its own stage's task script plus the project_id — no session shares context with another except through what Ghost itself surfaces.
4. **Stress** — one agent per stress scenario, run concurrently.
5. **Synthesize** — a single final agent reads all replay metrics, consolidation notes, storyline reports, and stress-test reports, and writes the combined report.

Phases 1, 2, and 4 are pipelines/parallel (independent). Phase 3's storylines are independent of each other but each is a long sequential chain internally. Phase 5 is a genuine barrier — it needs all prior results together to dedupe/rank friction points across agents.

## Reporting

Every live agent (replay 1a, storyline, stress) is explicitly prompted, at the end of its run, to report:
- What it did, and quantitative observations (did search surface the right memory, did injection include what mattered)
- **Frustration points**, logged as they happened, not rationalized afterward: anything that felt slow, confusing, misleading, or actively annoying about using Ghost's tools
- An honest trust rating: would it have relied on what Ghost surfaced, unverified?

The consolidation module (1b) reports quantitative notes only (no live-agent experience to report — it's a `ghost reflect` dry-run diff).

The synthesis agent produces one Markdown report at `docs/superpowers/reports/YYYY-MM-DD-ghost-eval.md`:
- A scorecard per capability (search relevance, session-start injection, `ghost resolve`, `ghost supersede`, MCP tool ergonomics, save-decision quality, consolidation quality)
- Replay recall/precision numbers per real project (1a) and consolidation notes per real project (1b)
- A ranked "friction points worth fixing" list — ranked by how many independent agents (across replay, storyline, and stress modules) hit the same or a similar issue, not just a flat log

Per-agent raw reports are not kept as separate permanent artifacts — the synthesis report is the deliverable; raw agent outputs live only in the Workflow's transcript/journal for follow-up if something needs digging into.

## Out of scope

- This is not wired into CI — it's a manual, on-demand diagnostic.
- Not testing `ghost obsidian`, `ghost bench`, or the self-update path — those aren't part of "how an agent experiences memory during a coding session."
- Not attempting to fix anything found — this design produces a report; follow-up fixes are separate work, scoped after reading the report.
