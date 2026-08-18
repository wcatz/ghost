# Delete Project — Design

## Context

Ghost has no way to remove a project and everything under it. `ghost_memory_delete` only removes one memory at a time; `ghost_list_projects` is read-only. There is no CLI or MCP path that wipes a project's memories, tags, links, tasks, decisions, or cost/audit history in one operation.

At the schema level (`internal/memory/schema.go`), most child tables already cascade from `projects` via `ON DELETE CASCADE`, and every connection opens with `foreign_keys(ON)` (`OpenDB`, schema.go), so cascades actually fire:

- Cascades automatically from `projects(id)`: `memories` (tags live as a JSON column on the memory row, not a separate table, so they go with it), `memory_embeddings` and `memory_links`/`link_scans` (both cascade from `memories(id)`, which itself cascades from `projects(id)`), `tasks`, `decisions`, `ghost_state`, `memory_snapshots`.
- Does **not** cascade: `token_usage` and `audit_log` both carry a `project_id` column with no foreign key constraint at all. A raw `DELETE FROM projects` leaves both tables holding rows that reference a project ID that no longer exists.

So a real delete-project feature is mostly "delete the project row and let the FKs do their job," plus two explicit cleanup statements for the tables that don't cascade.

## Goals

- A CLI subcommand, `ghost project delete <name-or-id>`, and an MCP tool, `ghost_project_delete`, both backed by one `Store.DeleteProject` method — no duplicated deletion logic between the two surfaces.
- Dry-run by default; an explicit `apply` (MCP) / `--apply` (CLI) is required to actually delete, following the same convention `ghost resolve`/`ghost supersede` already use.
- The CLI additionally requires re-typing the project's name at a confirmation prompt when `--apply` is passed, since this is irreversible and has no undo.
- `_global` can never be deleted, regardless of flags — it's a shared, seeded project every other project's context injection depends on.
- Project identification works exactly like every other command: `Store.ResolveProject` (id → name → path-prefix → basename), so a user can pass whatever they'd type anywhere else in Ghost.
- The two non-cascading tables (`token_usage`, `audit_log`) are explicitly cleaned up as part of the same apply, so nothing is left orphaned.

## Non-goals

- No Obsidian vault cleanup. The vault mirror (`internal/obsidian`) is a separate one-way export; a deleted project's exported notes are left in place for the user (or a future `ghost obsidian sync` pass) to deal with. Wiring vault deletion into this feature couples two independently-versioned subsystems for a cosmetic gain.
- No backup/export-before-delete step. The CLI's dry-run summary plus the re-type confirmation is the safety net; this is not a substitute for `sqlite3 <db> ".dump"` if the user actually needs a backup first.
- No bulk/multi-project delete. One project per invocation.
- No change to `ResolveProject`, `MergeProject`, or any other existing store method.

## Design

### `Store.DeleteProject`

```go
type DeleteProjectSummary struct {
	ProjectID   string
	ProjectName string
	Memories    int
	MemoryLinks int
	Tasks       int
	Decisions   int
	TokenUsage  int
	AuditLog    int
}

func (s *Store) DeleteProject(ctx context.Context, input string, apply bool) (DeleteProjectSummary, error)
```

