# Eval Findings Remediation — Action Plan

**Date:** 2026-08-02
**Source:** `docs/superpowers/reports/2026-08-02-ghost-eval.md`, cross-checked by independent opus and fable brainstorm agents (2 agents × 4 problem areas)

## Context

The 2026-08-02 eval run (`ghost eval`, run without API cost — CLI/sqlite-tier fallback confirmed) surfaced four recurring quality problems. This document is the synthesized design across all four, decomposed into four independent sub-projects. Each sub-project gets its own implementation plan (via `writing-plans`) and its own PR — they touch different code paths and should not be bundled.

Priority order (by severity — content loss first):

1. Duplicate-merge data loss (highest severity — silent content loss, 5 independent eval hits)
2. Reflect/consolidation quality (content loss, "mixed" quality rating)
3. Silent truncation (visibility/UX, not loss — the underlying data survives, just isn't shown)
4. Replay/wrong-session save quality (detectability, not preventable at the store layer)

---

## 1. Duplicate-merge data loss

**Root cause:** `Upsert` (`internal/memory/store.go:349`) finds near-duplicate candidates via `sanitizeFTS`-truncated FTS probe (first 10 words only, top 5 by rank), then on a Jaccard/overlap match over some threshold, overwrites content destructively — longest string wins, full replacement, no union. For `source='manual'` targets it silently discards the new content entirely while still reporting `merged=true` to the caller.

**Design:**
- `Upsert` never mutates existing content in place on a near-dup match. It inserts the new memory as its own row (as if no match were found), and creates a `memory_links` edge to the matched row carrying the match score and a `kind` marking it as a lexical near-dup (distinct from the existing similarity-linking worker's cosine-based links, so the two link sources stay distinguishable).
- Existing-row importance/access_count bump on match stays as-is — that part isn't lossy.
- The `ghost_memory_save` tool result text surfaces the match (existing memory ID + score) so the calling agent sees it immediately instead of a silent no-op.
- Actual consolidation of true duplicates is left entirely to the existing `ghost resolve`/`ghost supersede` LLM-classified machinery, which can already reason about content — it just needs to also consider these new lexical-dup links as a signal, not just cosine-similarity links.
- Secondary fix, same PR: `sanitizeFTS`'s 10-word truncation and the top-5 candidate cap make matching non-deterministic (a near-dup with its distinguishing content past word 10 may never surface as a candidate at all). Broaden the probe (e.g., significant-terms extraction instead of a raw word-count cutoff) so the candidate set is stable regardless of which part of the content differs.

**Schema change required (verified against `internal/memory/schema.go:186-197`):** `memory_links.relation` is `CHECK (relation IN ('related','supersedes','contradicts','elaborates','causes'))` and `source` is `CHECK (source IN ('auto','llm','manual'))`. The existing cosine-similarity linking worker already writes `relation='related', source='auto'` (`internal/linking/worker.go:107`) — reusing those values for the new lexical-dup edge would make it indistinguishable from a cosine link, defeating the whole point of tagging it separately. This needs:
- A `migrateV3`-style table rebuild (SQLite can't `ALTER` a `CHECK`; follow the rebuild pattern already used in `migrateV1`, `internal/memory/migrate.go:81-108`) adding `'duplicate'` to the `relation` CHECK list.
- `'duplicate'` is **not** added to `symmetricRelations` (`internal/memory/links.go:24`) — unlike `'related'`, it must preserve direction (`source_id` = the newly-saved memory, `target_id` = the existing matched memory) so a future dedup-resolution pass can tell which side is which. `related`'s (min,max) ID normalization is deliberately skipped for this relation.

**Interface changes:** `Upsert`'s signature/return likely gains the matched-ID+score in its bool→struct return (currently `(string, bool, error)`); update the one `provider.MemoryStore` interface definition and its mock/test implementations.

**Test plan:** the current dup-merge tests assert postconditions the redesign deliberately breaks and must be **rewritten, not extended** — confirmed by reading `store_test.go`: `TestStoreUpsert` (~line 79-119) asserts `len(all) == 1` and `id2 == id1` after a merge; `TestStoreUpsertMergesBestCandidate`, `TestStoreUpsertMergesLengthAsymmetricParaphrases`, and `TestStoreUpsertImportanceCap` make similar single-row assumptions. All of these need new assertions: two rows exist, a `'duplicate'` link connects them with the right direction, and original content on both rows is untouched. Also verify `source='manual'` targets no longer silently drop new content.

---

## 2. Reflect/consolidation quality

**Root cause:** every existing safety net (`ReplaceNonManual`'s empty-set guard, the 30% consolidator-fallback threshold, the 50% warning in `main.go:350` that doesn't block `--apply`) counts *rows*, not *content*. Both real eval failures (21/26=81%, 11/13=85% retained) pass every threshold trivially. Two active prompt lines in `BuildReflectionPrompt` (`internal/reflection/prompt.go`) cause the loss directly:
- `"Aim for 10-25 high-quality memories, not 50 repetitive ones"` (~line 110) — pressures over-merging.
- `"Drop stale situational memories (old gotchas that were fixed)"` (~line 107) — no way for the model to distinguish truly-stale from a live guardrail; `ghost resolve` already owns deprecation via `resolved_at` more safely.

**Design (layered, cheapest fix first):**
1. **Prompt fix (no schema change, ship first):** remove the "drop stale situational memories" instruction entirely — deprecation is `ghost resolve`'s job, not reflection's. De-emphasize the numeric target ("10-25... not 50") so the model isn't pressured to hit a count.
2. **Structural fix (schema change, ships after #1 is validated):** give the model stable per-memory IDs in the reflection prompt and require explicit accounting in its output — every input ID must appear in the output as either kept, `merged_from` (listing source IDs), `category_changed`, or `dropped` (with a one-line reason). Validate this in Go before `ReplaceNonManual` is called: reject on orphaned input IDs (input ID accounted for nowhere), unaccounted-for output claims, excessive fan-in (e.g., >4 memories merged into one — likely bundling unrelated content), or a `dropped` reason that doesn't reference staleness/supersession.
   - Note: neither `ReflectionInput` nor `ReflectMemory` (`internal/reflection/prompt.go:10-33`) carries a per-memory ID today, so this is new prompt plumbing, not just a phrasing change. Use small ephemeral per-run indices (1, 2, 3…) rather than the real 32-char hex memory IDs — a fast/small model transcribing dozens of random hex strings verbatim is a real transcription-failure risk; short sequential indices are far more likely to round-trip correctly and can be mapped back to real IDs in Go after validation.
3. Any validation failure **fails closed** on `--apply` — the run is treated as dry-run-only output and requires an explicit `--force` to actually apply, mirroring how `reflect --apply` already requires explicit opt-in today. A lightweight token/keyword-overlap heuristic (no schema change) is the fallback check to run if the model doesn't reliably populate the new accounting fields.

**Why layered:** the prompt fix is cheap, safe, and independently valuable even if the schema change never ships. The accounting/validation layer is what actually catches "3 unrelated gotchas silently bundled into one memory" — a heuristic content-overlap check alone would pass that case, since the merged memory legitimately contains all three token sets.

**Files:** `internal/reflection/prompt.go` (prompt text), `internal/reflection/consolidator.go` (new validation call site), `cmd/ghost/main.go:~295` (where the new validator gate slots into `runReflect`, before the existing 50%-warning check).

---

## 3. Silent truncation (context injection / search)

**Root cause:** the session-start hook (`internal/mcpinit/hook.go`) already fixed this in PR #227/#228 — it renders "N shown of M total — K not shown" and a `…` marker via `truncateUTF8`. That fix was never generalized to the MCP-tool-side readers: `ghost_project_context` handler, `buildProjectContext`, the `ghost://memories/global` resource, and `loadGlobalMemories`'s separate LIMIT-15 fetch. All of these still drop rows with no indication to the caller.

Additionally, `buildProjectContext` (`internal/mcpserver/mcpserver.go:~1610`) **double-renders `_global` memories**: its first `GetTopMemories(projectID, 20)` call already includes `_global` rows (per `GetTopMemories`'s own WHERE clause, `project_id=? OR project_id='_global' AND resolved_at IS NULL`), then a second, separate `GetTopMemories("_global", 15)` call re-fetches the same rows — burning project-memory display budget on duplicates.

A naive fix (bolt `CountMemories` onto `GetTopMemories`'s result to build a footer) is wrong on its own: `CountMemories`'s WHERE clause (`project_id=?` only) doesn't match `GetTopMemories`'s (`_global`-inclusive, `resolved_at`-filtered) — the two would silently disagree on totals.

**Design:**
- Change `GetTopMemories`'s signature to return `(memories []Memory, total int, err error)`, computing `total` from the exact same WHERE predicate used for the row selection — one query path, not two independently-maintained ones. Update the `provider.MemoryStore` interface and all call sites.
- Fix the `buildProjectContext` double-fetch: the dedicated `_global` call should only run if globals aren't already saturating the first 20-slot query, or the two calls should be merged into one query with a shared dedup by ID.
- Wire the hook's existing footer wording ("N shown of M total — K not shown") into the three still-missing call sites: `ghost_project_context` tool response, the `ghost://memories/global` resource, and `buildProjectContext`'s rendered output.
- Align `truncateUTF8` in `mcpserver.go` (~line 1666) with `hook.go`'s version — the mcpserver copy is missing the `…` marker.

**Files:** `internal/memory/store.go` (`GetTopMemories` signature), `internal/provider/provider.go` (interface), `internal/mcpserver/mcpserver.go` (`buildProjectContext`, `ghost_project_context` handler, global resource handler, `truncateUTF8`).

**Test plan:** extend `mcpserver_test.go` / `store_test.go` coverage asserting footer text appears exactly when `total > shown`, and that project + global memory sets returned by `buildProjectContext` never contain a duplicate ID.

---

## 4. Replay / wrong-session save quality

**Root cause:** Ghost cannot verify that saved content actually reflects the current session's source material — this is fundamentally unpreventable from the memory-store side. The eval observed saves that read as plausible but didn't match what actually happened in that session (a "replay" mismatch).

**Design:**
- Primary mechanism: a server-stamped `session_key` column (new, via a `migrateV3`-style nullable `ADD COLUMN`, following the existing `migrateV2` pattern in `internal/memory/migrate.go`), populated automatically from `req.Session` (already used for MCP sampling in `ghost_resolve`, `mcpserver.go:976-979`) — zero agent cooperation required, so it fires even when the agent is confidently wrong about its own save. This makes wrong-session saves *detectable and bulk-correctable* after the fact (e.g., a future `ghost resolve`-style pass that can group/flag saves by session and cross-check against what that session's transcript actually covered).
- Secondary, optional: a soft citation nudge in the `ghost_memory_save` tool description (mirroring `ghost_save_global`'s existing "don't just copy tool output, describe what you actually learned" language) — not a required field. A *mandatory* provenance field was considered and rejected: it trades one already-observed failure (wrong-source saves) for a worse one also independently observed (under-saving, because a required field adds friction to the save path).
- Both fixes must also patch `Upsert`'s merge branch (see area 1): today a merge silently drops tags on the losing side, and would silently drop `session_key`/provenance too if added naively. Since area 1's fix makes merges non-destructive (new row + link, no overwrite), this is automatically resolved once area 1 ships — **area 4 should ship after area 1**, not in parallel, to avoid stamping a field that immediately gets dropped by the old destructive-merge path.

**Files:** `internal/memory/schema.go` (new column), `internal/memory/migrate.go` (new `migrateV3`), `internal/memory/store.go` (stamp `session_key` on create), `internal/mcpserver/mcpserver.go` (`ghost_memory_save` handler — read `req.Session`, tool description nudge).

---

## Sequencing across sessions

1. **Area 1 (dup-merge)** — do first; area 4 depends on it, and it's the highest-severity content-loss bug.
2. **Area 2 (reflect quality)** — independent of area 1, can be done in parallel across sessions if needed. Ship the prompt-only fix (2a) as its own small PR before attempting the schema/validator layer (2b).
3. **Area 3 (truncation)** — independent, can slot in anytime.
4. **Area 4 (replay/session_key)** — after area 1 merges.

Each area becomes its own `writing-plans` implementation plan and its own feature-branch PR, per standing workflow (never commit to `main` directly, `go vet`/`go test` before commit, no AI attribution in commit messages).
