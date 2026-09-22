# Obsidian typed edge fields — design

**Date:** 2026-09-22
**Status:** Approved (design review with owner)
**Feature branch:** `feat/obsidian-typed-edge-fields`

## Goal

Expose Ghost's `memory_links` graph to Obsidian plugins that read typed frontmatter fields (Breadcrumbs Matrix/Tree views being the primary consumer) instead of parsing prose. Today `renderMemory` flattens every link into the `## Related` body section as `- [[file]] — relation (strength)`; Breadcrumbs cannot use that. This change adds Breadcrumbs-compatible typed edge fields to each memory note's YAML frontmatter while leaving the existing `## Related` section untouched.

## Decisions locked in review

| Decision | Choice |
|---|---|
| Scope | Option A only: typed edge fields in frontmatter. No extra scalar fields (`resolved_at`, `access_count`, …), no JSON Canvas, no Bases `.base` views. |
| Keys | Append after the existing frontmatter keys (11 single-line keys today: `ghost_id` … `source`), fixed alphabetical order among the five: `causes`, `contradicts`, `elaborates`, `related`, `supersedes`. Present only when non-empty. |
| Value shape | YAML flow list of quoted `[[wikilink]]` strings via the existing `fileFor` map and `yamlScalar`/list-quoting helpers, e.g. `related: ["[[other-note-beef0000]]"]`. |
| Direction | Directional relations (`supersedes`, `causes`, `contradicts`) emit the field on the **source endpoint only**, pointing at the target. Symmetric-ish relations (`related`, `elaborates`) emit on **both** endpoints, each pointing at the other (matches current `## Related` both-endpoint behavior). |
| `## Related` prose | Unchanged. Continues to carry every non-invalidated link with relation + strength, including plain short-ID fallback for targets missing from `fileFor`. |
| Filtered-out targets | If the other endpoint is absent from `fileFor` (e.g. `--project` filtered it out), skip that entry for the typed field — never emit a dead wikilink. If every entry for a relation is skipped, omit the key. |
| Determinism | Fixed key order + targets sorted by filename (not strength) → byte-identical re-exports, so `writeIfChanged` keeps its no-churn guarantee. |
| Out of scope / YAGNI | Inverse fields (`superseded_by`, …), strength in frontmatter, decisions/tasks links, JSON Canvas edges, Bases views. |

## Background

Prior work (spec `2026-07-10-obsidian-vault-mirror-design.md`) established the one-way vault mirror: memories → notes with YAML frontmatter, `memory_links` → `## Related` wikilink prose, deterministic export, prune safety. That spec deferred JSON Canvas and Bases to v2; this change stays inside the note format and does not revisit those deferrals.

`memory.Link` (from `GetLinks`) carries `SourceID`, `TargetID`, `Relation`, `Strength`. Storage semantics (`internal/memory/links.go`): only `related` is normalized to (min, max) ID order; `supersedes`, `contradicts`, `elaborates`, `causes` preserve direction. `GetLinks` returns every non-invalidated link touching the memory from either endpoint, ordered by strength DESC (ties unordered — hence filename sort for frontmatter determinism).

Relation vocabulary is the five keys above; anything else (none expected today) still appears in `## Related` prose but never as a frontmatter key.

## Note format after this change

```markdown
---
ghost_id: 74a37cba00112233
aliases: ["Embedding backfill bug: ticker only swept seen projects."]
type: memory
category: gotcha
importance: 0.8
pinned: false
project: ghost
tags: [embedding, backfill]
created: 2026-07-06
updated: 2026-07-08
source: mcp
related: ["[[other-note-beef0000]]", "[[third-note-cafe0000]]"]
supersedes: ["[[old-note-dead0000]]"]
---
> [!info] Mirrored from Ghost — edits here are not synced back.

Embedding backfill bug: ticker only swept seen projects.

## Related
- [[other-note-beef0000]] — related (0.83)
- [[old-note-dead0000]] — supersedes (0.90)
- [[third-note-cafe0000]] — related (0.50)
- beef0000deadbeef — related (0.40)
```

