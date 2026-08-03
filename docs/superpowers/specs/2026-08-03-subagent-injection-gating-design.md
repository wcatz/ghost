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

---

## Additional cost-reduction measures (folded in after initial approval)

Two more sources were investigated in parallel with the subagent-gating fix above: (1) my own review of `hook.go`'s handling of hook re-fires within one logical session, and (2) a backgrounded Opus-model agent tasked with finding token-efficiency improvements to the injected-context payload itself (the content that top-level sessions correctly still receive). Findings from both are folded into this same spec since they touch the same file and the same "reduce Ghost's token footprint" goal; each is scoped as an independent, separately-testable change.

### C. Gate re-injection on hook re-fires within one session

**Finding:** `SessionStart` fires not just on true fresh starts but also on `resume`, `clear`, and `compact`. Today `HandleSessionStartHook` treats all of these identically to a fresh start — the full ~3.5k-token block is re-emitted every time, even though `resume`/`clear`/`compact` are, from the user's perspective, a continuation of one session, not a new one. Given that 8+ hour sessions (which auto-compact repeatedly) are a large share of usage, repeated full re-injection on `compact` is a plausible major contributor to Ghost's overall footprint.

**Decision (user-selected):**
- `source == "resume"`: emit nothing. The resumed transcript already contains the original injection; re-emitting is pure waste.
- `source == "clear"`: unchanged — full injection, exactly as today. `/clear` wipes the transcript, so this is the only case where the model would otherwise have zero project context.
- `source == "compact"`: emit a short pointer instead of the full block — something like `"Ghost context was already loaded earlier this session and may have been condensed by compaction. Call ghost_project_context if you need the full detail again."` This avoids betting on Claude Code's compaction reliably preserving the original injected block verbatim, while still cutting most of the repeated cost.
- `source == "" or "startup"` (and `fork`, which behaves like a fresh start context-wise): unchanged, full injection.

**Implementation shape:** extend the existing `sessionStartInput.Source`-based branch (the same field already used today to gate the interaction-counter bump) to also gate the output path, rather than introducing a second, separate check.

### D. Globals section: relevance-gate, dedup, and cap

**Finding (Opus agent, measured on a live run of this project):** globals are 31% of the total injected payload (4,468 of 14,232 bytes) via `loadGlobalMemories`, which is a `LIMIT 15` query with no dedup and — unlike project memories — never passes through `DemotionPenalties`. A live example: two near-duplicate global memories ("Restricted repos READ-ONLY") link at `strength = 0.8857`, just under the existing `DefaultDemotionThreshold = 0.90`.

**Change:** apply `DemotionPenalties` to the globals section (same mechanism already used for project memories), lower the threshold to ~0.85 for this section given the observed 0.8857 near-dup, and cap the section at 6–8 entries (pinned-first). When entries are dropped by the cap, add an "N not shown" line matching the existing pattern already used for the project-memories section (`hook.go` ~line 149).

**Risk called out by the user's regression-guard rule:** globals are user-authoritative preferences, so this must never silently drop content the user cares about — the "N not shown" line is required, not optional, so nothing disappears without a visible trail.

### E. Decay-ranking parity in the hook (prerequisite for F)

**Finding:** `hook.go` orders memories by raw `pinned DESC, importance DESC, updated_at DESC`, while `Store.GetTopMemories` (the MCP-tool path) applies category-aware time decay (45-day pattern/architecture, 30-day else, floor 0.3/0.15) × a 1.5 pinned boost. The hook is the only injection path ignoring decay, which is why several already-resolved/closed-topic memories (the graph-expansion saga, shipped-PR changelogs) are still holding slots in the live output.

**Change:** port `Store.GetTopMemories`'s `ORDER BY` expression into the hook's query (or extract it to a shared constant/function both call) so the hook ranks identically to the MCP tool path. Token-neutral by itself — it's what makes cutting the cap (F) safe rather than arbitrary.

### F. Cut memory cap and per-item truncation (only after E lands)

**Change:** reduce the injected-memory cap from 25 to 15, and per-item truncation from 300 to 200 bytes. Only safe once (E) ensures the top 15 by decayed rank are actually the most relevant — today's raw-recency ordering means an arbitrary cut would drop good memories ahead of stale ones. Completeness is preserved via the existing "N not shown" messaging.

### G. Remove duplicated warning text

**Finding:** the injection-boundary warning text (the `«...»` delimiter explanation) is emitted twice in the current output (`hook.go` ~lines 120 and 141). This is a plain duplication bug, not a design tradeoff — dedup it to a single emission.

### Out of scope (from this round of investigation)

- **Running `ghost resolve` against this project's existing stale memories** — Opus found 6 of the current 25 injected slots are closed-topic saga/changelog memories that `ghost resolve` already exists to demote. This is an operational action (run the existing command), not a code change, and will be done directly as a one-off rather than folded into the implementation plan.
- **Summary-section redundancy with the memory list** (Opus estimated ~40% overlap between the `**Summary:**` prose and the verbatim memories listed below it) — real, but the fix requires changing what reflection is prompted to produce, which is a separate, lower-confidence change with its own risk profile (reflection quality regressions). Deferred to a future spec.
- Removing the `«»` delimiters/warning text entirely — Opus flagged this as prompt-injection defense worth ~200 tokens; not worth the security tradeoff.
- The pinned-globals/project-memory shared-budget bug in `Store.GetTopMemories` (the MCP-tool path, distinct from the hook's own `loadGlobalMemories`) — confirmed by Opus to be a separate, still-deferred item, out of scope here.
- `ghost reflect` re-trigger de-duping on long sessions — still deferred, unaffected by any change in this document.

### Updated testing scope

In addition to the subagent-gating tests above, `hook_test.go` needs:
- One case per `source` value (`""`/`startup`, `resume`, `clear`, `compact`) asserting the correct output shape (full / empty / full / pointer-only respectively).
- A case with a globals fixture containing a near-duplicate pair above the 0.85 threshold, asserting only one survives and a "not shown" line appears when the cap is exceeded.
- A case verifying the hook's memory ordering matches `Store.GetTopMemories`'s ordering given identical fixture data (same pinned/importance/updated_at/category inputs).
- A case asserting the warning text appears exactly once in output.

### Updated success criteria

- All tests above pass, plus the original subagent-gating tests.
- Manual check: live hook output for this project shows the near-dup globals pair collapsed to one entry, and the graph-expansion/changelog memories no longer occupy top slots (post `ghost resolve` run + decay-ranking fix).
- Re-check the usage dashboard after a day of normal use, per the original success criteria above.