1. Resolve `input` via `s.ResolveProject(ctx, input)`. An empty `id` result (Ghost's existing "not found" convention — see every `ResolveProject` call site in `mcpserver.go`/`main.go`) becomes `fmt.Errorf("project %q not found", input)`.
2. If the resolved `id == "_global"`, return an error immediately — `fmt.Errorf("refusing to delete the _global project")` — before computing anything else. This check runs whether or not `apply` is set, so even a dry-run on `_global` reports the refusal rather than a misleading count.
3. Compute the summary counts, always (this is the dry-run view):
   - `SELECT count(*) FROM memories WHERE project_id = ?`
   - `SELECT count(*) FROM memory_links WHERE source_id IN (SELECT id FROM memories WHERE project_id = ?) OR target_id IN (SELECT id FROM memories WHERE project_id = ?)` — links reference `memories(id)`, not `projects(id)`, so this has to join through memories rather than filter directly.
   - `SELECT count(*) FROM tasks WHERE project_id = ?`
   - `SELECT count(*) FROM decisions WHERE project_id = ?`
   - `SELECT count(*) FROM token_usage WHERE project_id = ?`
   - `SELECT count(*) FROM audit_log WHERE project_id = ?`
4. If `apply` is false, return the summary with no writes.
5. If `apply` is true, run inside one transaction (mirroring `mergeProjectLocked`'s existing tx pattern in the same file):
   - `DELETE FROM token_usage WHERE project_id = ?`
   - `DELETE FROM audit_log WHERE project_id = ?`
   - `DELETE FROM projects WHERE id = ?` — cascades to memories, memory_embeddings, memory_links, link_scans, tasks, decisions, ghost_state, memory_snapshots.
   - Commit. Return the same summary computed in step 3 (it reflects what was actually removed, since nothing else can write to a project mid-call under `s.mu.Lock()`).

`DeleteProject` takes `s.mu.Lock()` for the apply path (mutating) and `s.mu.RLock()` for the dry-run-only path (read-only), matching the existing lock-per-method convention elsewhere in `store.go`.

### `provider.MemoryStore` interface

Add `DeleteProject(ctx context.Context, input string, apply bool) (memory.DeleteProjectSummary, error)` alongside the other project-management methods.

### CLI: `ghost project delete <name-or-id>`

New top-level `case "project":` in `cmd/ghost/main.go` (the first CLI use of a `project` verb group), with a `delete` subcommand and a `--apply` flag, following the existing flag-parsing style used by `reflect`/`resolve`/`supersede`.

- Without `--apply`: resolve and print the summary (project name/id and each count), then exit 0. No prompt, nothing written.
- With `--apply`: print the same summary, then prompt `Type the project name to confirm deletion: ` and read a line from stdin. If it doesn't match the resolved project's name exactly, abort with a non-zero exit and delete nothing. If it matches, call `DeleteProject(..., apply=true)` and print a final confirmation line with the counts actually removed.

### MCP tool: `ghost_project_delete`

Same shape as `ghost_resolve`: args `project` (required) and `apply` (default `false`). Tool description states plainly that this is irreversible and that `_global` is refused. Response text always includes the summary; when `apply` is false it's framed as a preview ("would delete: ..."), when `apply` is true it's framed as a result ("deleted: ..."). No interactive confirmation step over MCP — there's no terminal to prompt through, so the tool response itself carries the full weight of making the action legible before/after the fact.

## Testing

- `Store.DeleteProject` unit tests in `internal/memory/store_test.go`:
  - Dry-run (`apply=false`) returns correct counts and leaves every row in place (assert row counts unchanged after the call).
  - `apply=true` removes the project row and every cascaded table, plus explicitly empties `token_usage`/`audit_log` for that project — verified by direct `SELECT count(*)` against each table, not just checking the returned summary.
  - Deleting `_global` errors in both dry-run and apply modes, and mutates nothing.
  - A nonexistent project (bad id, bad name, bad path) errors with the same `%q not found` shape every other command uses.
  - A project with rows in every table (memories with links between them, tasks, decisions, token_usage, audit_log) is deleted cleanly in one pass — this is the "all it's memories and tags, links etc" case from the original ask.
  - Deleting one project doesn't touch another project's rows (basic isolation check, cheap to add).
- CLI test(s) covering the confirmation-prompt gate: wrong re-typed name aborts without deleting; correct name proceeds.
- MCP tool test(s) covering the dry-run/apply framing, mirroring the existing `ghost_resolve` MCP test style.
- `go vet ./...` and `go test ./...` before and after, as with every change in this repo.
