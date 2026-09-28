# Benchmarks and methodology

Ghost publishes benchmark results together with the harness, inputs, and limitations needed to reproduce them. The guiding rule is simple: **a score is useful only when someone can re-run the evaluation and understand what it measures**.

## At a glance

| Evaluation | What it measures | Headline result |
|---|---|---|
| LongMemEval-S retrieval | Judge-free retrieval against official evidence labels | Hybrid Recall@5 **93.0%**, Recall@10 **97.3%** on 470 answerable questions (measured pre-task-prefix — re-baseline pending, see Phase 1) |
| `ghost bench` | Deterministic in-repo retrieval regression suite | Hybrid NDCG@10 **0.818** on 220 queries and 551 memories |
| LongMemEval-S end-to-end | Retrieve → generate → judge with DeepSeek v4 Pro | **96.2%** blended accuracy across 500 questions (its hybrid retrieval leg is pre-task-prefix too — see Phase 4) |
| Staleness suite | Fresh-fact ranking without breaking older-but-correct facts | Fresh-wins **1.000** while the recency-trap case stays **0.929** |
| Maintenance-state suite | Ranking over a corpus with resolved, shared and superseded rows | Hybrid live-wins **0.810** on 21 questions; the graded table cannot see this class of change at all |
| No-answer queries | What search returns when nothing in the corpus answers the query | Mean top cosine **0.584** vs **0.741** answerable; 51/220 answerable queries sit at or below the no-answer maximum |

These rows are not one leaderboard. Retrieval metrics, end-to-end answer accuracy, a staleness fixture, a maintenance-state fixture and a false-positive count answer different questions. Competitor scores also use different generators and judges, so cross-system comparisons are directional unless the evaluation protocol is identical.

**Status:** LongMemEval-S retrieval, `ghost bench`, and the documented end-to-end run have shipped. The staleness, maintenance-state and no-answer suites are report-only in CI. The official GPT-4o leaderboard-comparable run has not been executed.

> Sections explicitly labeled **Historical record** document past experiments and their original implementation details. They are retained for reproducibility context, not as a description of current production routing. For current behavior, start with the [documentation index](README.md).

