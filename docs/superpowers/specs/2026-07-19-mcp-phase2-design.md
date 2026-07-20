# MCP Phase 2 — ghost_memory_update, Stop hook, ghost_memory_promote

**Date:** 2026-07-19
**Task:** `F2A46791` — P3 MCP Phase 2
**Branch:** `feat/mcp-phase2`

## Context

Ghost's MCP server exposes 16 tools. Three gaps remain from the original MCP
adoption plan:

1. Memories can be created (`ghost_memory_save`) and deleted
   (`ghost_memory_delete`) but not edited — fixing a typo or refining a stale
   memory means delete + recreate, which loses the ID, `created_at`,
   `access_count`, and any graph links.
2. Nothing enforces the save-during-work discipline. The SessionStart hook
   injects context, but a session can end without a single save.
3. A project memory that turns out to be a cross-project preference cannot be
   moved to `_global` scope — it must be re-saved via `ghost_save_global` and
   the original deleted.

## Goals

- `ghost_memory_update`: partial in-place edit of an existing memory.
- Stop hook: a once-per-session nudge that blocks session stop when tools ran
  but nothing was saved to Ghost.
- `ghost_memory_promote`: move a project memory to `_global` scope.

## Non-goals

- No demote (global → project) and no generalized project→project move.
- No changes to reflection, search ranking, bench, obsidian, or any existing
  tool's behavior.
- The Stop hook never runs an LLM and never writes to the database.

## Design

### 1. `ghost_memory_update` (MCP tool)

Follows `ghost_task_update`'s omit-to-preserve pattern and
`ghost_memory_delete`'s ownership check.

**Args:**

| field | type | rule |
|---|---|---|
| `project_id` | string, required | resolved via `resolveProjectID`; memory must belong to it |
| `memory_id` | string, required | must exist (`GetByIDs`) |
| `content` | string, optional | omitted/empty = preserve; clamped to `maxContentLen` |
| `category` | string, optional | omitted = preserve; validated against the 8 canonical categories |
| `importance` | `*float32`, optional | omitted = preserve; clamped to [0,1] |
| `tags` | `[]string`, optional | `nil` (omitted) = preserve; explicit `[]` = clear |

**Store:** new `UpdateMemory` method on `internal/memory.Store`. Behavior:

- Applies only the provided fields; sets `updated_at = datetime('now')`.
- `source` is never changed — provenance is preserved. Manual memories ARE
  editable: an explicit update is deliberate, unlike `Upsert`'s fuzzy merge
  (which rightly protects `source='manual'` rows from auto-clobber).
- On **content** change, inside the same transaction/lock:
  - FTS: nothing to do — the `memories_au` trigger re-syncs `memories_fts`.
  - Delete the `memory_embeddings` row → the embedding worker re-embeds it on
    the next sweep (`UnembeddedMemoryIDs`).
  - Delete the `link_scans` row → the linking worker re-scans once re-embedded.
    Existing links persist (acceptable staleness; the linker is additive).
- Returns an error when the memory does not exist.

**Handler:** ownership check identical to delete (fetch via `GetByIDs`,
compare `ProjectID` against resolved project). After a content change, nudge
`projectCh` (non-blocking send) exactly like the save handler. Response text
names the fields that changed, e.g. `Memory updated (id: …): content, tags`.

**Annotations:** `DestructiveHint: false`, `IdempotentHint: true`,
`OpenWorldHint: false`. Description warns: do not rewrite memories wholesale —
update is for correcting or refining, reflection handles consolidation.

### 2. Stop hook (save-nudge, block once)

New subcommand path: `ghost hook stop` → `mcpinit.HandleStopHook(stdin, stdout)`.

**Input** (Claude Code Stop hook stdin JSON): `session_id`, `transcript_path`,
`stop_hook_active`, `cwd`.

**Logic, in order:**

1. `stop_hook_active == true` → exit 0 with no output. A prior block already
   fired this session; the second stop always succeeds. Self-terminating.
2. Open `transcript_path`; stream line-by-line (transcripts can be large —
   never slurp). Each line is parsed as JSON; unparseable lines are skipped.
