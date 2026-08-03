# Subagent SessionStart Injection Gating — Design

**Goal:** Stop Ghost's `SessionStart` hook from injecting project-context tokens into subagent sessions, since subagents already receive their working context in-band from the parent's prompt and derive near-zero benefit from a second, independent context dump. This is the first of three candidate fixes identified for reducing Ghost's Claude Code usage footprint (~30% of a recent 24h window) toward single digits; the other two (pinned-globals/project-memory shared-budget bug, `ghost reflect` re-trigger de-duping on long sessions) are deliberately deferred until this change is measured.

**Architecture:** Claude Code's `SessionStart` hook stdin payload carries `agent_id` and `agent_type` fields that are populated only when the session belongs to a subagent (spawned via the Agent/Task tool, or a Workflow-tool `agent()` call). `internal/mcpinit/hook.go`'s `HandleSessionStartHook` currently has no branch on session origin — it unconditionally bumps the interaction counter, loads global memories, loads project context, and kicks off Obsidian sync, then writes all of it to stdout. This change adds an early-exit branch: if the parsed stdin shows a non-empty `agent_id`, the hook writes nothing to stdout and performs none of its current side effects, before any database connection is opened.

**Data flow (subagent case):** Claude Code invokes `ghost hook session-start` → stdin JSON includes `agent_id: "<id>"` → `HandleSessionStartHook` parses it, sees `agent_id != ""`, returns immediately. No DB open, no counter bump, no Obsidian-sync trigger, no stdout output. Top-level sessions (no `agent_id`) are unaffected — the function proceeds exactly as it does today, unchanged code path.

**Tech Stack:** No new dependencies. Pure Go stdlib (`encoding/json` for the existing input struct, already imported).

---

## Behavior

- Gate applies **uniformly** to every subagent — no allowlist of agent_types that still get full injection. If a specific subagent type is later found to need project memory, it can call `ghost_project_context` itself; that path is unaffected by this change.
- The gate is a pure function of the hook's stdin payload — it does not depend on config, environment variables, or DB state.
- Side effects skipped for subagent sessions, all of which currently run unconditionally at the top of `HandleSessionStartHook`:
  - `ensureObsidianSyncRunning()` — pointless to re-trigger per subagent spawn
  - `bumpSessionCount` (the per-project interaction counter) — a subagent spawn is not a real user interaction and today inflates the displayed session count
  - `loadGlobalMemories` — currently loaded unconditionally regardless of project match; skipped entirely for subagents
  - `loadSessionContext` (project memories/tasks/decisions/summary) — skipped entirely for subagents
- Top-level session behavior (the `agent_id`-absent case, including the "no project matched" branch) is byte-for-byte unchanged.

## Out of scope

- The pinned-globals/project-memory shared slot-budget bug (globals currently compete with project memories for the same display cap) — deferred.
- De-duplicating/rate-limiting `ghost reflect` triggers on long-running sessions — deferred.
- Any change to `ghost_project_context` or other MCP tools a subagent can still call on demand — unaffected, out of scope.
- Any attempt to distinguish *which* subagents might benefit from injection (e.g. an allowlist) — explicitly rejected in favor of uniform gating, per user decision.

## Testing

`internal/mcpinit/hook_test.go` already exercises `HandleSessionStartHook` for the existing top-level-session cases (project match, no project match, globals-only). Add:

- A case with `agent_id` set in the stdin payload (with an otherwise-normal cwd/project setup that would produce non-empty output for a top-level session) asserting stdout is empty.
- A case confirming the existing non-subagent behavior is unchanged (regression guard — same fixture, `agent_id` omitted, same assertions as today).
- No new DB-interaction assertions are needed beyond "stdout is empty," since the function returns before any DB code runs in the subagent branch — this is enforced by control flow, not something a test can observe independently of a DB file appearing on disk. (A test can additionally assert no DB file was created in a scratch data dir, as an extra guard against a future refactor accidentally moving the DB-opening code above the gate.)

## Success criteria

- Unit tests above pass.
- Manually verified: spawning a subagent (via the Agent tool) in a Ghost-tracked project produces no `## Ghost context: ...` block in that subagent's context.
- Follow-up (outside this change, tracked separately): re-check the usage dashboard after a day of normal use to see whether Ghost's share drops meaningfully; if not, revisit the two deferred fixes.
