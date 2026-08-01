# Near-duplicate demotion at session-start injection

## Problem

Two separate code paths rank and return "top memories," and neither
deduplicates near-identical content:

- `Store.GetTopMemories` (`internal/memory/store.go:431-459`) — backs the
  `ghost_project_context` MCP tool and the project-context/global-context MCP
  resources (`internal/mcpserver/mcpserver.go:602/1384/1561/1581`).
- `loadSessionContext` in `internal/mcpinit/hook.go:212-304` — a completely
  separate raw-SQL query against its own lightweight `*sql.DB` connection,
  never going through `Store` at all. **This is the actual session-start
  hot path** — the `## Ghost context: {name}` block injected by `ghost hook
  session-start` at the start of every session, which the MCP server's own
  instructions tell Claude not to re-fetch via `ghost_project_context`. It is
  the highest-frequency call in the whole system, and the one the 2026-07-31
  eval report's friction items #4/#5 actually describe.

Both rank purely by importance × time-decay × pinned boost (or, for
`loadSessionContext`, a simpler `pinned DESC, importance DESC, updated_at
DESC`) with no dedup step, so two near-identical memories (e.g. a fact
restated after a small edit, or saved twice with slightly different wording)
can both occupy injection slots, wasting budget that could go to distinct
information.

Save-time dedup (`Upsert`, `internal/memory/store.go:333-427`) only catches
same-category Jaccard-overlap near-exact duplicates at write time — it does
not catch pairs that drift into near-duplication after independent edits, or
duplicates across categories.

## Scope

