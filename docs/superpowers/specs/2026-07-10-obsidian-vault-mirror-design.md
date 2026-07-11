# Obsidian vault mirror — design

**Date:** 2026-07-10
**Status:** Approved (design review with owner)
**Feature branch:** `feat/obsidian-mirror`

## Goal

Mirror Ghost's memory store into a plain-Markdown folder that Obsidian reads natively, giving users browsing, backlinks, graph view, Properties/Bases filtering, and mobile access (via any vault-sync mechanism) — without Ghost growing a UI. Strictly **one-way** (Ghost → vault) in v1.

Obsidian auto-refreshes on external file changes, so a folder of generated Markdown is a live view. Ghost's schema maps 1:1 onto Obsidian primitives: memories → notes with YAML frontmatter, `memory_links` → wikilinks (graph view), decisions → ADR-style notes, tasks → notes with status properties, projects → folders.

## Decisions locked in review

| Decision | Choice |
|---|---|
| Vault home | Dedicated Ghost-managed folder (default `~/Documents/GhostVault`), configurable via `obsidian.vault_dir` / `--out`. Open as its own vault or symlink into an existing one. |
| Freshness | `ghost obsidian export` (one-shot) + `ghost obsidian sync` (long-running, polls for DB changes). No background worker inside `ghost mcp` in v1. |
| v1 scope | Memories + decisions + tasks. Bases `.base` views, JSON Canvas, vault ingestion, `obsidian://` deep links → v2. |
| Direction | One-way, read-only mirror. Two-way sync explicitly out of scope. |

## Command surface

```text
ghost obsidian export [--out DIR] [--project NAME]   # one-shot full mirror
ghost obsidian sync   [--out DIR] [--interval 30s]   # poll PRAGMA data_version, re-export on change
```

`--project NAME` limits the mirror to that project's folder plus `Global/` (globals apply everywhere, mirroring the hook's behavior). Pruning is likewise scoped to the mirrored subtrees only.

Added as `case "obsidian":` in the `cmd/ghost/main.go` string-switch, hand-parsed flags, matching the existing `reflect` pattern.

## Change detection (sync)

Poll SQLite `PRAGMA data_version` every `interval` (default 30s, config `obsidian.interval`). The counter changes whenever another connection commits — no fsnotify dependency, no WAL races. DB opened **read-only** (same pattern as `internal/mcpinit/hook.go`), safe alongside a live MCP server. Sync latency is bounded by the interval; that trade-off is documented.

## Vault layout

```text
GhostVault/
  .ghost-vault          # marker: JSON {schema_version: 1} — pruning guard
  Global/               # the _global project — same shape as any project folder
    Memories/  Decisions/  Tasks/     # subfolders created only when non-empty
  <project-name>/       # one folder per project (sanitized name)
    Memories/  Decisions/  Tasks/
```

## Note format

One note per entity. Memory example:

```markdown
---
ghost_id: 74A37CBA…
type: memory
category: gotcha
importance: 0.8
pinned: false
project: ghost
tags: [embedding, backfill]
created: 2026-07-06
updated: 2026-07-08
source: mcp
---
> [!info] Mirrored from Ghost — edits here are not synced back.

<memory content>

## Related
- [[<other-note>]] — related (0.83)
```

- Frontmatter keys are Obsidian Properties (filterable, Bases-ready in v2).
- `## Related` renders non-invalidated `memory_links` as wikilinks with relation + strength → graph view shows Ghost's memory graph.
- Decisions: frontmatter `status: active|superseded|revisit`, body sections Rationale / Alternatives.
- Tasks: frontmatter `status: pending|active|done|blocked`, `priority`.

## Filenames and idempotence

`<slug>-<id8>.md` — slug from the first ~6 content words, 8-char ID suffix guarantees uniqueness and stability of identity. Export is a deterministic full regeneration: each entity renders to its own file, the backing queries are explicitly ordered (importance/created_at), and the output bytes are independent of iteration order. A file is only rewritten when rendered content differs — no mtime churn for Obsidian's indexer or file-sync tools. Content edits after consolidation may change the slug (a rename); all wikilinks are regenerated in the same pass, so the vault stays internally consistent.

## Pruning safety (the load-bearing part)

A file is deleted only when **all** hold:

1. It is under `vault_dir`, inside a Ghost-managed subtree (`Global/` or `<project>/`).
2. The `.ghost-vault` marker exists at the vault root.
3. The file's own frontmatter contains a `ghost_id` key (parsed before deletion).

User-created files are never touched. First export into an existing non-empty directory **without** the marker is refused (no `--force` in v1 — create a fresh dir or add the marker manually). The banner callout in every note states the mirror is one-way.

## Known limitations (v1)

- Renamed or merged projects can leave orphaned folders in the vault: prune only walks the **current** run's managed subtrees by design, so a folder that no longer maps to any project is never revisited. Recovery is trivial — delete the vault directory and re-export; the mirror is fully regenerable from the store.
- Per-project list queries are capped at 100,000 entries. A list that hits the cap may be silently truncated by the store, so the export logs a warning and skips pruning that project's folder for the run (stale extra notes beat silently deleted ones).

## Configuration

```yaml
obsidian:
  vault_dir: ~/Documents/GhostVault
  interval: 30s
```

Two new keys in the compiled defaults (`internal/config/config.go`); CLI flags override config.

## Implementation shape

- New package `internal/obsidian/`:
  - `render.go` — entity → Markdown (frontmatter marshal, slug, wikilinks)
  - `export.go` — full-mirror walk: load projects/memories/links/decisions/tasks via `provider.MemoryStore` read methods, render, diff-write, prune
  - `sync.go` — data_version poll loop wrapping export
- `cmd/ghost/main.go` — `obsidian` subcommand dispatch.
- No new dependencies: frontmatter is a fixed field set, emitted by a small hand-rolled writer (ordered keys, deterministic output, trivially golden-testable) — no yaml marshaling dep, **no fsnotify**.

## Error handling

- Missing/unreachable DB: same error path as `hook.go` (clear message, non-zero exit).
- Unwritable `vault_dir`: fail the export with the path in the error; sync logs and retries next tick.
- Partial-write safety: write to temp file + rename (atomic per note).
- A project with unsanitizable/colliding names: sanitize with the existing `sanitizeName` approach from `mcpinit`; collisions get ID suffixes.

## Testing

- Golden-file tests for rendering (memory/decision/task notes, wikilink section).
- Prune-guard unit tests: marker missing, foreign files, files without `ghost_id` — all must survive.
- Integration test: seeded store (`OpenDB(":memory:")` pattern) → export to `t.TempDir()` → assert tree; mutate store → re-export → assert diff-writes and pruning.
- Determinism: two exports of the same store are byte-identical.

## Out of scope (v2 candidates)

Bases `.base` dashboards; JSON Canvas relation edges; vault ingestion (`ghost obsidian ingest`); `obsidian://` deep links in MCP tool responses; two-way sync; background mirror worker inside `ghost mcp`.