The last `## Related` line is a link whose target is absent from `fileFor` (e.g. `--project` filtered it out): prose keeps the short-ID fallback, and the typed `related` field correctly omits that entry.

Invariants preserved:

- `ghost_id` remains the first frontmatter key; every value remains exactly one line (prune's `hasGhostID` scan).
- Typed keys are appended after `source:` and never re-order the existing frontmatter keys.
- Empty / unknown / fully-filtered relations omit their key entirely — a note with no qualifying links has the same 11 frontmatter lines as today.

## Endpoint emission rules

For each non-invalidated link `l` touching memory `m` with `l.Relation ∈ {causes, contradicts, elaborates, related, supersedes}`:

1. **Directional** (`causes`, `contradicts`, `supersedes`): emit only when `m.ID == l.SourceID`. The field value is the target's filename. When `m` is the target, emit nothing for that relation.
2. **Symmetric-ish** (`related`, `elaborates`): emit whenever `m` is either endpoint. The field value is the other endpoint's filename.

Then:

- Resolve each target filename through `fileFor`; drop entries with no filename (filtered-out project, deleted-but-still-linked edge, etc.).
- Sort remaining filenames lexicographically (byte order) before joining.
- Omit the key when zero filenames remain.

Duplicate edges of the same relation pointing at the same filename collapse to one list entry (defensive; the store enforces uniqueness on `(source, target, relation)`).

## Determinism and churn

- Key order is the fixed five-key alphabetical order, independent of link order in `GetLinks`.
- Within a key, targets are sorted by filename — strength reordering, SQL tie order, or map iteration cannot change output bytes.
- Result: exporting the same store twice is still byte-identical; `writeIfChanged` still skips unchanged notes. Only the first export after this feature lands (or an actual graph change) rewrites linked notes.

## Compatibility

- `## Related` body output is byte-identical to today for the same store — existing body assertions and the filtered-target short-ID fallback keep working.
- Prune scans only `ghost_id` on line 1; extra keys after `source:` are irrelevant to it.
- Frontmatter remains a fixed ordered hand-rolled writer — no YAML marshaling dependency.
- Breadcrumbs and other frontmatter consumers see native typed fields; Obsidian Properties panel gains five optional list properties.

## Testing

Extend `internal/obsidian/render_test.go` golden tests:

1. **Mixed links** → exact frontmatter: directional link appears only on source note; `related`/`elaborates` on both; multi-target lists sorted by filename; keys in fixed alphabetical order after `source:`.
2. **Filtered-out target** (target id not in `fileFor`) → typed key omitted (or entry dropped), while `## Related` still shows the short-ID fallback line.
3. **No qualifying links** → exactly the existing 11 frontmatter lines, no new keys.
4. **Existing goldens** (`TestRenderMemory`, hostile-frontmatter line counts) updated only where links are present; unlinked-note goldens unchanged.

Extend `internal/obsidian/export_test.go`:

5. Seeded store with a directed + a symmetric link → export → re-export → assert byte-identical / no rewrite (mtime or `writeIfChanged` skip), covering the determinism guarantee end-to-end.

Run `go vet ./...` and `go test ./...` before considering the work done.

## Implementation shape

- `internal/obsidian/render.go`:
  - New helper (e.g. `fmEdges`) grouping `[]memory.Link` for `m` into the five relation buckets under the endpoint rules above, then emitting one flow-list line per non-empty bucket after `source:`.
  - Reuse `fm`/`yamlScalar`/`fileFor`; no signature change to `renderMemory(m, links, fileFor)`.
- `internal/obsidian/export.go` — unchanged (already passes `fileFor` built over the filtered selection).
- `cmd/ghost/main.go` — unchanged.
- No schema, store, or MCP changes.

## Out of scope (v2 candidates)

Inverse typed fields; strength as a parallel numeric field or nested map; Canvas JSON edges; Bases `.base` views linking the same graph; decisions/tasks as graph endpoints; vault ingestion of user-edited edge fields.
