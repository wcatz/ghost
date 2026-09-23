# Obsidian Export: Orphaned Project Folder Cleanup

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

## Problem

When a project is deleted from Ghost (via `ghost project delete` or the MCP tool), the Obsidian sync loop detects the DB change and re-exports, but the deleted project's vault folder remains as stale files. The `prune` function only walks folders of projects that still exist in the DB (`subtrees`), so deleted projects' folders are never touched.

## Approach

Add a second pass to `prune` that scans the vault root for top-level directories not in the current `subtrees` set, and removes any that contain Ghost-managed content.

### Changes to `internal/obsidian/vault.go`

Change `prune`'s signature to accept a `knownFolders []string` parameter — the complete set of project folder names computed by `folderNames` for all selected projects (including truncated ones). This is distinct from `subtrees` (which excludes truncated projects and is used for the existing per-file prune walk).

After the existing subtree walk, add:

1. **Scan vault root** for top-level directories (skip the `.ghost-vault` marker file).
2. **Filter** to directories not in `knownFolders` — these are orphan candidates.
3. **Ghost-content check**: for each candidate, walk its `.md` files looking for any with `ghost_id` in frontmatter (via existing `hasGhostID`). If none found, skip — it's a user folder or empty.
4. **Delete** the entire directory with `os.RemoveAll` if it contains Ghost-managed content.
5. **Same guards** as existing prune: marker check at vault root, `filepath.IsLocal` on directory names.

### Edge cases

- **Empty orphan folders** (no `.md` files): left alone — no Ghost content to clean up.
- **Truncated lists**: safe because `knownFolders` includes ALL selected projects (truncated or not), so their folders are never orphan candidates. The existing per-file prune walk still skips truncated projects (stale beats deleted).
- **Filtered export** (`--project ghost`): orphan cleanup is skipped when a filter is active — we only see the filtered subset and can't determine what's orphaned. When a filter is active, pass `nil` for `knownFolders`.
- **User-created folders**: left alone — `hasGhostID` returns false for non-Ghost `.md` files.

### Changes to `internal/obsidian/export.go`

Update the `prune` call to pass `knownFolders` — the full set of folder names from `folderNames(projects)` for all selected projects. When a filter is active, pass `nil` to disable orphan cleanup.

### Tests

**`TestExportPrunesDeletedProjectFolder`**: Create two projects with memories, export, delete one from DB, re-export, assert deleted project's folder is gone.

**`TestExportSkipsOrphanCleanupWhenFiltered`**: Export with `--project ghost`, assert a second project's folder is not touched even if it exists on disk.

**`TestExportIgnoresUserFolders`**: Create a vault, export, then manually create a non-Ghost folder with user `.md` files. Re-export, assert user folder survives.
