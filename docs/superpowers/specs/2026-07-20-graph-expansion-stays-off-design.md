# Remove Graph-Expansion Ranking Bonus — Decision & Cleanup

**Date:** 2026-07-20
**Status:** Design (pending implementation)
**Decision owner:** wcatz

## Summary

Ghost's link-graph expansion ranking bonus ships disabled
(`DefaultSearchParams().GraphWeight = 0`). It was evaluated against public
multi-session retrieval data (LongMemEval-S) and lost cleanly to the cheaper
alternative of simply retrieving deeper (a larger vector `k`). We are not going
to revisit it.

This spec therefore **removes the dead machinery** rather than documenting a
default nobody will change or maintaining a harness to re-litigate a settled
question. It leaves a short rationale in its place so the decision is legible
and no one re-adds the mechanism blindly.

## Why remove instead of document-and-keep

The instinct to keep a "reproducible diagnostic + falsification condition"
assumes the decision might flip on new data. It won't, because the reason it
loses is **structural, not a benchmark artifact**:

- Links are built from cosine similarity (the linking worker connects each
  memory to its top cosine neighbors ≥ 0.70).
- The hybrid searcher's vector leg is *also* cosine.
- So graph expansion — "pull in the cosine-neighbors of the top seeds" — is an
  approximation of "retrieve more cosine-neighbors of the query," i.e. a larger
  `k`. Deeper-`k` reaches a superset of what graph reaches, more cheaply.

A kill experiment on public LongMemEval-S multi-session haystacks confirmed
this empirically: graph's recoveries were a strict subset of deeper-`k`'s, and
at production retrieval depth base search already surfaced every answer session
(zero headroom for any intervention). Full findings are recorded in the Ghost
decision record and the commit message; they are not reproduced here because
the code that produced them is being removed.

Since the conclusion follows from the architecture, a permanent re-runnable
diagnostic earns nothing — it would only re-derive a fact the design already
implies. YAGNI applies: delete the dead lever, keep a note.

## What is dead vs. load-bearing

Verified by tracing consumers across the codebase.

**Dead — only ever fed the disabled bonus, safe to remove:**

- `Store.GraphNeighbors` and the `GraphNeighbor` struct
  (`internal/memory/links.go`). Sole consumer is `applyGraphBonus`, inside the
  `GraphWeight > 0` guard that never fires in production.
- `SearchParams.GraphWeight` / `GraphSeeds` / `GraphHops` fields and their
  defaults (`internal/memory/vector.go`).
- `Store.applyGraphBonus` and the `if p.GraphWeight > 0 && p.GraphSeeds > 0`
  block in `fuseAndRank` (`internal/memory/vector.go`).
- The graph-ablation arm in the synthetic bench harness: `candidateGraphWeight`
  and `pGraph` in `internal/bench/runner.go`, the `GraphWeight` grid in
  `internal/bench/sweep.go`, and `internal/bench/graph_test.go`.

**Load-bearing — must NOT be touched:**

- The linking worker (`internal/linking`) and the `'related'` links it builds.
  They also feed the **Obsidian vault graph** — `obsidian/export.go` renders a
  memory's links into its note (`GetLinks` → `renderMemory`). Removing the
  worker would degrade a shipped feature.
- The `memory_links` table, `CreateLink` / `GetLinks` / `InvalidateLink`.
- The entire `supersedes` machinery — `SupersedesWithin`, `demoteSuperseded`,
  `ghost supersede`, the staleness suite. It is independent of the graph bonus
  and drives real ranking demotion.

The removal is surgical: only the graph-*bonus* traversal and its parameters
go. The link graph itself stays, because other features depend on it.

## The mention that stays

1. **`SearchParams` / `DefaultSearchParams`** (`internal/memory/vector.go`):
   the current multi-line `GraphWeight` comment is replaced by a one- or
   two-line note at the top of `DefaultSearchParams` (or the type doc):
   *"A link-graph expansion bonus was evaluated and removed — it was
   structurally dominated by a deeper vector-`k` (links and the vector leg are
   both cosine). The link graph is retained for Obsidian export and supersedes
   ranking. See docs/architecture.md."*
2. **`docs/architecture.md`** (and the existing `memory_links` line in
   `CLAUDE.md`, updated in a follow-up): one paragraph recording that graph
   expansion was evaluated against public LongMemEval-S and removed as dominated
   by deeper-`k`, and that the link graph now serves only Obsidian export and
   supersedes.
3. **The commit message** carries the empirical kill-experiment numbers for the
   git record; the Ghost decision record (`AD0310468A0E17128E3FED6AA941CDE2`,
   already updated) is the durable home for the reasoning.

## Benchmark data policy (unchanged, affirmed)

No change to how benchmarks run; this is only stated so the cleanup does not
disturb it. CI regression gating already runs on public data — the
`longmemeval` workflow downloads `xiaowu0162/longmemeval-cleaned` and gates on
metric floors. The synthetic `internal/bench` harness is a local-only report
tool (hand-authored synthetic Cardano/Ghost-domain text, no private data); it
is kept for the time-decay signals LongMemEval cannot exercise — the staleness
and recency-trap suites. This cleanup only removes that harness's dead
graph-ablation arm; its fts/vector/hybrid conditions and staleness/recency
suites are untouched.

## Out of scope

- Any new or redesigned graph ranking mechanism. The kill experiment returned
  no-go; there is no Stage 2.
- Removing or altering the linking worker, `memory_links`, Obsidian export, or
  the supersedes machinery.
- Building a reproducible diagnostic or CI gate for graph expansion (the whole
  point of removal is that we won't re-run it).
- Changing any non-graph search parameter or the floor-gate machinery.

## Testing

- `go vet ./...` and `go test ./...` pass after removal. Existing tests that
  reference the graph params (`internal/bench/graph_test.go`, the graph rows of
  `sweep_test.go`) are removed or trimmed with them; no fts/vector/hybrid or
  staleness/recency test loses coverage.
- Manual check: `ghost obsidian export` still renders `'related'` links into
  vault notes (confirms the linking worker and `GetLinks` path are intact after
  the surgical removal).