This design covers **both** injection paths above — `GetTopMemories` and
`loadSessionContext` — since demoting duplicates only in the MCP-tool path
would leave the actual session-start text (the one users see, and the one
the eval report's friction items describe) unfixed.

On-demand search (`ghost_memory_search`, which uses RRF fusion +
`demoteSuperseded`/`applyRecency` post-fusion reorder hooks in
`internal/memory/vector.go`) is explicitly deferred to a follow-up design —
it already has an extensible reorder-hook shape that a similar demotion step
can slot into later, but that's separate work.

## Shared implementation, two call sites

`GetTopMemories` and `loadSessionContext` return different shapes (`[]Memory`
vs. `[][3]string`-style tuples) and query through different DB handles (`s.db`
under `Store`'s mutex vs. `loadSessionContext`'s own unlocked read-only
connection), so the demotion logic is split into two pieces so both call
sites can share the DB-facing part without sharing incompatible data shapes:

- `internal/memory.DemotionPenalties(ctx, db *sql.DB, ids []string, pinned
  map[string]bool, threshold float64) (map[string]int, error)` — exported
  (new file `internal/memory/demotion.go`), takes any `*sql.DB`/transaction
  handle so `loadSessionContext` can call it directly against its own
  connection without going through `Store`. Runs the single batched query
  (see Demotion algorithm) and returns the penalty map. `ids` order encodes
  rank (index 0 = highest-ranked); no side effects, no locking — callers that
  need `Store`'s `s.mu.RLock` (i.e. `GetTopMemories`) take it themselves
  around the call, same as every other `Store` method.
- `internal/memory.StableDemote[T any](items []T, id func(T) string, penalty
  map[string]int) []T` — a small generic helper doing the
  `sort.SliceStable` reorder, so `GetTopMemories` (over `[]Memory`) and
  `loadSessionContext` (over its own local struct, see below) each pass their
  own `id` accessor instead of duplicating the sort.

`loadSessionContext`'s memory tuple type changes from `[3]string{id, category,
content}` to a local struct:

```go
type sessionMemory struct {
	ID, Category, Content string
	Pinned                bool
}
```

— needed because the pinned-override rule (see Demotion algorithm) requires
pinned status, which the current query doesn't select. `memories
[][3]string` becomes `memories []sessionMemory` throughout
`loadSessionContext`/`HandleSessionStartHook`; the two existing format call
sites (`hook.go:147-149`) update from `m[1]`/`m[0]`/`m[2]` indexing to
`m.Category`/`m.ID`/`m.Content` field access. `GetTopMemories` needs no shape
change — `Memory.Pinned` already exists.

## Signal: reuse `memory_links`, don't recompute

`internal/linking/worker.go` already runs in the background, computing cosine
similarity between memories and writing `related` edges to `memory_links`
when similarity is at or above `linking.threshold` (default 0.70). The
`memory_links.strength` column stores the actual similarity score, not just a
boolean edge.

Reusing these edges means zero extra embedding computation on the
session-start hot path — the highest-frequency call in the whole system. The
trade-off: a memory saved seconds ago, before the linking worker's next scan,
won't be caught until the worker runs. Given the linking worker's existing
polling interval (2 minutes, `cmd/ghost/main.go`), this lag is acceptable for
an injection-quality feature.

**Known limitation — `strength` is a ratchet, not a live reading.**
`CreateLink`'s upsert (`internal/memory/links.go:43-45`) sets
`strength = MAX(strength, excluded.strength)` on conflict, so a stored
`related` edge's strength only ever goes up, never down, across rescans. If
two memories are edited independently after the edge was written and drift
apart semantically, `memory_links` still reports their old (higher) peak
similarity — demotion could keep suppressing a pair that's no longer actually
near-duplicate. This is accepted as a limitation rather than fixed here:
recomputing a live cosine at injection time would reintroduce the embedding
cost this design exists to avoid, and the linking worker's own ratchet
behavior is out of scope for a demotion-only change (see Out of scope). If
this proves to matter in practice, the fix belongs in the linking worker
(e.g. overwrite instead of ratchet, or expire edges older than N rescans),
not in `GetTopMemories`.

## Threshold: stricter than link-creation

0.70 (the link-creation threshold) is tuned for "genuinely related," not
"redundant." Two memories about the same subsystem can clear 0.70 while
still each carrying distinct information. Demotion needs a stricter cutoff so
it only fires on near-restatements, not topical overlap.

Add `linking.demotionThreshold` (new config key, default 0.90) — independent
of `linking.threshold`, since raising the link-creation threshold globally
would also thin out the link graph used for Obsidian export and supersede
ranking.

## Demotion algorithm

This mirrors the codebase's established batched-lookup + penalty-reorder
pattern (`SupersedesWithin` + `demoteSuperseded` in
`internal/memory/links.go`/`vector.go`) rather than a per-pair query loop —
same shape, new predicate — via the shared `DemotionPenalties`/`StableDemote`
helpers described above, used identically by both call sites.

Given the over-fetched candidate list (see below), in rank order:

1. One query (`DemotionPenalties`): `SELECT source_id, target_id FROM
   memory_links WHERE relation = 'related' AND invalidated_at IS NULL AND
   strength >= ? AND source_id IN (...) AND target_id IN (...)`, parameterized
   over the threshold plus every candidate ID in the list — a single round
   trip regardless of candidate count, exactly like `SupersedesWithin`.
2. Build a `penalty` map over candidate IDs: for each returned pair `(a, b)`,
   increment the penalty of whichever of `a`/`b` ranks lower in the candidate
   list — unless that one is pinned and the other isn't, in which case
   penalize the unpinned one instead regardless of rank. Pinning is an
   explicit user signal to keep something visible, and demotion must never
   penalize a pinned memory in favor of an unpinned one.
3. `StableDemote` sorts the candidate list ascending by penalty (ties keep
   existing rank order) — identical mechanics to `demoteSuperseded`. This
   sinks near-duplicates to the bottom without a separate cluster-detection
   pass: in a transitive cluster (A~B, B~C, A~C all above threshold), each
   lower-ranked member accumulates a penalty from every higher-ranked member
   it matches, so the single highest-ranked survivor naturally ends up with
   the lowest penalty in the cluster.
4. Truncate to `limit` (see over-fetch below). Truncating after the penalty
   sort is what actually removes near-duplicates from the returned set —
   the sort alone only reorders.

## Backfill via over-fetch

To avoid returning fewer than the caller's requested `limit` after truncation
removes demoted candidates, both call sites over-fetch before demoting:

- `GetTopMemories` fetches `limit * 2` candidates from the existing ranked
  SQL query (mirroring the `limit * 3` over-fetch pattern
  `ghost_memory_search`'s category filter already uses), applies the demotion
  reorder above, then truncates to `limit`.
- `loadSessionContext` changes its memories query's `LIMIT 25` to `LIMIT 50`
  (same `limit * 2` ratio), applies the same reorder, then truncates to 25 in
  Go before building `sessionMemory` values for display. The tasks/decisions
  queries in the same function are untouched — this design only covers the
  memories list.

If fewer than `limit` survive even after over-fetching — the over-fetch
window itself didn't contain enough non-duplicate candidates — return what's
left rather than erroring or fetching further. This mirrors the already-
shipped behavior of `ghost_memory_search`'s category filter, which signals
possible under-return rather than guaranteeing an exact count.

## Error handling

If the batched `memory_links` query (`DemotionPenalties`) fails (DB error),
fail open: log the error and return the un-demoted, over-fetched-then-
truncated ranked list. Demotion is a quality enhancement, not a
correctness-critical path — session-start injection must never fail outright
because a secondary dedup query broke. This matches `demoteSuperseded`'s
existing non-fatal-on-error behavior in `internal/memory/vector.go`.
`loadSessionContext` has no `*slog.Logger` available today — it logs via
`fmt.Fprintln(os.Stderr, ...)` on failure (matching how the rest of that
function already swallows non-fatal query errors with a bare `_ =` or a
best-effort fallback, e.g. `bumpSessionCount`'s doc comment).

## Testing

Unit tests for `DemotionPenalties`/`StableDemote` in
`internal/memory/demotion_test.go`:

1. A simple pair above `demotionThreshold` demotes the lower-ranked one.
2. A pair below `demotionThreshold` (but above `linking.threshold`) is left
   alone — related, not redundant.
3. Pinned always survives even when its own rank score is lower than its
   unpinned near-duplicate's.
4. A 3-memory mutual cluster (all pairwise above threshold) collapses to one
   survivor.
5. A `memory_links` query error returns a non-nil error from
   `DemotionPenalties`, and the caller falls back to the undemoted list
   without propagating an error further.

Call-site tests:

6. `internal/memory/store_test.go`: `GetTopMemories` backfill — requesting
   `limit` returns exactly `limit` results when the over-fetch window
   contains enough non-duplicate candidates to cover the gap left by
   demotion.
7. `internal/mcpinit/hook_test.go`: `loadSessionContext` (or a helper it
   delegates to) exercises the same backfill case against its own query
   shape, confirming the `LIMIT 50` → demote → truncate-to-25 pipeline
   behaves the same way as `GetTopMemories`'s.

## Out of scope

- Search-time (`ghost_memory_search`) demotion — follow-up design.
- Cross-project demotion — both `GetTopMemories` and `loadSessionContext` are
  already scoped to one project plus `_global`; demotion operates only within
  that existing candidate set.
- Unifying `GetTopMemories` and `loadSessionContext` into one code path.
  They stay architecturally separate (`loadSessionContext` deliberately
  avoids depending on `Store`/the heavier MCP-server machinery for hook
  startup isolation) — this design only makes their ranking behavior
  consistent by sharing the `DemotionPenalties`/`StableDemote` helpers, not
  by merging the two functions.
- Changing `linking.threshold` or the linking worker's scan behavior,
  including its `strength` ratchet (see Known limitation above) and its
  `maxCandidates = 6` per-memory neighbor cap (`internal/linking/worker.go:35`).
  That cap means a memory already has at most 6 `related` edges to begin
  with, so this design's over-fetch window relies on the worker having
  linked the actual near-duplicate within that top-6 — an existing linking-
  worker constraint this design inherits rather than changes.