For the product overview, see [`../README.md`](../README.md). For the implementation that produces these results, see [`architecture.md`](architecture.md). For the built-in command, see [`cli.md`](cli.md#context-and-benchmarks).

## Why these benchmarks and not others

- **LongMemEval** ([arXiv 2410.10813](https://arxiv.org/abs/2410.10813), ICLR 2025) is the primary long-term-memory benchmark for this project: 500 questions, each with a haystack of chat sessions. The 470 answerable questions carry official evidence labels (`answer_session_ids`); the remaining 30 are abstention cases with no evidence labels, excluded from retrieval scoring. Crucially it supports a **retrieval-only evaluation** using those labels — no LLM judge, no API cost, fully deterministic.
- **LoCoMo** is not the primary benchmark, but a judge-free comparability layer is shipped below. Public audits found ~6.4% of its answer key wrong, its standard judge accepts a majority of intentionally wrong answers, and trivial baselines (full-context, even filesystem+grep) beat specialized memory systems on it. Ghost therefore reports LoCoMo retrieval as a separate, directional comparison rather than folding it into the primary leaderboard claim.
- **Zep's DMR** is not used as a Ghost benchmark — 60-message conversations fit trivially in any context window; Zep itself moved on from it.

## Phase 1 — LongMemEval-S retrieval-only (judge-free) — SHIPPED

The harness lives at `bench/longmemeval/` (standalone program, not in the ghost binary). Per question it ingests every haystack turn into a fresh in-memory Ghost store, runs Ghost's production search, collapses ranked memories to unique sessions (first occurrence wins), and scores against the official `answer_session_ids` evidence labels on the 470 non-abstention questions. No LLM judge; deterministic given the embedding cache. Dataset: **`longmemeval_s_cleaned.json`** from [`xiaowu0162/longmemeval-cleaned`](https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned) — the current canonical variant (the original HF dataset is deprecated); numbers are not directly comparable to runs on the original -S files.

Results (2026-07-15, `nomic-embed-text:v1.5` local embeddings; per-question logs committed at `bench/longmemeval/results/`):

```text
condition   R@1     R@5     R@10    MRR@10  NDCG@10   (session-level, n=470)
fts-only    0.429   0.751   0.832   0.758   0.738     44s wall
vector      0.558   0.926   0.968   0.911   0.909     ~1m wall on a warm embedding cache
hybrid      0.532   0.930   0.973   0.901   0.903     one-time local embedding ~12h on ARM64 CPU
```

- **Hybrid session Recall@5 is 93.0%, Recall@10 97.3%** — in the band of the best-reported hybrid retrieval results on -S (~95% R@5 published for hybrid BM25+vector on the original variant) and far above the paper's flat-index baseline (R@5 ≈ 0.64 on -M).
- **The lift lands exactly where the architecture predicts.** FTS alone nearly solves keyword-friendly classes (`single-session-user` R@10 1.000) but fails vocabulary-mismatch classes; embeddings fix precisely those: `single-session-assistant` R@10 **0.607 → 1.000**, `temporal-reasoning` 0.767 → 0.938.
- **Honest nuance: on this chat-style benchmark, vector-only ties hybrid** (vector edges R@1/MRR/NDCG, hybrid edges deep recall R@5/R@10). On the current v2 `ghost bench` dataset, hybrid beats vector (NDCG 0.818 vs 0.800) — exact identifiers (ports, versions, hostnames) need the keyword leg. Fusion is the robustness play across both data shapes, which is exactly why a memory system for coding agents ships it.
- **Remaining headroom is at R@1** (0.532 overall; `multi-session` 0.371, `temporal-reasoning` 0.379) — R@10 is close to saturated, so the next win is ranking, not recall.
- Reproduce: `go run ./bench/longmemeval --data <longmemeval_s_cleaned.json> --condition fts|vector|hybrid --embed-cache <cache.jsonl>`. The append-only content-hash cache makes reruns and interruptions cheap. The hash is taken over the **prefixed** input (`search_document: ` / `search_query: `, the same two the production client applies), so since the bench harnesses started applying those prefixes, cache entries written by older builds hash differently and are never hit again — an old cache file is inert, not wrong, and the first prefixed run re-embeds the corpus once.
- **CI gating:** only the **fts** floor (`R@5 ≥ 0.74`, `NDCG@10 ≥ 0.72`) is enforced automatically on PRs — it needs no Ollama and finishes fast. The **hybrid** floor (`R@5 ≥ 0.91`, `NDCG@10 ≥ 0.89`) is run **manually** (`workflow_dispatch`) or locally, not on a schedule: the cold embedding pass is CPU-bound (the ~12h above), too slow for any CI cap. Because `nomic-embed-text:v1.5` is deterministic, a cold run computes the same vectors as a warm one, so the manual gate is justified by the warm local numbers here without CI re-deriving them — **but those numbers were measured before the harness adopted the task prefixes, and that space has since changed, so until the hybrid run below is re-baselined the manual hybrid gate is a floor-check, not a valid regression signal**: it can tell you a prefixed run is below a bar, not that this change made it worse (see the re-baseline note below). **Re-baseline pending:** the results table, the per-class claims above and the committed per-question logs predate the harness adopting the `search_document: `/`search_query: ` prefixes (the vector space the published numbers were measured in can no longer be reproduced by this harness), so a prefixed hybrid run measures a different space than the one that set these floors — re-run it to re-baseline the hybrid numbers, then restore the gate's regression meaning.

## Phase 1b — end-to-end anchors (for later comparison)

Published end-to-end (answer-accuracy) numbers use a GPT-4o judge and a generator that dominates the score — see Phase 4. Retrieval-only numbers above are not comparable to those percentages.

## Phase 2 — `ghost bench`: an in-repo dataset + CI regression floors — SHIPPED

`ghost bench` runs a self-authored graded dataset (in `internal/bench/testdata/`) with a committed real `nomic-embed-text:v1.5` embedding fixture, so CI runs the vector/hybrid conditions with no Ollama. The harness (`internal/bench/`) drives Ghost's production `SearchFTS`/`SearchVector`/`SearchHybrid` over a fresh in-memory store and scores judge-free IR metrics.

Current numbers (v2 dataset: 551 memories spanning all 8 categories, 220 graded queries with heavy paraphrase/vocab-mismatch coverage; retrieval-only, no LLM judge; fully deterministic — reproduce with `go run ./cmd/ghost bench` after rebuild):

```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.467   0.625   0.697   0.836   0.749
vector-only      0.503   0.694   0.764   0.882   0.800
hybrid           0.520   0.712   0.763   0.902   0.818
```

Every row above was measured on this build from the committed dataset and
fixture, and reproduces exactly with `go run ./cmd/ghost bench` (or
`go test ./internal/bench -run TestBenchDatasetReport -v`). The corpus grew by
four `validity_*` rows carrying `valid_from`/`valid_until`/`verified_at`, and
three things moved with it that need separating, because only one of them is a
ranking change:

- **The four rows moved the numbers by at most 0.001 on the gated metrics.**
  `fts-only` R@5 0.626 → 0.625, `vector-only` R@1 0.506 → 0.503 and
  NDCG@10 0.801 → 0.800, `hybrid` unchanged on all five. The corpus grew
  547 → 551, and four new candidates now compete for a ten-row window, so a
  query that filled its window from eleven candidates fills it from fifteen.
  That is the cost of a larger corpus rather than a ranking regression — the
  shipped path did not move — and every condition stays inside the 0.005
  NDCG@10 / R@5 tolerance the context-assembler plan applies to each of its
  ranking-affecting PRs (the plan's own comparison contract, measured on the
  branch against `origin/main`; there is no CI job asserting it). The floors
  `TestBenchRegressionFloors` does enforce — NDCG@10 0.73/0.78/0.80 and
  recall@10 0.67/0.75/0.75 — are met with the same headroom as before, which
  is why the `fts` CI job stays green. Nothing in these conditions reads a validity
  column: they call `SearchFTS`, `SearchVector` and `SearchHybrid` directly, so
  the new rows are inert here on purpose. What the corpus now carries is a
  validity window for the assembler's own condition to act on, which is where
  a stage-2 filter becomes measurable (PR 7 of
  [`docs/superpowers/specs/2026-09-25-context-assembler-design.md`](superpowers/specs/2026-09-25-context-assembler-design.md)).
  `TestBuiltinDatasetCarriesValidityIntoRetrieval` keeps that claim honest: it
  reads the rows back out of `Store.Candidates` — the read stage 2 consumes
  — with the stamps still attached, and
  `TestValidityFixtureCoversEveryStage2State` fails if the four stop covering
  every state stage 2 reads.
- **The `embeddings.json` fixture was added to, never rewritten.** The four new
  vectors were embedded through `internal/embedding`'s `EmbedDocument`, the same
  client and `search_document: ` prefix that produced the committed ones, and
  they were merged into the fixture by a one-off script that read the existing
  keys, re-encoded them through the same JSON writer and refused to write unless
  the result decoded to the original map. That script was not committed: it is
  four keys in and nothing out, and a tool that exists to preserve one file's
  keys is a thing to keep only while keys are being added. A future
  regeneration goes through the route
  [in this section](#phase-2--ghost-bench-an-in-repo-dataset--ci-regression-floors--shipped) — `EmbedDocument` for memory keys and
  `EmbedQuery` for query names, not raw `/api/embed` calls, which is the mistake
  the prefix-free fixture used to carry — and that route rewrites every key, so
  its diff is expected to be whole-file. Re-embedding an existing key reproduces
  the committed vector exactly on the current model, so this table is measured in
  the same space as the one before it. Reproduce the additive shape against this
  PR's own base with
  `git diff --numstat $(git merge-base origin/main HEAD) -- internal/bench/testdata/embeddings.json`
  — 3080 insertions, 0 deletions, and the 3080 is four keys of 770 lines (one key
  line, 768 floats, one closing line). Reproduce the numbers with
  `go run ./cmd/ghost bench`.
- **The no-answer report moved with the corpus**: a floor that refuses all the
  no-answer queries now costs 51/220 answerable queries rather than 52, because
  the new rows are vector neighbours for one more answerable query.
  Report-only, no gate.

The bullets below record the earlier fixture regeneration, which is what moved
the vector legs. Their before/after figures are re-measurements of the
547-memory corpus, so they no longer describe the table above exactly; they still
describe the change that caused them, which is what they are for.

- **The `vector-only` and `hybrid` rows come from the committed
  `embeddings.json`**, which is produced by `nomic-embed-text:v1.5` **with its
  task prefixes** — `search_document: ` on each memory, `search_query: ` on each
  query, the same two the production client applies. Prefix-free embedding is
  the mistake that fixture used to carry. Measured against the same build with
  the pre-regeneration fixture, the prefixes moved hybrid NDCG@10
  0.814 → 0.818 and R@5 0.697 → 0.712, and vector-only NDCG@10 0.795 → 0.801
  with R@1 0.484 → 0.506. Vector-only R@5 gives back 0.014 (0.708 → 0.694)
  while its top-of-list metrics all improve: the prefixes make the two halves
  of the space more separable, which sharpens the head of the pure vector
  ranking at some cost to its deep tail. Hybrid — the shipped path — improves
  R@1, R@5, MRR@10 and NDCG@10, and gives back 0.014 on R@10
  (0.777 → 0.763): the sharper vector head takes window slots the deep tail
  used to hold, and fusion does not fully replace them. The two gated metrics,
  R@5 and NDCG@10, both improve.
- **The `fts-only` row moved by 0.001–0.008 and had nothing to do with the
  fixture** — it never reads a vector. It is in this diff because the table it
  replaces had drifted from the committed dataset: it published
  `fts-only 0.469 0.623 0.689 0.837 0.748` where the committed `memories.jsonl`
  and `queries.jsonl` produce `0.467 0.625 0.697 0.836 0.749` (measured on
  `origin/main` with the old fixture, so the drift predates the regeneration).
  The vector rows of that published table were stale for the same reason, which
  is why the "before" numbers quoted above are a re-measurement rather than the
  published ones: a delta against a table no build produces would be meaningless.

To regenerate the fixture after a dataset change, embed `memories.jsonl` and
`queries.jsonl` through `internal/embedding`'s client — `EmbedDocument` for the
memory keys and `EmbedQuery` for the query names, at the configured
`embedding.dimensions` — and write the result as a single JSON object of
key → vector, keys sorted. Going through the client is what keeps the fixture in
the same space as production, prefixes and all; a fixture built by hand from raw
`/api/embed` calls drifts the moment either side changes.

Two findings, both honest:

- **Hybrid fusion earns its keep.** Hybrid NDCG@10 (0.818) beats both single legs (FTS 0.749, vector 0.800) — the 70/30 RRF weighting is a net win on this dataset. `TestBenchRegressionFloors` asserts this relationship so a regression trips CI. Absolute numbers are lower than the v1 starter because v2 deliberately adds paraphrase queries where lexical overlap is weak (the FTS leg's R@1 falls to 0.467; vector and hybrid carry those).
- **The graph-expansion bonus was evaluated and removed.** An additive link-graph bonus (former 0.15 default) lifted semantically-adjacent neighbors above exact matches, and a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth. The former `GraphWeight` setting and the bonus are now removed entirely (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`). The link graph is retained for the Obsidian mirror and `supersedes` ranking.

**What this table cannot see.** The v2 corpus is the *graded retrieval* dataset, and it is deliberately clean: every memory is created through `store.Create` in one batch, so all 551 share a `created_at` and the decay factor is identical across every candidate — inert, and pinned by `TestDecayDoesNotPerturbGradedBench`. It also holds no resolved row, no `_global` row and no `supersedes` edge. A ranking change that acts on any of that measures 0.000 on this table, which is exactly what happened when the resolved/`_global` demotion shipped: measured on one fixture, `f3a80f7` (pre-#634) and `main` both read 0.818 here. That is a property of the corpus, not a bug in the harness, so the coverage lives elsewhere: the [maintenance-state suite](#phase-3b--maintenance-state-suite-report-only) and the [no-answer queries](#no-answer-queries-the-abstention-baseline-report-only).

The v2 dataset overshoots the original ~150/~40 growth target (551/220) to give distractor density room for paraphrase grading. Regression tests assert **metric floors** (a little below observed), not exact rankings, since RRF scores can tie.

### Parameter sweep (`ghost bench --sweep`)

The RRF fusion is parameterized (`memory.SearchParams`), and `ghost bench --sweep` grid-searches the vector-leg weight (FTS = complement) — 6 combinations over the same dataset, one prepared store. Findings on the v2 dataset (full table: run `go run ./cmd/ghost bench --sweep`):

- **Leg weights remain robust, and the default still wins.** On 220 queries, vec 0.70 (shipped default) tops the grid at NDCG 0.818, level with vec 0.80; 0.60/0.90 are within 0.004; only vec 0.30 degrades (0.790). The earlier v1 sweep's "0.3–0.7 flat" band does not fully carry over — the paraphrase-heavy queries reward a stronger vector leg — but there is still no evidence to move off 70/30.
- **Outcome: the 70/30 leg weighting ships unchanged, and the graph bonus was removed.** With the leg weights robust across the upper half of the grid, there is no evidence to change the shipped 70/30 split. The graph-expansion bonus was removed rather than kept disabled (see the spec linked above); the link graph is still built for the Obsidian mirror and `supersedes`.

## Phase 3 — staleness suite (the flagship)

Deterministic scenarios for the failure users actually complain about: agents acting on superseded facts ("prod runs Postgres 14" retrieved after the migration to 16). Modeled on the MemTrace error taxonomy ([arXiv 2605.28732](https://arxiv.org/abs/2605.28732)) and STALE probe design ([arXiv 2605.06527](https://arxiv.org/abs/2605.06527)):

- Save fact v1; later save superseding v2; assert search ranks v2 above v1 (**fresh-wins rate**, **fresh@1**), including for queries that presuppose the outdated state, and across update chains (v1→v2→v3).
- Deletion regressions: reflection must never drop pinned or manual memories (codifies the existing empty-set guard and snapshot behavior).
- Runs in CI in seconds. No LLM judge.

This suite was designed to *fail* at first — production search had no decay signal in `SearchHybrid` ranking (decay lived only in `GetTopMemories`). At the pre-decay shipped default it reported **fresh-wins 0.083** (fresh-found 1.000 — the update is always retrieved, just out-ranked by its shorter, older original). With `DecayEnabled: true` (the current default, reorder-only) it reports **fresh-wins 1.000** with trap correct-wins flat at **0.929** (`TestDecayFrontier`). It lands in CI as **report-only**; scenarios graduate to enforced assertions as the fix ships.

### Category-aware time decay (default on, reorder-only)

`SearchParams.DecayEnabled` (default true) applies the category-aware time-decay factor `decayFactor(category, pinned, ageDays)` — the Go mirror of `DecayRankingSQL` — to reorder the result window after truncation: results are truncated to `limit` by base score first (fused RRF score, or the FTS-only synthesized score `1/(RRFK+rank+1)`), then the surviving window is reordered by `base × decayFactor`. `decayFactor` is `1.0` for pinned memories and for `preference`/`convention`/`fact` (never decay); `MAX(0.3, 1/(1+ageDays/45))` for `pattern`/`architecture` (τ=45); `MAX(0.15, 1/(1+ageDays/30))` for every other category (τ=30). Age is read from `created_at` only (never `updated_at`); an unparseable timestamp is treated as ancient so it can never spuriously win. Decay is ordering-only: it never changes membership, which is what keeps findability intact — see the reorder-only rationale in `docs/superpowers/specs/2026-08-20-search-time-decay-design.md` (the synthesized FTS base spans ~1.3× over the window while decay spans ~5.4×, so multiplying before truncation lets decay override relevance and drops rank-1 relevant fresh answers for unrelated younger memories).

With decay on, the staleness suite **flips from fresh-wins 0.083 to 1.000** (`TestStalenessDecayProof`; the `dependency`-category fixture makes decay observable). It is provably inert on the graded benchmarks: those datasets seed via `store.Create`, which never sets `created_at`, so every candidate shares a timestamp and the decay factor is identical across them — no reorder possible (`TestDecayDoesNotPerturbGradedBench`).

**Why category-awareness is what makes decay defaultable — the recency-trap experiment.** The predicted risk was that a global recency prior can't tell "superseded" from "old-but-still-true." A second fixture (`internal/bench/testdata/recency_trap.jsonl`) tests the opposite of staleness: the *older* memory is the correct answer, with a newer keyword-overlapping distractor that recency would wrongly promote (`correct-wins` = correct outranks every trap). Sweeping the rejected blanket prior (`RecencyWeight`/`RecencyTau`, `final = base * (1 + RecencyWeight · recency(age))`, `recency = 1/(1+age/RecencyTau)`) against both suites at once (`TestRecencyFrontier` in its original form) is not a gentle tradeoff — it's a cliff:

```text
recency   staleness-fresh   trap-correct   min(both)
0.00      0.083             0.929          0.083
0.05      0.750             0.214          0.214   ← best min(both)
0.10      0.917             0.071          0.071
0.15      0.979             0.000          0.000
0.25+     1.000             0.000          0.000
```

At *every* weight that meaningfully helps staleness, the trap collapses. The best achievable `min(both)` is 0.214 — i.e. there is no global recency weight where both old-but-correct and newer-supersedes retrieval are acceptable, because the only signal (age) is exactly the thing that conflates the two cases. **Verdict: the blanket age-only recency prior is not defaultable and was removed.** Category-aware decay resolves the cliff because the trap suite's memories are `fact` category, which never decays — under `TestDecayFrontier` the frontier collapses to two points `decay-off 0.083/0.929 → decay-on 1.000/0.929`, so staleness flips while the trap stays flat. That never-decay exemption is what lets `DecayEnabled` ship on by default.

**The real fix is targeted, and it clears the frontier.** `SearchParams.SupersedeDemote` (default true, alongside `DecayEnabled`) consumes directed `supersedes` links: within the result window it demotes a memory below every present memory that supersedes it (penalty = count of present superseders, stable-sorted — so update chains order correctly given star links, and it is a hard no-op when no supersedes edge joins two results). Because it only ever acts on genuine replacement pairs, it does what no blanket age-only prior could (`TestSupersedeDemoteClearsFrontier`):

```text
                        staleness fresh-wins   recency-trap correct-wins
both off                0.083                  0.929
decay on (DefaultSearchParams)          1.000                  0.929   ← decay alone flips staleness, trap untouched (fact never decays)
supersede demote on     1.000                  0.929   ← likewise, trap untouched (no supersession edge)
both on (shipped default)              1.000                  0.929
```

The trap is untouched in both cases: under decay its distractors are `fact` (never-decay, so decay never fires), and under demote they are *not* supersession pairs (no `supersedes` edge, so the demote never fires). That is the free lunch the blanket-recency frontier proved a global prior can't be.

**Both halves now ship. Creation:** `ghost supersede <project> [--apply]` (`internal/supersede`) proposes newer→older candidate pairs from cosine-similar memories (tighter than the 0.70 'related' floor), confirms them with batched CLI-harness classify calls (up to 8 pairs per call), and writes star `supersedes` links (`source='llm'`). It is re-runnable and self-heals after reflection cascade-deletes links (`ReplaceNonManual` reinserts memories with new IDs — a re-run rebuilds the links, exactly as the cosine worker rebuilds 'related'). The cosine worker is rejected as the creator itself — symmetric similarity can't assign direction (the failure that got the graph bonus disabled), so similarity only *proposes* and the LLM *confirms + directs*. The classifier prompt is biased toward NO (a false supersedes buries a valid memory), and on a labeled set of 10 genuine-vs-parallel pairs plus the 4 real pairs [#641](https://github.com/wcatz/ghost/issues/641) was filed from, both the single-pair and batched paths are scored against one table (`TestRelationClassifierLive`, `TestRelationClassifierLiveBatch`; run manually against a CLI harness, skipped when none is installed). The maintenance benchmark behind #641 judged 4 of 7 links wrong on a real database — one backwards, two `causes` between status reports of one open issue, one event record superseded by a later unrelated event on the same host — so the prompt now also shows each note's `created_at` (the pass orients a pair by `updated_at`, which is wrong whenever a note was re-saved after the fact it reports), offers a fourth verdict `reversed`, forbids `causes` between two status reports of one issue, and states that an event record is never superseded by a later unrelated event on the same host. `reversed` is the one wrong answer code can refuse: a `supersedes` link only points newer→older, so the pass reports and drops it instead of writing it backwards, and never caches it. `internal/supersede/regression_test.go` holds the four pairs as a labeled regression set, anonymized (the shape of each pair is the signal; the text is not).

**Consumption has now graduated; creation stays opt-in.** `DefaultSearchParams` ships `DecayEnabled: true` and `SupersedeDemote: true`, so production `SearchHybrid` / `SearchHybridAll` (i.e. `ghost_memory_search` and `ghost_search_all`, including their FTS-only fallbacks) apply decay reordering and consume `supersedes` links. Creation of `supersedes` links remains opt-in — links only exist if the user ran `ghost supersede --apply` — so the demote is still a hard no-op for anyone who has not asked for it, and the numbers above are what back the flip: decay alone moves staleness fresh-wins 0.083 → 1.000 with recency-trap correct-wins unchanged at 0.929 (`TestDecayFrontier`); the demote does the same 0.083 → 1.000 with trap flat at 0.929 (`TestSupersedeDemoteClearsFrontier`). Either half alone clears the frontier, and both on keeps it cleared. It was flipped because an eval run found the opposite failure: a memory the user had explicitly marked as replaced still outranked its replacement in live search. `TestProductionSearchDemotesSuperseded` (internal/memory) guards the production entry points; `SearchHybridParams` still takes explicit params for the sweep harness.

### Decay re-selection (evaluated, NOT default)

`SearchParams.DecayReselect` (default **false**) keeps the top `limit*2` by base score, then selects the top `limit` by `base × decay` — the fix for "a fresh memory ranked below the pure-base cut is never rescued." Probed via `TestDecayReselectProbe` (`GHOST_BENCH_PROBE=1`):

```text
                  graded hybrid NDCG@10   staleness fresh-found/wins   trap correct-wins
reselect=false    0.818                   1.000 / 1.000                0.929
reselect=true     0.818                   0.938 / 0.938                0.929
```

**Verdict: not defaultable.** The wider base window **regresses staleness** — `default_branch` (both probes) and `vpn_solution` (state probe) lose the fresh version entirely — while graded and trap stay flat. Ship gate requires staleness not to regress; the flag stays off by default and is available for future experiments behind `SearchParams.DecayReselect`. Reorder-only membership (relevance owns the cut) remains the shipped behavior.

## Phase 3b — maintenance-state suite (report-only)

The graded table above cannot see the state a real store accumulates, because its corpus has none of it. This suite is that corpus: a small graded dataset — 65 memories and 21 questions in `internal/bench/testdata/maintenance_{memories,queries}.jsonl`, plus a committed `nomic-embed-text:v1.5` vector fixture, so it runs in CI with no Ollama. Its memories carry a `resolved_at` verdict (17 of them), live in `_global` while being searched from the project (16), or sit behind a `supersedes` edge (11), and their `created_at` spans 10 to 950 days across decaying and never-decaying categories, so the decay factor has something to separate. Resolved rows are stamped through the production `SetResolved`, which refuses `convention`/`preference`, so the corpus can only describe a state a store could actually hold.

The vectors carry the same task prefixes the production client applies and the committed graded fixture was regenerated with — `search_document: ` on each memory, `search_query: ` on each question (`internal/embedding`'s `EmbedDocument`/`EmbedQuery`) — so this suite is measured in the same vector space as the table above and as a live store. Regenerate with Ollama's `/api/embed` through those two methods when the corpus changes.

The answer is the live project memory; the distractors are the copies that state leaves behind — a resolved restatement, a shared `_global` copy, a superseded version. Three questions invert it: the answer *is* the shared `_global` row, which is how the suite shows whether demoting shared rows makes shared knowledge unfindable. **`importance` is not correlated with role** (a stale copy is sometimes the most important-looking row in the store), and `TestMaintenanceFixtureCarriesState` enforces that at least ten live-probe questions have a distractor at least as important as their answer — the anti-construction guard the recency-trap fixture lacks.

Run it with `GHOST_TEST_SOURCE=none go test ./internal/bench/ -run TestMaintenanceStateReport -v` (shipped defaults):

```text
condition         n     R@5  NDCG@10  live-wins answer-found
fts-only         21   0.929    0.883      0.762        1.000
vector-only      21   0.976    0.833      0.571        1.000
hybrid           21   0.905    0.583      0.810        0.905
fts-only: shared _global answers found 3/3, live-wins 3/3
vector-only: shared _global answers found 3/3, live-wins 2/3
hybrid: shared _global answers found 1/3, live-wins 0/3

hybrid: 2 question(s) where a resolved/_global/superseded copy outranked the answer:
  q_grafana
  q_yaml
hybrid: 2 question(s) whose answer was not retrieved at all (evicted or never matched):
  q_commits
  q_verify
```

The two single-leg rows are the raw legs, exactly as in the Phase 2 table. That is not the same as production's keyword-only fallback (Ollama down), which still goes through fusion and therefore *does* get the status demotion and decay — it ranks like the hybrid row, not like the fts-only row. Only the hybrid row is the shipped ranking. The two lost-question lists are kept apart because they are different failures: an answer that was never returned is not something a copy outranked.

The three `shared _global answers` lines are the suite's only view of whether a single leg can find a shared row at all, and they are the clearest statement of what the demotion cost: fts-only still returns 3 of 3 shared answers and vector-only 3 of 3, while the fused hybrid path returns **1 of 3**. Fusion is what loses them, not either leg.

**Report-only, and that is a decision, not an omission.** No metric floor is asserted: the suite exists to move when ranking changes, and a floor would freeze today's numbers into a gate on the next fix — including a gate against the fix for the finding below. What is asserted is that the suite can see at all — the fixture carries resolved, `_global` (including a decaying-category one) and superseded state, and with decay off the `supersedes` edges demonstrably raise live-wins (`TestMaintenanceSupersedeEdgesMoveLiveWins`; 0.524 → 0.857). Delete the edges and that test fails; that is the mutation check.

Three things it says that the graded table cannot, each measured on **one fixture** with only the ranking code differing:

| | graded hybrid NDCG@10 | maint. R@5 | maint. NDCG@10 | maint. live-wins | maint. answer-found | shared answers found |
|---|---|---|---|---|---|---|
| `f3a80f7` (pre-#634) | 0.818 | 0.714 | 0.534 | **0.476** | 1.000 | 3/3 |
| `main` (demotion shipped) | **0.818** | 0.905 | 0.583 | **0.810** | **0.905** | **1/3** |

- **The demotion moves this suite and not the graded table** — live-wins 0.476 → 0.810 while the 220-query table reads 0.818 on both. That 0.000 is what made the change look inert, and it is why this suite exists.
- **The demotion costs findability, and only this suite can see it.** `answer-found` falls 1.000 → 0.905 and only **1 of 3** shared-row answers is still retrieved: halving a shared row's fused score puts it below the deepest live candidate, and the factor is applied before the window cut, so two shared answers are *evicted* rather than sunk. A demotion that sinks a row is a reorder; one that evicts it is a deletion, and a user whose only record of a preference is the shared row stops retrieving it. Whether the fix is a rescue lane for shared rows or a demotion that reorders after the cut is a ranking decision (#580) — recorded here so it inherits the number instead of re-deriving it.
- **Category-exempt decay and resolve state pull against each other, and most of that pull was the missing demotion.** Shipped: live-wins 0.810 (NDCG 0.583). With decay off: 0.857 (NDCG 0.904) — a 0.047 live-wins gap, down from 0.429 before #634. The mechanism is visible question by question: a resolved `fact` never decays (factor 1.0) while a live `dependency` copy decays (0.33–0.75 at 10–80 days). Neither the staleness suite nor the recency-trap suite can see this, because in both the correct answer sits in a never-decay category — which is exactly the "old-but-correct memory in a decaying category" case the audit asked for. Not asserted, on purpose: it is a residual finding for the ranking work, and the day it is fixed this suite should show it.

## No-answer queries: the abstention baseline (report-only)

Recall cannot see a leak. A wrong memory returned counts as a hit for whatever it displaced, so a system can score well while feeding an agent plausible garbage — which is what [#537's negative retrieval fixtures](https://github.com/wcatz/ghost/pull/537) pin at the unit level (a named memory must not surface, each with a reason). What they cannot do is measure a query nothing answers, because recall has no denominator and `runCondition` skips such queries entirely.

`testdata/negative_queries.jsonl` adds 24 of them to the graded corpus, with `rel` empty by construction, in two flavors: `off_domain` (no vocabulary overlap with the corpus — the floor) and `near_miss` (corpus vocabulary, unrecorded answer: "who is on call this weekend", "how do i restore a dropped postgres table by hand"). `ghost bench` prints them after the main table:

```text
no-answer queries (n=24, nothing in the corpus answers these; report-only, no gate)
  results returned per query   10.0 (window 10, no similarity floor configured)
  mean top cosine             0.584  vs 0.741 for the 220 answerable queries
  floor refusing all of them   0.697 (the no-answer maximum) costs 51/220 answerable queries

  flavor          n    results/query     mean top
  near_miss      12             10.0        0.623
  off_domain     12             10.0        0.545

  floor     results/query   queries w/ hit
  0.30              10.00         24/24
  0.40               9.79         24/24
  0.50               6.83         21/24
```

**Every score here is the row's own cosine, read from its stored vector** — not a lookup in the vector leg's fetched list. A hybrid result can arrive on the keyword leg alone (the keyword reservation guarantees the top keyword hits a place in the window whatever their cosine), and reading such a row's score from a truncated list yields 0, which counts it below every floor: the floor rows were undercounting precisely the results that came from the leg with no score of its own. The cosine is also the only calibrated number in the pipeline — an RRF score is a function of a row's rank rather than of its match, and the keyword-only fallback's synthesized `1/(K+rank+1)` is a position, not a confidence.

Two conventions, stated because both change what the numbers mean:

- **The score of a query is the best cosine among the rows production returns for it**, and the no-answer set and the answerable contrast are both measured that way, so the two distributions differ only in whether the corpus has an answer. Those two can genuinely diverge from each other — a demoted row can be the corpus's strongest match and sit outside the window.
- **"Above a floor" means strictly above**, which is the rule production itself applies (`memory.filterVectorFloor` keeps a candidate when `score > floor`). So a floor set at the no-answer maximum already refuses the answerable queries whose best score ties it, which is why that count includes ties.

The floor rows are a sweep, not a proposal — `search.min_similarity` ships 0, so there is no configured floor to inherit, and the band is where one would have to live. They are also a stricter reading than the shipped flag, which applies a floor to the vector leg *before* fusion and therefore never touches a keyword-only result: the rows answer "how strong are the results a caller actually receives", not "what would the flag do". The flavors are reported apart because a pooled mean would let the easy half carry the hard one: a near-miss that reuses corpus vocabulary scores 0.623 against the off-domain floor's 0.545, and it is the 0.623 any abstain rule has to clear.

**What this suite asserts, and what it does not.** Nothing here gates the ranking. The enforced claims are about the *fixture and the report plumbing*: both flavors are present and their counts survive into the report, the searches returned something (otherwise the mean is a vacuous 0), one row per configured floor exists, the counts add up, and the maximum is not below the mean drawn from it. The near-miss flavor must not score *below* the off-domain one — that is a statement about the fixture being labelled correctly, not about the ranking. Notably **absent**: any assertion that the two distributions are separated. That is a claim about today's ranking, and the plausible abstention fix this baseline exists for — returning fewer, more similar rows for a query nothing answers — would move the no-answer mean up and trip a test whose job is to watch that fix land. The separation is reported, not asserted.

Read the third line as the actual baseline for the abstention work: **the two distributions overlap.** A floor of 0.697 would refuse all 24 no-answer queries and would also refuse 51 of the 220 answerable ones, so a threshold alone cannot abstain — the near-miss flavor is what makes the overlap visible, and it is why the answer is likely to be a calibrated decision rather than a constant.

Cost: the block runs the production search for 244 queries (24 no-answer plus the 220 answerable contrast) so that every returned row can be scored from its own vector, which takes `ghost bench` from about 3.5s to about 9s. That is the price of scoring the window rather than a short list; a future change that wants the same numbers faster has to make the production search hand back its legs (`searchHybridLegs` already does, for explain mode).

## Phase 4 — end-to-end LongMemEval-S (retrieve → generate → judge) — SHIPPED (DeepSeek v4 Pro)

The pipeline ([`bench/longmemeval/phase4/`](../bench/longmemeval/phase4/)) is four stages: Ghost retrieves (Go, `-retrieval-out ranked.jsonl`), `merge_retrieval.py` folds the ranking into the dataset, and `phase4_run.py` generates hypotheses then judges them. Generation prompt assembly (`prepare_prompt`) and the yes/no grading templates (`get_anscheck_prompt`) are imported **verbatim** from an upstream LongMemEval checkout — only the API client is swapped — so numbers stay reproducible against the published harness. Both stages are append-only and resume-safe. To reproduce any reported score, run the full pipeline:

```bash
# 1. Retrieve (Ghost Go harness)
go run ./bench/longmemeval -data longmemeval_s_cleaned.json \
    -condition hybrid -ollama http://localhost:11434 \
    -embed-cache ~/.cache/ghost-bench/embed-cache.jsonl \
    --include-abstention \
    -retrieval-out ranked.jsonl

# 2. Merge retrieval into dataset
python bench/longmemeval/phase4/merge_retrieval.py \
    --dataset longmemeval_s_cleaned.json --retrieval ranked.jsonl --out merged.json

# 3. Generate hypotheses (DeepSeek v4 Pro via OpenCode Go shown; swap provider/model for other runs)
export OPENCODE_API_KEY="your-key"
python bench/longmemeval/phase4/phase4_run.py generate \
    --provider openai --model deepseek-v4-pro \
    --api-base-url https://opencode.ai/zen/go \
    --longmemeval-src .cache/LongMemEval/src \
    --dataset merged.json --out hyp.jsonl

# 4. Judge + report
python bench/longmemeval/phase4/phase4_run.py judge \
    --provider openai --model deepseek-v4-pro \
    --api-base-url https://opencode.ai/zen/go \
    --longmemeval-src .cache/LongMemEval/src \
    --dataset merged.json --hyp hyp.jsonl
```

See the [phase4 README](../bench/longmemeval/phase4/README.md) for full setup (LongMemEval checkout, API keys, cost estimates).

### Supported providers

The provider adapters in Phase 4 are **benchmark-only**. They call the selected provider directly for the standalone generation/judge harness; the Ghost runtime itself does not expose an Anthropic API client and routes memory maintenance through the selected CLI harness. `ANTHROPIC_API_KEY` references in this section therefore describe the benchmark's independent Claude generator/judge, not a Ghost runtime requirement.

| Provider | Endpoint | Notes |
|----------|----------|-------|
| `openai` (default) | `api.openai.com` | Leaderboard-comparable with gpt-4o |
| `openai` + `--api-base-url` | Any OpenAI-compatible | **OpenCode Go** (`https://opencode.ai/zen/go`), DeepSeek direct, etc. |
| `anthropic` | `api.anthropic.com` | Internal check, not leaderboard-comparable |

The recorded run used the OpenCode Go endpoint; provider pricing and usage limits change over time, so re-check them before reproducing. The `--api-base-url` flag routes requests through any OpenAI-compatible endpoint; a `User-Agent` header is included for Cloudflare compatibility, and `GoUsageLimitError` responses auto-sleep until the rate limit resets.

Results (2026-08-20, DeepSeek v4 Pro as both generator and judge, **500 questions** including 30 abstention, `topk_context=5`):

The hybrid leg's retrieval comes from the same `bench/longmemeval` hybrid path as Phase 1, so it was measured in the pre-task-prefix vector space; the hybrid retrieval numbers here are re-baseline pending alongside Phase 1's (`fts-only` is unaffected — it never embeds).

```text
condition   blended(500)  non-abstention(470)  abstention(30)
hybrid      96.2%         96.8%                86.7%
fts-only    83.4%         83.6%                80.0%
```

Per-category breakdown (non-abstention):

| Question type | Hybrid | FTS-only | Delta |
|---|---|---|---|
| single-session-user (64) | 100.0% | 98.4% | +1.6pp |
| single-session-assistant (56) | 98.2% | 67.9% | +30.3pp |
| single-session-preference (30) | 96.7% | 80.0% | +16.7pp |
| multi-session (121) | 92.6% | 72.7% | +19.9pp |
| temporal-reasoning (127) | 97.6% | 88.2% | +9.4pp |
| knowledge-update (72) | 98.6% | 94.4% | +4.2pp |

Not leaderboard-comparable (DeepSeek v4 Pro, not GPT-4o), but the retrieval → answer pipeline is identical to the official harness. The blended score (500 questions) enables fair comparison with competitors who include abstention in their aggregates.

### Competitor comparison (500-question blended)

| System | Score | Generator | Source |
|--------|-------|-----------|--------|
| **Ghost (hybrid)** | **96.2%** | DeepSeek V4 Pro | This repo — retrieval leg pre-task-prefix, re-baseline pending (see Phase 4)¹ |
| Mem0 | 94.4% | Not specified | [mem0.ai/research](https://mem0.ai/research) — "managed platform, proprietary optimizations not in OSS SDK" |
| Hindsight | 91.4% | Gemini-3 Pro | [arxiv 2512.12818](https://arxiv.org/abs/2512.12818), [benchmarks](https://benchmarks.hindsight.vectorize.io/) — independently validated by Virginia Tech + Washington Post |
| Supermemory | 85.2% | Gemini-3 | [supermemory.ai/research](https://supermemory.ai/research/longmembench/) — self-reported |

**Read carefully:** These numbers are **not directly comparable** across rows. Each uses a different generator model and (in Ghost's case) a different judge. Within the same generator+judge pair, differences are meaningful — across pairs, they're directional only. Ghost's self-judged score carries the same caveat as every other system that judges its own output.

¹ Ghost's row additionally rests on retrieval measured before `bench/longmemeval` adopted the task prefixes, so the gap to the rows below it is not a like-for-like current measurement of Ghost's retrieval — re-baseline pending (see Phase 4).

### Cost

Estimated cost at `topk_context=5` over 500 questions: ~$3-5 (DeepSeek V4 Pro via OpenCode Go), ~$20 (gpt-4o gen+judge), ~$24 (claude-sonnet-5 gen+judge). Use `cost_estimate.py` (no API calls) to re-anchor before spending. Temperature 0, single recorded run, per-case results JSON and full logs committed, an explicit note that the memory system never saw oracle context.

Reference points, all judged with the official GPT-4o harness but with **different generators** (which dominate the score — compare within-generator only): Zep 71.2% and full-context 60.2% (GPT-4o generator); Mastra 94.87% (gpt-5-mini generator; 84.23% with GPT-4o); agentmemory 96.2% (Claude Opus 4.6 generator, temperature 0).

### Abstention Scoring

The official LongMemEval benchmark includes 30 abstention questions (question_id suffix `_abs`) where the correct behavior is to decline answering. Most third-party implementations score abstention via LLM judge (did the system correctly refuse?) and fold it into headline accuracy.

**Previous approach:** Excluded abstention from scoring (470-question accuracy). This inflated scores compared to competitors who include abstention.

**Current approach:** 
- Phase 1 (retrieval-only): Excludes abstention (IR metrics can't evaluate "no evidence" questions)
- Phase 4 (end-to-end): Includes all 500 questions with abstention-specific judge prompts
- Reports three accuracy numbers:
  - **Blended (500-question):** Fair comparison with competitors
  - **Non-abstention (470-question):** Backward-compatible with previous reports
  - **Abstention (30-question):** Shows refusal accuracy

**Why this matters:** Abstention is typically where retrieval-heavy systems perform worst — over-eager retrieval hallucinates answers instead of declining. Including abstention in the score provides a more honest assessment of system capabilities.

## LoCoMo comparability run (`bench/locomo`) — SHIPPED (retrieval-only)

[LoCoMo](https://arxiv.org/abs/2402.17753) is where mem0 and competitors publish, so Ghost runs it as a **comparability layer** — LongMemEval-S remains the primary benchmark. Methodology differences are explicit: LoCoMo here is judge-free turn-level retrieval over 1,531 scored questions (adversarial excluded), evidence-label scored like Phase 1.

```bash
mkdir -p .cache/locomo
curl -sL -o .cache/locomo/locomo10.json \
  https://raw.githubusercontent.com/snap-research/LoCoMo/main/data/locomo10.json
go run ./bench/locomo --condition hybrid \
  --embed-cache ~/.cache/ghost-bench/locomo-cache.jsonl \
  --retrieval-out ranked.jsonl --lmeval-out locomo-lmeval.json
```

**Ghost retrieval, LoCoMo locomo10 (2026-08-21, n=1531):**

Hit@k is a question-level hit rate — 1 when any gold `dia_id` lands in the top-k, not the fraction of gold items retrieved (multi-evidence questions diverge from true recall). MRR/NDCG credit every gold item.

| Condition | Hit@1 | Hit@5 | Hit@10 | MRR@10 | NDCG@10 | SessHit@5 |
|---|---|---|---|---|---|---|
| FTS | 0.265 | 0.484 | 0.581 | 0.362 | 0.384 | 0.823 |
| Hybrid | 0.380 | 0.653 | 0.758 | 0.497 | 0.522 | 0.886 |

This run (2026-08-21) predates `bench/locomo` adopting the `search_document: `/`search_query: ` task prefixes, so the Hybrid row is measured in the pre-prefix vector space and is re-baseline pending like Phase 1's hybrid numbers (the FTS row is unaffected — it never embeds).

Per category (hybrid): temporal Hit@5 0.719 / open-domain 0.668 / single-hop 0.598 / multi-hop 0.449. Multi-hop is the known hard tail.

The `--lmeval-out` dataset is LongMemEval-schema, so phase4's `merge_retrieval.py` + `phase4_run.py` run **verbatim** against it for the judged end-to-end number. Not a CI gate — report-only, mirroring the hybrid precedent.

### Judged smoke (2026-08-21, n=40 per-type×10)

Generation + judge both through `--provider opencode --model opencode-go/deepseek-v4-pro`:

| Type | Accuracy |
|---|---|
| **Overall** | **0.800** |
| open-domain | 1.000 |
| temporal | 0.900 |
| multi-hop | 0.800 |
| single-hop | 0.500 |

**Read this as a smoke, not a claim:** n=40, self-judging model family (deepseek judging deepseek), and a stronger backbone than the gpt-4o-mini protocol behind Mem0's published 66.9% / Zep's contested 66–75%. A defensible headline needs the full 1,531-question run with a fixed judge protocol and disclosure of all three choices.


## Classifier fallback verification (2026-07-26)

> Historical record. The credit-exhaustion seam this section validates
> (`FallbackProvider`/`ErrCreditExhausted`, `internal/ai/provider_test.go`,
> `internal/ai/fallback_provider_test.go`) was removed in the harness-only
> memory-management change (2026-09-12). The fail-fast property lives on, but
> guards a missing CLI binary instead of exhausted API credit; reproduce at
> the CLI level by removing `claude`/`opencode`/`codex`/`goose` from PATH.

The headless CLI path (`ghost resolve`/`ghost supersede`, and the stop hook's
auto-resolve) cannot be driven into a real `ErrCreditExhausted` from outside
the process: `internal/ai.APIURL` is a compile-time constant, not a config
override, so there is no stub-server route into a live `go run ./cmd/ghost
resolve` invocation. The fail-fast behavior (Task 6) is therefore verified at
the unit level only, via `internal/ai/provider_test.go`'s
`parseAPIErrorFixtureCreditBalance` (exercises the real `parseAPIError` code
path against the actual Anthropic 400 response shape) and
`internal/ai/fallback_provider_test.go`'s
`TestFallbackProvider_NoSecondary_CreditExhaustionFailsFast` (confirms
`FallbackProvider` with a nil secondary returns `ErrCreditExhausted`
unchanged). This is a known gap, not a demonstrated end-to-end CLI run —
flagged here rather than silently treated as equivalent.

**Update (2026-07-26, later same day):** both paths above were exercised live
against the real `ghost` project database (43 memories):

- **CLI path** (`ghost resolve ghost` then `--apply`, primary
  `anthropicClient` provider): worked end-to-end — 21 kept after prefilter,
  13 confirmed evidence on dry-run, 12 actually stamped on `--apply` (one
  candidate re-classified out at write time by `SetResolved`'s own
  eligibility re-check, expected non-determinism across two separate classification
  calls, not a bug).
- **`ghost_resolve` MCP tool / sampling path**: connected live, tool
  correctly registered and reachable (`project`/`apply` args validated), but
  the classify call fails immediately with
  `mcp sampling: calling "sampling/createMessage": Method not found`. This
  Claude Code session's MCP client does not implement the `sampling/createMessage`
  capability, so the request never reaches a model — `SamplingProvider` never
  gets a response to classify. The sampling path is therefore **structurally
  verified** (wiring, args, error propagation all correct) but **not
  functionally verified** — no live client currently available on this
  machine implements MCP sampling to complete the test. Re-test once/if a
  connected MCP client adds sampling support.

## Session-injection budget (category-priority + compact render) — SHIPPED

`TestBenchInjectionBudget` (internal/mcpinit/injectionbudget_test.go) drives the
real `loadSessionContext`/`formatSessionContext` pipeline over a 57-memory
representative corpus skewed so the descriptive categories (14 architecture +
12 facts) would otherwise crowd the rank-only top-15 and starve the behavioral
slots. Measured under the default `injection.behavior_floor: 8`:

| metric | value |
|---|---|
| selected memories (cap) | 15 |
| behavioral hit (gotcha/convention/preference/decision) | **8/8** (floor 8 met) |
| compact render | **732 bytes** |
| legacy render (32-hex ID per line) | 852 bytes |
| byte saving | **120 bytes (~14%)** |

This evidences that the trade is a net win: the category-priority selection hits
its behavioral floor even when high-importance descriptive rows dominate the
raw decay ranking, while the compact render shrinks (never grows) the injected
block. Ran 2026-09-08 on the category-aware-injection worktree.

## Reporting rules (all phases)

1. Harness, datasets, and judge prompts live in this repo.
2. Fixed seeds, temperature 0; single-run results labeled as such.
3. Per-category tables with sample sizes; raw per-question logs attached to the release.
4. Token cost and latency reported next to accuracy.
5. Negative or mediocre results get published too.