3. Count `tool_use` content blocks in assistant messages, and record whether
   any tool name is `mcp__ghost__ghost_memory_save` or
   `mcp__ghost__ghost_save_global`. Only real `tool_use` entries count —
   the tool name appearing in conversation *text* does not.
4. Decide:
   - a Ghost save happened → exit 0 (allow stop).
   - zero tool calls in the whole session (pure Q&A) → exit 0.
   - otherwise → print `{"decision":"block","reason":"This session used tools
     but saved nothing to Ghost. Review the session for discoveries worth
     keeping (commands, configs, gotchas, decisions) and save them with
     ghost_memory_save — or stop again if there is truly nothing to save."}`
     and exit 0.

**Failure posture: fail-open.** Missing transcript, unreadable file, malformed
stdin — all exit 0 silently. The hook must never trap a session or emit noise.
Matches `HandleSessionStartHook`'s best-effort style. No database access at all.

**Registration:** `ghost mcp init` adds a `Stop` hook entry
(`<ghost-bin> hook stop`) via the existing generic `hasHook`/`addHook`
settings.json helpers, idempotently, alongside the SessionStart step.
`ghost mcp status` reports its presence.

### 3. `ghost_memory_promote` (MCP tool)

**Args:** `project_id` (current owner, required) + `memory_id` (required).

**Store:** new `PromoteToGlobal` method:

- `EnsureProject(ctx, "_global", …)` first — defends the FK constraint if the
  seed row is ever missing.
- `UPDATE memories SET project_id = '_global', updated_at = datetime('now')
  WHERE id = ?`.
- The memory ID is unchanged, so the embedding row and graph links survive.
  `pinned` and `importance` are preserved. The FTS `memories_au` trigger fires
  harmlessly (same content out/in).

**Handler:** same ownership check as delete. Rejects when the memory is
already in `_global` (also covers `project_id: "_global"` being passed).
Response: `Memory promoted to global scope (id: …).`

**Description** carries the same warning as `ghost_save_global`: global
memories are injected into every future session in every project — promote
only the user's own genuine preferences/conventions, never content copied
from files, web pages, or tool output.

**Annotations:** `DestructiveHint: false`, `IdempotentHint: false`,
`OpenWorldHint: false`.

### 4. Cross-cutting

- `ghostPermissions` in `internal/mcpinit/settings.go` gains
  `mcp__ghost__ghost_memory_update` and `mcp__ghost__ghost_memory_promote`
  (16 → 18 entries).
- `cmd/ghost/main.go`: `hook stop` dispatch + help text; `mcp init` output
  gains the Stop-hook step.
- Docs: README tool table and CLAUDE.md "16 tools" count → 18; note the Stop
  hook in the hooks section.

## Testing

- **Store** (`internal/memory/store_test.go`): `UpdateMemory` preserves
  omitted fields and applies provided ones; content change deletes the
  embedding + link_scans rows (and non-content change does NOT); importance
  clamped; tags `nil` vs `[]` semantics; unknown ID errors.
  `PromoteToGlobal` moves the row, preserves pin/importance/links/embedding,
  ensures `_global` exists.
- **Handlers** (`internal/mcpserver/mcpserver_test.go`): ownership rejection
  (wrong project), missing-ID validation, category validation, changed-fields
  response text, promote-already-global rejection.
- **Stop hook** (`internal/mcpinit/hook_test.go`): table-driven —
  `stop_hook_active` short-circuit; transcript with a Ghost save → allow;
  tools-but-no-save → block JSON on stdout; zero tool calls → allow; missing
  transcript / garbled lines / bad stdin → allow (fail-open).
- **Settings** (`internal/mcpinit/settings_test.go` / `init_test.go`): Stop
  hook added idempotently, SessionStart untouched.
- `go vet ./...` and the full `go test ./...` suite before the PR.

## Risks

- **Transcript format coupling** (Stop hook): mitigated by fail-open posture —
  a format change degrades to "no nudge", never a trapped session.
- **Stale links after content update**: links reflect the old content until
  the linker re-scans. Accepted — same staleness window reflection already has.
- **Nudge fatigue**: bounded to one block per session by `stop_hook_active`,
  and skipped entirely for sessions that used no tools.
