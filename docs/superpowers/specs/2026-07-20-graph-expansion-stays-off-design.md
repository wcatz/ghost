# Graph-Expansion Stays Off — Decision & Reproducible Diagnostic

**Date:** 2026-07-20
**Status:** Design (pending implementation)
**Decision owner:** wcatz

## Summary

Ghost's link-graph expansion ranking bonus ships disabled
(`DefaultSearchParams().GraphWeight = 0`). This spec makes that a
**documented, evidence-backed decision** rather than an unexplained default,
and ships a **reproducible public diagnostic** that anyone can re-run to
confirm — or falsify — the decision.

The bonus was evaluated against public multi-session retrieval data and lost
cleanly to the cheaper alternative of simply retrieving deeper (larger vector
`k`). This spec does **not** design a new graph mechanism: the kill experiment
returned no-go, so there is nothing downstream to build.

## Background

Graph expansion injects the link-graph neighbors of the top retrieval seeds
into the result set with an additive, strength-scaled bonus and **no
query-relevance gate**. It is controlled by `GraphWeight`, `GraphSeeds` (3),
`GraphHops` (2). Links are built by the background linking worker
(`internal/linking`), which connects each embedded memory to its top-6 cosine
neighbors at similarity `>= 0.70`.

The in-repo `ghost bench` dataset (22 memories) cannot test graph expansion:
it produces only ~2 links and only ~2 golds ever fall out of base top-10. Any
conclusion drawn from it is an artifact of dataset size. The honest test needs
a corpus with genuine multi-session evidence chains — which is exactly what
the public LongMemEval-S benchmark already provides, and which Ghost already
ingests in `bench/longmemeval`.

## The decision bar

The relevant comparison is **not** `graph=0` vs `graph-on`. Links are built
from cosine similarity, and the hybrid searcher's vector leg is *also* cosine.
So graph expansion — "pull in cosine-neighbors of the seeds" — is structurally
an approximation of "retrieve more cosine-neighbors of the query," i.e. a
larger `k`. The bar graph must clear is therefore:

> **graph-on must recover relevant results that `graph=0` with a deeper
> vector-`k` does not.**

If deeper-`k` recovers everything graph recovers, graph is a strictly more
expensive way to get a strict subset — and it stays off.

## Why only cross-session links can matter

LongMemEval labels are **session-level** (`answer_session_ids`), not
memory-level. A multi-session question's evidence spans several sessions;
"recall" means surfacing those answer *sessions*.

