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
same shape, new predicate.

Given the over-fetched candidate list (see below):

1. One query: `SELECT source_id, target_id FROM memory_links WHERE
   relation = 'related' AND invalidated_at IS NULL AND strength >=
   linking.demotionThreshold AND source_id IN (...) AND target_id IN (...)`,
   parameterized over every candidate ID in the list — a single round trip
   regardless of candidate count, exactly like `SupersedesWithin`.
2. Build a `penalty` map over candidate IDs: for each returned pair `(a, b)`,
   increment the penalty of whichever of `a`/`b` ranks lower in the candidate
   list — unless that one is pinned and the other isn't, in which case
   penalize the unpinned one instead regardless of rank. Pinning is an
   explicit user signal to keep something visible, and demotion must never
   penalize a pinned memory in favor of an unpinned one.
3. `sort.SliceStable` the candidate list ascending by penalty (ties keep
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
removes demoted candidates, `GetTopMemories` fetches `limit * 2` candidates
from the existing ranked SQL query (mirroring the `limit * 3` over-fetch
pattern `ghost_memory_search`'s category filter already uses), applies the
demotion reorder above, then truncates to `limit`.

If fewer than `limit` survive even after over-fetching — the over-fetch
window itself didn't contain enough non-duplicate candidates — return what's
left rather than erroring or fetching further. This mirrors the already-
shipped behavior of `ghost_memory_search`'s category filter, which signals
possible under-return rather than guaranteeing an exact count.

## Error handling

If the batched `memory_links` query fails (DB error), fail open: log the
error and return the un-demoted, over-fetched-then-truncated ranked list.
Demotion is a quality enhancement, not a correctness-critical path —
session-start injection must never fail outright because a secondary dedup
query broke. This matches `demoteSuperseded`'s existing non-fatal-on-error
behavior in `internal/memory/vector.go`.

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
- Changing `linking.threshold` or the linking worker's scan behavior,
  including its `strength` ratchet (see Known limitation above) and its
  `maxCandidates = 6` per-memory neighbor cap (`internal/linking/worker.go:35`).
  That cap means a memory already has at most 6 `related` edges to begin
  with, so this design's over-fetch window relies on the worker having
  linked the actual near-duplicate within that top-6 — an existing linking-
  worker constraint this design inherits rather than changes.
