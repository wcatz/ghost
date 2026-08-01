# Near-duplicate demotion at session-start injection

## Problem

`Store.GetTopMemories` (`internal/memory/store.go:431-459`), which backs session-start
context injection, ranks candidates purely by importance × time-decay × pinned
boost. It has no dedup step, so two near-identical memories (e.g. a fact
restated after a small edit, or saved twice with slightly different wording)
can both occupy injection slots, wasting budget that could go to distinct
information. This is root-cause fix #3 of 3 identified in the 2026-07-31 Ghost
eval report (friction items #4/#5).

Save-time dedup (`Upsert`, `internal/memory/store.go:333-427`) only catches
same-category Jaccard-overlap near-exact duplicates at write time — it does
not catch pairs that drift into near-duplication after independent edits, or
duplicates across categories.

## Scope

This design covers **session-start injection only** (`GetTopMemories`).
On-demand search (`ghost_memory_search`, which uses RRF fusion +
`demoteSuperseded`/`applyRecency` post-fusion reorder hooks in
`internal/memory/vector.go`) is explicitly deferred to a follow-up design —
it already has an extensible reorder-hook shape that a similar demotion step
can slot into later, but that's separate work.

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

Given the ranked candidate list (see over-fetch below):

1. For each ordered pair of candidates `(a, b)` where `a` ranks higher than
   `b`, check `memory_links` for a `related` edge between them with
   `strength >= linking.demotionThreshold`.
2. If found, one of the pair is dropped:
   - If neither is pinned, or both are pinned: drop `b` (the lower-ranked).
   - If exactly one is pinned: drop the *unpinned* one, regardless of rank —
     pinning is an explicit user signal to keep something visible, and
     demotion must never drop a pinned memory in favor of an unpinned one.
3. Repeat pairwise across the surviving set until no remaining pair exceeds
   the threshold. This collapses transitive clusters (A~B, B~C, A~C all above
   threshold) down to one survivor per cluster, one pair-comparison at a
   time, without needing separate cluster-detection logic.

## Backfill via over-fetch

To avoid returning fewer than the caller's requested `limit` after demotion
removes candidates, `GetTopMemories` fetches `limit * 2` candidates from the
existing ranked SQL query (mirroring the `limit * 3` over-fetch pattern
`ghost_memory_search`'s category filter already uses), applies demotion, then
truncates to `limit`.

If fewer than `limit` survive even after over-fetching — the over-fetch
window itself didn't contain enough non-duplicate candidates — return what's
left rather than erroring or fetching further. This mirrors the already-
shipped behavior of `ghost_memory_search`'s category filter, which signals
possible under-return rather than guaranteeing an exact count.

## Error handling

If the `memory_links` lookup fails (DB error), fail open: log the error and
return the un-demoted, over-fetched-then-truncated ranked list. Demotion is a
quality enhancement, not a correctness-critical path — session-start
injection must never fail outright because a secondary dedup query broke.

## Testing

Unit tests in `internal/memory/store_test.go`:

1. A simple pair above `demotionThreshold` demotes the lower-ranked one.
2. A pair below `demotionThreshold` (but above `linking.threshold`) is left
   alone — related, not redundant.
3. Pinned always survives even when its own rank score is lower than its
   unpinned near-duplicate's.
4. A 3-memory mutual cluster (all pairwise above threshold) collapses to one
   survivor.
5. Backfill: requesting `limit` returns exactly `limit` results when the
   over-fetch window contains enough non-duplicate candidates to cover the
   gap left by demotion.
6. A `memory_links` query error falls back to the undemoted, truncated list
   without returning an error to the caller.

## Out of scope

- Search-time (`ghost_memory_search`) demotion — follow-up design.
- Cross-project demotion — `GetTopMemories` is already scoped to one project
  plus `_global`; demotion operates only within that existing candidate set.
- Changing `linking.threshold` or the linking worker's scan behavior.