This makes the mechanism's necessary condition precise. If a starved base
retrieves memories from session S1 but misses answer sessions S2/S3, graph
can only help if some S2/S3 memory is linked to a *retrieved* S1 memory — a
**cross-session** link. Intra-session links (turns within one already-retrieved
session) are inert: expanding into more S1 memories recovers a session already
counted. So the fraction of cross-session links is not decorative color — it is
the precondition for graph to move the metric at all, and measuring it is what
makes a null result *diagnosable* ("no bridge links form" vs "they form but
deeper-`k` dominates").

## Evidence (preliminary probe)

A throwaway probe over **15** multi-session haystacks (~489 memories each,
threshold 0.70, embeddings from the cached `nomic-embed-text:v1.5` run)
produced three independently decisive results. The committed diagnostic
(below) regenerates these over the full 133-question set; those become the
authoritative numbers in this doc.

| Probe | Preliminary result | Interpretation |
|---|---|---|
| Link composition | 7.3% cross-session, 92.7% intra-session; cross-session links touch an answer session in **15/15** questions | Bridge links **do** form — not a null-link artifact. The mechanism had a fair shot. |
| Reachability (starved base k=5) | graph recovered 7/9 missed answer sessions; deeper-k(30) recovered 9/9; **graph-beyond-deeper-k = 0** | Graph's recoveries are a strict **subset** of deeper-k's. It reaches nothing deeper-k misses, even in a regime engineered to favor it. |
| Natural regime (production k=150) | **0/15** questions miss any answer session | Base retrieval at production depth already finds every answer session. Zero headroom for any intervention. |

**Conclusion:** graph expansion is dominated by deeper-`k` on public
multi-session data, and there is no headroom for either at production depth.
`GraphWeight` stays `0`.

## Deliverable 1 — reproducible diagnostic

A new diagnostic mode in the existing `bench/longmemeval` program.

### Interface

```
go run ./bench/longmemeval --diagnostic graph \
    --data <longmemeval_s_cleaned.json> \
    --embed-cache <nomic-cache.jsonl> \
    [--question-type multi-session] [--limit 0] \
    [--threshold 0.70] [--starved-k 5] [--deeper-k 30] [--hops 2]
```

- `--diagnostic graph` short-circuits **before** the normal
  condition/aggregation loop and runs the probe instead.
- `--question-type` defaults to `multi-session` (133 questions); the flag lets
  a user point it at `temporal-reasoning` or any other type.
- `--limit 0` means all matching questions; a positive value samples the first
  N (for quick local runs).
- `--threshold` defaults to `0.70` to match the shipped linking worker; the
  flag lets a user explore other thresholds.
- `--starved-k`, `--deeper-k`, `--hops` parameterize the reachability arms.

### Behavior

The diagnostic **reuses the existing ingestion path** — `loadQuestions`, the
`cachedEmbedder`, and the same per-turn `fact` ingestion + `memToSession`
mapping used by `rankSessionsForQuestion`. This guarantees it tests the
identical corpus the scored benchmark uses; there is no second ingestion
codepath to drift.

For each matching question it:

1. Ingests every turn as a `fact` with its cached embedding, recording
   `memToSession`.
2. Builds the link graph by looping `linking.Worker.SweepOnce` until
   `UnscannedEmbeddedMemoryIDs` drains (the worker processes `batchSize`=50
   per sweep, so a ~489-memory haystack needs ~10 sweeps).
3. Enumerates unique links and classifies each as cross- or intra-session via
   `memToSession`.
4. Computes the reachability arms: starved base (`SearchHybrid` top
   `--starved-k`), graph expansion (`GraphNeighbors` from the base hits), and
   deeper-`k` (`SearchHybrid` top `--deeper-k`), counting per-question
   recoveries of *missed* answer sessions and the decisive
   graph-beyond-deeper-k count.
5. Computes natural-regime miss rates at k=10 and k=150.

### Output

Three aligned tables printed to stdout (link composition, reachability,
natural regime), plus the run parameters. Deterministic given a fixed embed
cache. This is a **diagnostic**, not a floor-gated CI benchmark: the 1.7 GB
embed cache is not in CI, so it is not wired into `--floors`. It is a
documented, re-runnable local tool.

### Constraints

- No hardcoded paths; every input is a flag.
- `go vet ./...` clean; follows the existing `bench/longmemeval` style.
- Does not alter the existing `fts`/`vector`/`hybrid` condition paths or the
  floor-gate machinery.

## Deliverable 2 — documentation

1. **This spec** records the decision, the bar, the mechanism analysis, the
   evidence, and the reproduce command.
2. **`docs/benchmarks.md`** gains a short "Graph expansion — evaluated and
   rejected" subsection: one paragraph stating the decision, linking to this
   spec, and giving the one-line reproduce command.
3. **Falsification condition (in this spec):** the decision reopens if a future
   run of the diagnostic shows graph-beyond-deeper-k > 0 on a non-trivial
   fraction of questions *and* non-zero base misses at production k=150 — i.e.
   both that graph reaches something deeper-`k` cannot *and* that there is
   headroom to reach it. A change to the embedding model, the link threshold,
   or the linking strategy is the kind of event that warrants re-running it.

## Out of scope

- Any new graph ranking mechanism (protected-head/contested-tail, relevance-
  gated expansion, etc.). The kill experiment returned no-go; there is no
  Stage 2.
- Wiring the diagnostic into CI floors (the embed cache is not available in
  CI).
- Changing `DefaultSearchParams()` — `GraphWeight` is already `0`; this spec
  documents *why* it stays there.

## Testing

- The diagnostic is itself the test harness; correctness is verified by
  running it against the cached dataset and confirming the three tables match
  the preliminary probe's shape (subset relationship, zero production-regime
  misses).
- `go vet ./...` and `go test ./...` pass (the diagnostic mode adds no new
  unit-test surface beyond a smoke check that `--diagnostic graph` parses and
  dispatches).
