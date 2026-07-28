# Project Resolution Unification — Design

## Problem

Ghost has three independent implementations of "resolve a project identifier to a project row," and they disagree:

1. **MCP session-start hook** (`internal/mcpinit/hook.go:302-319`, `lookupProject`) — resolves a cwd path via path-prefix match (longest wins) OR exact-name-equals-basename fallback. Read-only; never creates a project row. Talks to `*sql.DB` directly, not through `internal/memory.Store`.
2. **CLI `ghost reflect <name>` / `ghost resolve <name>`** (`internal/memory/store.go:295-309`, `ResolveProjectByName`, called from `cmd/ghost/main.go:188` and `:526`) — strict exact `name =` match only. No prefix, no basename fallback, no path awareness. Returns `""` on miss; both CLI commands then hard-fail with a bare `error: project "X" not found` and `os.Exit(1)`.
3. **MCP tool layer** (`internal/mcpserver/mcpserver.go:220-244`, `resolveProjectID`) — tries exact name, then exact ID, then (if input looks like an absolute path) path match. Used by every `ghost_*` tool. `ghost_memory_save` additionally calls `EnsureProject` (`store.go:154-241`) afterward, which creates the project row if it doesn't exist (`INSERT ... ON CONFLICT DO UPDATE`) — the only entrypoint in the codebase that auto-creates.

Because (2) has no prefix/basename fallback and (1)/(3) were each written separately, a project that the hook or MCP tools find without trouble can be unresolvable from the CLI, or vice versa. This was the single largest source of eval friction in `docs/superpowers/reports/2026-07-27-ghost-eval.md`: `ghost reflect`/`ghost resolve` failed with `project "X" not found` in 4 independent cases, blocking all consolidation-quality grading; separately, the FOREIGN KEY failures on first MCP write trace to the same underlying inconsistency in what "the project exists" means depending on which code path is asked.

## Goals

- One resolution algorithm, one implementation, used by all three call sites.
- CLI stays create-never (a `reflect`/`resolve` invocation on a nonexistent project has nothing to do; auto-creating it would still leave zero memories to act on). Only `ghost_memory_save`'s `EnsureProject` step continues to auto-create, unchanged.
- CLI failure messages become actionable: list known project names/IDs instead of a bare "not found."
- Behavior-preserving for the two paths that already work (hook, MCP tools) — this is a consolidation, not a semantics change for them.

## Design

### `Store.ResolveProject`

New method on `internal/memory.Store`:

```go
// ResolveProject resolves an identifier — a project name, hash ID, or
// filesystem path — to that project's (id, name). Returns ("", "", nil)
// on no match; a non-nil error only indicates a real DB failure.
//
// Lookup order, first hit wins:
//  1. exact id = input
//  2. exact name = input
//  3. if input contains '/': input = path OR input LIKE path || '/%'
//     (ordered by LENGTH(path) DESC — longest/most-specific match wins)
//  4. name = basename(input)
func (s *Store) ResolveProject(ctx context.Context, input string) (id, name string, err error)
```

"Not found" is a normal return value (empty strings), not an error — callers branch on emptiness, not on error type. This matches how `ResolveProjectByName` already behaves and avoids forcing every caller into error-type inspection.

This method replaces, and its callers migrate off of:
- `ResolveProjectByName` (subsumed — same exact-name case, now case 2 of a longer chain)
- `mcpserver.go`'s `resolveProjectID`
- `hook.go`'s `lookupProject`

### Near-miss listing on CLI failure

New method (or reuse of the existing `ListProjects`):

```go
func (s *Store) ListProjectNames(ctx context.Context) ([]string, error)
```

`cmd/ghost/main.go`'s `reflect` and `resolve` subcommands call this on a `ResolveProject` miss and format:

```
error: project "roller" not found. Known projects: ghost, infra, roller-web, _global
```

Project counts are small (per-user, dozens at most), so listing all of them beats attempting substring/fuzzy matching — no matching heuristics to get wrong, and the user can spot the typo immediately.

### Call-site migration

- **`cmd/ghost/main.go`** (reflect at `:188`, resolve at `:526`): replace `store.ResolveProjectByName(ctx, name)` with `store.ResolveProject(ctx, name)`. On empty result, call `ListProjectNames` and format the error as above. Still `os.Exit(1)` on miss — no behavior change to create-never.
- **`internal/mcpserver/mcpserver.go`**: delete `resolveProjectID`; every current call site (`ghost_memory_save`, `ghost_memory_search`, `ghost_memories_list`, `ghost_memory_delete`, `ghost_memory_update`, `ghost_task_create`, `ghost_decision_record`, etc.) calls `store.ResolveProject(ctx, args.ProjectID)` directly. The chain is a strict superset of the old `resolveProjectID` logic (adds basename fallback as case 4), so this is additive, not behavior-removing. `EnsureProject`'s auto-create call in `ghost_memory_save` is unchanged — it still runs after resolution, using whatever ID `ResolveProject` returned (or the raw input, if resolution missed, exactly as today).
- **`internal/mcpinit/hook.go`**: delete `lookupProject`. The hook currently opens `*sql.DB` directly rather than via `*memory.Store`; it gains a `*memory.Store` wrapping that same connection (or the hook's DB-open helper is adjusted to construct a `Store`), then calls `store.ResolveProject(ctx, cwd)`. Since `cwd` is always a path (contains `/`), this reliably hits case 3 or 4 — identical outcome to today's `lookupProject` for every existing input shape.

### Testing

Table-driven unit tests on `Store.ResolveProject` covering:
- exact ID match
- exact name match
- path-prefix match, including the "longest path wins" tie-break (two projects where one's path is a prefix of another's)
- path input with no prefix match, falling through to basename-of-input matching a project name (new behavior for the CLI/MCP-tool paths, though already how the hook worked)
- no match at all → `("", "", nil)`
- a real DB error (e.g. closed connection) → non-nil error

Existing tests referencing `ResolveProjectByName` or `resolveProjectID`/`lookupProject` directly are updated to call `ResolveProject` instead; no new integration test is needed beyond what already exercises the hook/MCP/CLI paths end-to-end.

## Non-goals

- Auto-create-on-CLI-reference is explicitly out of scope (decided during brainstorming) — `reflect`/`resolve` remain create-never.
- Fuzzy/substring-based near-miss suggestions are out of scope — full project listing was chosen instead for simplicity.
- This spec does not touch `ghost supersede`'s relation-type conflation or session-start injection dedup — those are separate specs, sequenced after this one, per the three-root-cause breakdown in `docs/superpowers/reports/2026-07-27-ghost-eval.md`.
