# Benchmarks and methodology

Ghost publishes benchmark results together with the harness, inputs, and limitations needed to reproduce them. The guiding rule is simple: **a score is useful only when someone can re-run the evaluation and understand what it measures**.

## At a glance

| Evaluation | What it measures | Headline result |
|---|---|---|
| LongMemEval-S retrieval | Judge-free retrieval against official evidence labels | Hybrid Recall@5 **93.0%**, Recall@10 **97.3%** on 470 answerable questions (measured pre-task-prefix — re-baseline pending, see Phase 1) |
| `ghost bench` | Deterministic in-repo retrieval regression suite | Hybrid NDCG@10 **0.818** on 220 queries and 551 memories || LongMemEval-S end-to-end | Retrieve → generate → judge with DeepSeek v4 Pro | **96.2%** blended accuracy across 500 questions (its hybrid retrieval leg is pre-task-prefix too — see Phase 4) |
| Staleness suite | Fresh-fact ranking without breaking older-but-correct facts | Fresh-wins **1.000**, fresh@1 **0.521** (0.583 state / 0.458 premise) — the top slot is the stale answer on about half the premise probes |
| Recency-trap suite | Old-but-correct memory against newer distractors | **0.929** in a never-decay category (invariant under decay, as claimed) and **0.417** in a decaying one — but **1.000** there when the correct memory is pinned |
| Ranking-state suite | Graded corpus carrying `created_at` spread and `supersedes` edges | Demote alone **1.000** R@1, decay alone **0.071**, shipped pair **0.214** against **0.571** with both off — the two paths do not compose, because the rows decay pushes down are the rows the demote promotes |
| Maintenance-state suite | Ranking over a corpus with resolved, shared and superseded rows | Hybrid live-wins **0.810** on 21 questions; the graded table cannot see this class of change at all |
| No-answer queries | What search returns when nothing in the corpus answers the query | Mean top cosine **0.584** vs **0.741** answerable; 51/220 answerable queries sit at or below the no-answer maximum |These rows are not one leaderboard. Retrieval metrics, end-to-end answer accuracy, a staleness fixture, a recency-trap fixture, a ranking-state fixture, a maintenance-state fixture and a false-positive count answer different questions. Competitor scores also use different generators and judges, so cross-system comparisons are directional unless the evaluation protocol is identical.

**Status:** LongMemEval-S retrieval, `ghost bench`, and the documented end-to-end run have shipped. The staleness, recency-trap, ranking-state, maintenance-state and no-answer suites are report-only in CI. The official GPT-4o leaderboard-comparable run has not been executed.

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

- **Hybrid fusion earns its keep.** Hybrid NDCG@10 (0.818) beats both single legs (FTS 0.749, vector 0.800) — the 70/30 RRF weighting is a net win on this dataset. `TestBenchRegressionFloors` asserts this relationship so a regression trips CI. Absolute numbers are lower than the v1 starter because v2 deliberately adds paraphrase queries where lexical overlap is weak (the FTS leg's R@1 falls to 0.467; vector and hybrid carry those).- **The graph-expansion bonus was evaluated and removed.** An additive link-graph bonus (former 0.15 default) lifted semantically-adjacent neighbors above exact matches, and a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth. The former `GraphWeight` setting and the bonus are now removed entirely (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`). The link graph is retained for the Obsidian mirror and `supersedes` ranking.
**What this table cannot see.** The v2 corpus is the *graded retrieval* dataset, and it is deliberately clean: every memory is created through `store.Create` in one batch, so all 551 share a `created_at` and the decay factor is identical across every candidate — inert, and pinned by `TestDecayDoesNotPerturbGradedBench`. It also holds no resolved row, no `_global` row and no `supersedes` edge. A ranking change that acts on any of that measures 0.000 on this table, which is exactly what happened when the resolved/`_global` demotion shipped: measured on one fixture, `f3a80f7` (pre-#634) and `main` both read 0.818 here. That is a property of the corpus, not a bug in the harness, so the coverage lives elsewhere: the [maintenance-state suite](#phase-3b--maintenance-state-suite-report-only) and the [no-answer queries](#no-answer-queries-the-abstention-baseline-report-only).
The v2 dataset overshoots the original ~150/~40 growth target (551/220) to give distractor density room for paraphrase grading. Regression tests assert **metric floors** (a little below observed), not exact rankings, since RRF scores can tie.
### Parameter sweep (`ghost bench --sweep`)

The RRF fusion is parameterized (`memory.SearchParams`), and `ghost bench --sweep` grid-searches the vector-leg weight (FTS = complement) — 6 combinations over the same dataset, one prepared store. Every point but the default also prints a **paired 95% interval against the default**, so the findings below are read off the tool's own output rather than asserted alongside it. This is one captured run of that command:

```
params                     R@1    R@10   MRR@10  NDCG@10  vs default (paired 95%)
vec=0.70                 0.520   0.763    0.902    0.818  this is the default  <- current default
vec=0.80                 0.520   0.763    0.903    0.818  -0.0004 [-0.0043, +0.0030]
vec=0.60                 0.519   0.763    0.901    0.816  -0.0018 [-0.0060, +0.0020]
vec=0.90                 0.511   0.771    0.894    0.814  -0.0045 [-0.0146, +0.0046]
vec=0.50                 0.509   0.751    0.890    0.809  -0.0097 [-0.0197, -0.0001]
vec=0.30                 0.494   0.737    0.875    0.790  -0.0279 [-0.0439, -0.0127]
```

- **Leg weights remain robust, and the default still wins.** On 220 queries, vec 0.70 (shipped default) tops the grid at NDCG 0.818, level with vec 0.80; 0.60/0.90 are within 0.004; vec 0.50 and vec 0.30 fall away, 0.30 sharply (0.790). The earlier v1 sweep's "0.3–0.7 flat" band does not fully carry over — the paraphrase-heavy queries reward a stronger vector leg — but there is still no evidence to move off 70/30. **The top four points are not separable, and the table should not be read as a ranking of them.** That is measured rather than asserted, and the table above is the measurement: the intervals for 0.80, 0.60 and 0.90 all contain zero and 0.30's excludes it outright, while 0.50's lower edge sits close enough to zero that the next bullet has to be read before it means anything. So the grid establishes a **shape**: a broad plateau of indistinguishable points at the top, one point at the plateau's edge (0.50), and one bad corner (0.30) — and nothing about which point inside the plateau is best. (These intervals are narrower than the cross-condition ones above because two grid points differ in one fusion weight only, so most per-query differences are exactly zero — which is the point: the plateau is flat, not merely close.)
- **One row of that table is not reproducible, and the report says so.** The store breaks tied fused scores by memory id (`internal/memory/vector.go`), and the benchmark seeds every id from `hex(randomblob(16))`, so a point whose two legs are weighted equally re-draws that tie-break on every run. `vec=0.50` is the affected point. Over 12 runs of one binary its NDCG@10 took three values (0.807, 0.809, 0.810) and its interval's lower edge eight times, of which **four intervals reached past zero** — so the row usually separates from the default and about a third of the time does not, which is a measurement that cannot be quoted to four decimals. The other five points are byte-identical across every run. That is [#708](https://github.com/wcatz/ghost/issues/708), and the fix (ids a function of the corpus) would move every published number in this file, so it is not folded into #561. Until it lands, read 0.50's row as a shape — and note that the report's own rule ("an interval containing 0.0 means the point is not separable") reads this row as separable, because −0.0001 is on the wrong side of zero. That is the contradiction, not a reason to believe the row: the next four runs are as likely not to be.
- **Outcome: the 70/30 leg weighting ships unchanged, and the graph bonus was removed.** With the leg weights robust across the upper half of the grid, there is no evidence to change the shipped 70/30 split. The graph-expansion bonus was removed rather than kept disabled (see the spec linked above); the link graph is still built for the Obsidian mirror and `supersedes`.

## Phase 3 — staleness suite (the flagship)

Deterministic scenarios for the failure users actually complain about: agents acting on superseded facts ("prod runs Postgres 14" retrieved after the migration to 16). Modeled on the MemTrace error taxonomy ([arXiv 2605.28732](https://arxiv.org/abs/2605.28732)) and STALE probe design ([arXiv 2605.06527](https://arxiv.org/abs/2605.06527)):

- Save fact v1; later save superseding v2; assert search ranks v2 above v1 (**fresh-wins rate**, **fresh@1**), including for queries that presuppose the outdated state, and across update chains (v1→v2→v3).
- Deletion regressions: reflection must never drop pinned or manual memories (codifies the existing empty-set guard and snapshot behavior).
- Runs in CI in seconds. No LLM judge.

This suite was designed to *fail* at first — production search had no decay signal in `SearchHybrid` ranking (decay lived only in `GetTopMemories`). At the pre-decay shipped default it reported **fresh-wins 0.083** (fresh-found 1.000 — the update is always retrieved, just out-ranked by its shorter, older original). With `DecayEnabled: true` (the current default, reorder-only) it reports **fresh-wins 1.000** at trap correct-wins 0.929 (`TestDecayFrontier`). It lands in CI as **report-only**; scenarios graduate to enforced assertions as the fix ships.

**`fresh-wins` is not the whole headline, and reading it alone overstates the result.** It says the newest version outranks its stale siblings *somewhere in the window*; an agent reads the top result, and the two are not the same number:

```text
probe            n  fresh-found   fresh-wins    fresh@1
state           24        1.000        1.000      0.583
premise         24        1.000        1.000      0.458
all             48        1.000        1.000      0.521
```

So on the 24 premise probes — the ones that presuppose the outdated state and are the failure users complain about — the stale version is the top result on 54% of them while every one of them is scored a win. The report carries all three columns and an all-probes row for exactly this reason (`TestStalenessReportNamesFreshAt1` pins the row's presence and that the aggregate is the arithmetic of the per-type rows). The size of the gap is not asserted: it is a statement about today's ranking, and freezing it would make the next ranking fix a test failure.

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

**And the never-decay exemption is also the limit of that claim, which is why the trap fixture now spans both classes (#561).** Every scenario in the suite used to be `fact`, so 0.929 was a property of a category decay never touches and said nothing about the categories it reorders. Scenarios now carry a category (defaulting to `fact`, so the original fourteen are unchanged) and the score is reported per category, with decay off and on:

```text
category               decays     n  wins(off)   wins(on)     delta     @1(on)     pinned(on)
architecture           yes        2      1.000      0.500    -0.500      0.500      1.000 (1)
decision               yes        2      0.500      0.500    +0.000      0.500      1.000 (1)
dependency             yes        3      1.000      0.333    -0.667      0.333      1.000 (1)
gotcha                 yes        3      0.667      0.333    -0.333      0.333      1.000 (1)
pattern                yes        2      1.000      0.500    -0.500      0.500      1.000 (1)
fact                   no        14      0.929      0.929    +0.000      0.929              -
decaying (pooled)      yes       12      0.833      0.417    -0.417      0.417      1.000 (5)
never-decay (pooled)   no        14      0.929      0.929    +0.000      0.929              -
all probes             mixed     26      0.885      0.692    -0.192      0.692      1.000 (5)
```

The never-decay row is unchanged and is now asserted invariant scenario by scenario — that invariance is the frontier's claim and it holds. The decaying rows are its cost: **an old-but-correct memory competing with a fresh distractor in a category decay actually reorders loses the contest, 0.833 → 0.417.** That is decay working as designed rather than a defect, so it is reported rather than gated, and it is the number a reader needs in order to weigh the default.

Two guarantees about those rows are asserted because production makes them. The old memory is still **retrieved** under decay — reordering never changes window membership, so what decay costs is a rank and not the answer. And a **pinned** one still takes the top slot against a fresh *unpinned* distractor in the same category: the `pinned(on)` column is 1.000 in every decaying category and over all five pinned probes, and it is a column of the report rather than a name list in a test because pinning is the only control a user has over a category that decays — if the ranker multiplied a pinned row down, that control would be decorative. The column is a ratio over the pinned probes *in that row*, so a row with no pinned probe prints no claim rather than a misleading 0.000. The two classes are seeded into separate projects so neither one's window depends on how many scenarios the other contributes; pooling them let adding these fixtures push a never-decay scenario's answer out of the top-10 window, and keeping them apart is also what makes the never-decay row the number the tables above quote.

**The real fix is targeted, and it clears the frontier.** `SearchParams.SupersedeDemote` (default true, alongside `DecayEnabled`) consumes directed `supersedes` links: within the result window it demotes a memory below every present memory that supersedes it (penalty = count of present superseders, stable-sorted — so update chains order correctly given star links, and it is a hard no-op when no supersedes edge joins two results). Because it only ever acts on genuine replacement pairs, it does what no blanket age-only prior could (`TestSupersedeDemoteClearsFrontier`):

```text
                        staleness fresh-wins   recency-trap correct-wins
both off                0.083                  0.929
decay on (DefaultSearchParams)          1.000                  0.929   ← decay alone flips staleness, trap untouched (fact never decays)
supersede demote on     1.000                  0.929   ← likewise, trap untouched (no supersession edge)
both on (shipped default)              1.000                  0.929
```

The trap is untouched in both cases: under decay its distractors are `fact` (never-decay, so decay never fires), and under demote they are *not* supersession pairs (no `supersedes` edge, so the demote never fires). That is the free lunch the blanket-recency frontier proved a global prior can't be. **The "trap untouched" in both rows is the never-decay half of the suite** — the same two rows measured before #561 added the decaying scenarios, whose score does move under decay. Every table on this page that reads a trap number off a frontier is scoped to that half on purpose, and the split is in the [table above](#phase-3--staleness-suite-the-flagship).

**Both halves now ship. Creation:** `ghost supersede <project> [--apply]` (`internal/supersede`) proposes newer→older candidate pairs from cosine-similar memories (tighter than the 0.70 'related' floor), confirms them with batched CLI-harness classify calls (up to 8 pairs per call), and writes star `supersedes` links (`source='llm'`). It is re-runnable and self-heals after reflection cascade-deletes links (`ReplaceNonManual` reinserts memories with new IDs — a re-run rebuilds the links, exactly as the cosine worker rebuilds 'related'). The cosine worker is rejected as the creator itself — symmetric similarity can't assign direction (the failure that got the graph bonus disabled), so similarity only *proposes* and the LLM *confirms + directs*. The classifier prompt is biased toward NO (a false supersedes buries a valid memory), and on a labeled set of 10 genuine-vs-parallel pairs plus the 4 real pairs [#641](https://github.com/wcatz/ghost/issues/641) was filed from, both the single-pair and batched paths are scored against one table (`TestRelationClassifierLive`, `TestRelationClassifierLiveBatch`; run manually against a CLI harness, skipped when none is installed). The maintenance benchmark behind #641 judged 4 of 7 links wrong on a real database — one backwards, two `causes` between status reports of one open issue, one event record superseded by a later unrelated event on the same host — so the prompt now also shows each note's `created_at` (the pass orients a pair by `updated_at`, which is wrong whenever a note was re-saved after the fact it reports), offers a fourth verdict `reversed`, forbids `causes` between two status reports of one issue, and states that an event record is never superseded by a later unrelated event on the same host. `reversed` is the one wrong answer code can refuse: a `supersedes` link only points newer→older, so the pass reports and drops it instead of writing it backwards, and never caches it. `internal/supersede/regression_test.go` holds the four pairs as a labeled regression set, anonymized (the shape of each pair is the signal; the text is not).

**Consumption has now graduated; creation stays opt-in.** `DefaultSearchParams` ships `DecayEnabled: true` and `SupersedeDemote: true`, so production `SearchHybrid` / `SearchHybridAll` (i.e. `ghost_memory_search` and `ghost_search_all`, including their FTS-only fallbacks) apply decay reordering and consume `supersedes` links. Creation of `supersedes` links remains opt-in — links only exist if the user ran `ghost supersede --apply` — so the demote is still a hard no-op for anyone who has not asked for it, and the numbers above are what back the flip: decay alone moves staleness fresh-wins 0.083 → 1.000 with recency-trap correct-wins unchanged at 0.929 (`TestDecayFrontier`); the demote does the same 0.083 → 1.000 with trap flat at 0.929 (`TestSupersedeDemoteClearsFrontier`). Either half alone clears the frontier, and both on keeps it cleared. It was flipped because an eval run found the opposite failure: a memory the user had explicitly marked as replaced still outranked its replacement in live search. `TestProductionSearchDemotesSuperseded` (internal/memory) guards the production entry points; `SearchHybridParams` still takes explicit params for the sweep harness.

Note what those two suites are *not* measuring: a version chain is hand-built state in both, and the trap suite's two halves are scored on whether one memory outranks another rather than on graded relevance over a corpus. The [ranking-state suite](#phase-3c--ranking-state-suite-report-only) is the graded version of the same question, and its numbers do not agree with the headline table's.

### Decay re-selection (evaluated, NOT default)

`SearchParams.DecayReselect` (default **false**) keeps the top `limit*2` by base score, then selects the top `limit` by `base × decay` — the fix for "a fresh memory ranked below the pure-base cut is never rescued." Probed via `TestDecayReselectProbe` (`GHOST_BENCH_PROBE=1`, which reports the trap suite as its two classes because one of them cannot move — see the note under the table):

```text
                  graded hybrid NDCG@10   staleness fresh-found/wins   trap never-decay   trap decaying
reselect=false    0.818                   1.000 / 1.000                0.929              0.417
reselect=true     0.818                   0.938 / 0.938                0.929              0.417
```

The trap fixture is reported as its two classes, and the split matters more than the numbers do. `decayRank` multiplies base by `DecayFactor(category, …)`, which is exactly 1.0 for every never-decay category — so on those scenarios `base × decay == base`, the second sort is a stable no-op, and taking the top `limit*2` by base before re-selecting the top `limit` by base returns the same set in the same order. **The never-decay half cannot move under this flag whatever the ranker does**, so that column is arithmetic rather than evidence; it is printed as the check that the flag is not silently reaching a corpus that cannot express the question. The decaying half is the one that *can* move, and it happens to be flat too (0.417 both ways) — a measurement this time, though a weak one on 12 probes.

So the verdict below rests on the two columns that are real: the graded NDCG is unchanged, and the staleness suite — the one with a seed of decayed, superseded versions to reorder — loses probes. A verdict quoted as "graded and trap stay flat" is resting one third on a tautology, which is worth saying rather than leaving the reader to find it.

**Verdict: not defaultable.** The wider base window **regresses staleness** — `default_branch` (both probes) and `vpn_solution` (state probe) lose the fresh version entirely — while the graded NDCG and both trap classes stay flat. Ship gate requires staleness not to regress; the flag stays off by default and is available for future experiments behind `SearchParams.DecayReselect`. Reorder-only membership (relevance owns the cut) remains the shipped behavior.

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
- **Category-exempt decay and resolve state pull against each other, and most of that pull was the missing demotion.** Shipped: live-wins 0.810 (NDCG 0.583). With decay off: 0.857 (NDCG 0.904) — a 0.047 live-wins gap, down from 0.429 before #634. The mechanism is visible question by question: a resolved `fact` never decays (factor 1.0) while a live `dependency` copy decays (0.33–0.75 at 10–80 days). Neither the staleness suite nor the recency-trap suite can see this, because in both the correct answer sits in a never-decay category — which is exactly the "old-but-correct memory in a decaying category" case the audit asked for, and the [ranking-state suite](#phase-3c--ranking-state-suite-report-only) below and the decaying half of the [recency-trap table](#phase-3--staleness-suite-the-flagship) are where that case is now measured. Not asserted, on purpose: it is a residual finding for the ranking work, and the day it is fixed this suite should show it.

## Phase 3c — ranking-state suite (report-only)

The [Phase 2 table](#phase-2--ghost-bench-an-in-repo-dataset--ci-regression-floors--shipped) cannot see either ranking path production runs. Its corpus seeds through `store.Create`, which stamps one `created_at` for every row, so the decay factor is identical across every candidate and cannot reorder them; and it holds no `supersedes` edge, so the demote is a hard no-op. A change to either path therefore measures **0.000** on the headline table — which is a property of the corpus, not evidence that the change did nothing, and it is how a shipped ranking change can look inert.

This suite is the same measurement with the state those two paths consume present. 31 memories in `internal/bench/testdata/withstate_memories.jsonl` whose `created_at` spans 4 to 700 days across both decay classes, and eight supersession chains written through `store.CreateLink` with the same relation and source `ghost supersede --apply` uses — 10 edges in all, of which one memory (the three-deep `k8s_ver` chain's live row) supersedes two others in a star. Its 14 questions are graded the way the headline set's are — one answer each, and the answer to a chain question is the LIVE row — and its vectors come from the same `nomic-embed-text:v1.5` with the same task prefixes, so the two tables are read next to each other rather than instead of each other. The superseded rows carry gain 0 and are the hard part: they are the same claim in older words, so a lexical search finds them first and a vector search finds them too.

The answer to a change that acts on maintenance state is the whole set of configurations, not one number, because two reordering passes can undo each other on the same row:

```text
configuration        condition        R@1     R@5   MRR@10  NDCG@10
reference (no params) fts-only       0.571   1.000    0.774    0.832
reference (no params) vector-only    0.571   1.000    0.786    0.842
both off             hybrid         0.571   1.000    0.786    0.842
decay only           hybrid         0.071   0.571    0.327    0.486
demote only          hybrid         1.000   1.000    1.000    1.000
both on (shipped)    hybrid         0.214   0.786    0.409    0.548
```

The aggregate table says how many; it cannot say which, and on this corpus "which" is the whole finding. `TestRankedStateSuiteIsNotInert` prints the per-probe answer rank for every configuration:

```text
answer rank by probe         both off   decay only  demote only both on (shipped)
q_db_sync_host                      2            4            1            4
q_k8s_version                       1            3            1            3
q_signer_topology                   2            3            1            3
q_metrics_backend                   2            2            1            1
q_alert_channel                     2            2            1            1
q_backup_window                     2            6            1            5
q_tls_source                        1            1            1            1
q_cardano_dir                       2            2            1            2
q_ogmios_port                       1            8            1            7
q_kv_store                          1            6            1            6
q_retry_budget                      1            4            1            4
q_log_scrape                        1            6            1            5
q_wire_protocol                     1            7            1            5
q_index_type                        1            7            1            7
```

Four things to read out of it, none of them comfortable and all of them the reason the suite exists:

- **The demote alone takes the suite to 1.000, losing nothing.** Every chain's live row is first and all six old live rows are first, so the targeted demote does the whole job on this corpus when decay is out of the way. What this does **not** show is the *intra-chain* order — another chain's rows outrank `k8s_ver_v2` and `k8s_ver_v1` out of the top four, so the star-link ordering is exercised but not observed here, and no claim about it is made below.
- **Decay alone is the expensive half: R@1 0.571 → 0.071, and it costs SEVEN probes, not six.** Six are the old live rows (`ogmios_port` at 500 days, `kv_store` at 640, `wire_protocol` at 700, `retry_budget` at 280, `log_scrape` at 150, `index_type` at 95 — all decaying categories, all demoted to ranks 4–8). The seventh is `k8s_ver_v3`, a **six-day-old** live row that decay pushes from rank 1 to rank 3. That last case is the more interesting of the two and is the one a summary of "old memories lose" hides: once the demote is off, even a fresh replacement of a stale fact loses the top slot, because the stale row it replaced is the only thing decay is not penalising.
- **The two paths do not compose, and the interaction is the opposite of additive.** Counting probes whose answer lost the top slot: decay alone demotes 10 relative to both-off, the demote alone demotes 0, and the **shipped pair demotes 11 relative to the demote alone**. The demote recovers none of them, and the reason is visible in the grid: the rows decay pushes down are the rows the demote promotes. The three chains whose live rows survive the shipped pair are exactly the three in never-decay categories (`metrics_backend`, `alert_channel`, `tls_source`); the five chains in decaying categories end at ranks 2–5.
- **So the shipped default on this corpus is worse than no ranking path at all**: R@1 0.214 against 0.571 with both off, and 0.071 to 1.000 for the two paths taken alone. That is a finding about a 14-query corpus and is reported, not gated — but it is the number the ranking work inherits, and it was invisible while the headline table read 0.818 in every configuration.

Every column here discounts by position, NDCG@10 included; R@1 and MRR@10 are read because they are the least discounted and move first, not because NDCG is blind to the reorder. `recall@10` is asserted identical across all four configurations — the findability half, and the check that stops "ranking improved" from meaning "decay rescued something out of the cut".

The comparison's other half is that the **headline** corpus is still the inert reference this suite is measured against, and that claim lives in `TestDecayDoesNotPerturbGradedBench` rather than here. It is asserted in the two forms the property actually has. **Decay is measured**: hybrid NDCG@10 and recall@10 are identical with `DecayEnabled` off and on. **The demote is structural**: no memory in the corpus declares a `supersedes` edge, and a row with no edge to consume is the demote's documented no-op (`demoteSuperseded`); what this suite's page cannot show is the demote *measured* on that corpus, because sweeping the demote toggle there would be two more passes over 547 memories and the toggle is `true` in both points the decay comparison sweeps. The "no edges, no effect" claim is measured once, on the recency-trap suite instead, where it costs a tenth of a second: `TestSupersedeDemoteClearsFrontier` runs it with the demote off as well as on and requires the two trap scores to be equal. So the corpus-side fact (no edges declared) and the ranking-side fact (no edges means no reorder) are both asserted, in the two places each is cheap to assert. A corpus that gains an age or an edge is reported by name and field, rather than quietly making this section's comparison unlike-for-like. The check sits where the corpus is already loaded for a cost reason as well: 547 memories with 768-dim vectors, loaded by six other tests, and a seventh load put the package over CI's 10-minute ceiling (it sits at 478s of it on `main`). The level itself is not re-asserted anywhere — that is `TestBenchRegressionFloors`' job. `TestRankedStateFixtureCarriesState` is the anti-vacuity guard — enough memories, at least eight distinct ages among decaying rows, at least six edges, **exactly one** three-deep chain (the one this section describes), and both classes of edge (one decay cannot move on its own, one with a wide age gap) — because a fixture that lost its state would score identically in all four rows and every delta above would be a difference between nothing and nothing.

Cost: ~0.2s (31 memories, 14 queries, four configurations, plus the per-probe grid). The headline corpus is not loaded or seeded here at all; the paragraph above says which test owns that and why. It is a test-only suite, not part of `ghost bench`; `go test ./internal/bench -run TestRankedStateSuiteIsNotInert -v`.

## No-answer queries: the abstention baseline (report-only)

Recall cannot see a leak. A wrong memory returned counts as a hit for whatever it displaced, so a system can score well while feeding an agent plausible garbage — which is what [#537's negative retrieval fixtures](https://github.com/wcatz/ghost/pull/537) pin at the unit level (a named memory must not surface, each with a reason). What they cannot do is measure a query nothing answers, because recall has no denominator and the runner used to skip such queries entirely. It no longer does: a query with an empty relevance map is **measured** as a false positive rather than dropped, per condition, so the rate sits under the NDCG numbers where it can be read next to them.

`testdata/negative_queries.jsonl` adds 24 of them to the graded corpus, with `rel` empty by construction, in two flavors: `off_domain` (no vocabulary overlap with the corpus — the floor) and `near_miss` (corpus vocabulary, unrecorded answer: "who is on call this weekend", "how do i restore a dropped postgres table by hand"). `ghost bench` prints them under the main table:

```text
no-answer queries (n=24, nothing in the corpus answers these; report-only, no gate)
  A FALSE POSITIVE is a result returned for a query with no answer, and the false-positive rate is
  the share of queries with at least one returned row above that cosine floor. search.min_similarity
  ships 0, which refuses nothing, so at the shipped setting the rate is 1.000 in every condition;
  the floors below are the graded reading, and they are the band an abstention rule would live in.

  condition       results    mean top  max top  rate @0.30  rate @0.40  rate @0.50
  fts-only           10.0       0.548    0.697       1.000       1.000       0.625
  vector-only        10.0       0.584    0.697       1.000       1.000       0.875
  hybrid             10.0       0.584    0.697       1.000       1.000       0.875
```

**Read the rate columns as the finding they are.** `search.min_similarity` ships 0, so at the shipped setting Ghost never abstains: the false-positive rate is **1.000 in every condition**, and the only thing the number says is that the system returns ten rows for a question it cannot answer. The floors are therefore the graded part, and they say the two legs fail differently: at a 0.50 cosine the **vector leg leaks 0.875** of the no-answer set against the keyword leg's **0.625**, and the shipped hybrid path follows the vector leg exactly (0.875, mean top 0.584 against the keyword leg's 0.548). Fusion did not fix the leak and did not add to it — it inherited the vector leg's. A reader who had only the single hybrid number would not have known which leg was responsible, which is why the measurement is per condition.

**Every score here is the row's own cosine, read from its stored vector** — not a lookup in the vector leg's fetched list. A hybrid result can arrive on the keyword leg alone (the keyword reservation guarantees the top keyword hits a place in the window whatever their cosine), and reading such a row's score from a truncated list yields 0, which counts it below every floor: the floor rows were undercounting precisely the results that came from the leg with no score of its own. The cosine is also the only calibrated number in the pipeline — an RRF score is a function of a row's rank rather than of its match, and the keyword-only fallback's synthesized `1/(K+rank+1)` is a position, not a confidence.

The deeper block, still on the shipped hybrid path and still read from the runner's own measurement of it (one search per query, not one per report), names the condition it describes:

```text
abstention baseline (hybrid path, the shipped ranking)
  results returned per query   10.0 (window 10, no similarity floor configured)
  mean top cosine             0.584  vs 0.741 for the 220 answerable queries
  floor refusing all of them   0.697 (the no-answer maximum) costs 51/220 answerable queries

  flavor          n    results/query     mean top
  near_miss      12             10.0        0.623
  off_domain     12             10.0        0.545

  floor     results/query   queries w/ hit     rate
  0.30              10.00         24/24       1.000
  0.40               9.79         24/24       1.000
  0.50               6.83         21/24       0.875
```

Two conventions, stated because both change what the numbers mean:

- **The score of a query is the best cosine among the rows production returns for it**, and the no-answer set and the answerable contrast are both measured that way, so the two distributions differ only in whether the corpus has an answer. Those two can genuinely diverge from each other — a demoted row can be the corpus's strongest match and sit outside the window.
- **"Above a floor" means strictly above**, which is the rule production itself applies (`memory.filterVectorFloor` keeps a candidate when `score > floor`). So a floor set at the no-answer maximum already refuses the answerable queries whose best score ties it, which is why that count includes ties.

The floor rows are a sweep, not a proposal — `search.min_similarity` ships 0, so there is no configured floor to inherit, and the band is where one would have to live. They are also a stricter reading than the shipped flag, which applies a floor to the vector leg *before* fusion and therefore never touches a keyword-only result: the rows answer "how strong are the results a caller actually receives", not "what would the flag do". The flavors are reported apart because a pooled mean would let the easy half carry the hard one: a near-miss that reuses corpus vocabulary scores 0.623 against the off-domain floor's 0.545, and it is the 0.623 any abstain rule has to clear.

**What this suite asserts, and what it does not.** Nothing here gates the ranking. The enforced claims are about the *fixture and the report plumbing*: both flavors are present and their counts survive into the report, the searches returned something (otherwise the mean is a vacuous 0), one row per configured floor exists, the counts add up, and the maximum is not below the mean drawn from it. The near-miss flavor must not score *below* the off-domain one — that is a statement about the fixture being labelled correctly, not about the ranking. Notably **absent**: any assertion that the two distributions are separated. That is a claim about today's ranking, and the plausible abstention fix this baseline exists for — returning fewer, more similar rows for a query nothing answers — would move the no-answer mean up and trip a test whose job is to watch that fix land. The separation is reported, not asserted.

Read the third line as the actual baseline for the abstention work: **the two distributions overlap.** A floor of 0.697 would refuse all 24 no-answer queries and would also refuse 51 of the 220 answerable ones, so a threshold alone cannot abstain — the near-miss flavor is what makes the overlap visible, and it is why the answer is likely to be a calibrated decision rather than a constant.

### The keyword arm, measured (#580, PR 4)

`ghost_memory_search` now returns a verdict (`answerable` / `weak` / `empty`) rather than a list that is either long or literally empty. Two arms decide the `weak` case, and only one of them is a keyword rank (a returned row within the top 4 FTS ranks clears it); the vector arm ships **off** because the paragraph above is the reason.

Measured over the built-in fixture with the arm-A rule applied to what `Store.Candidates` returns for each query:

| Query set | n | best keyword rank | `weak` or `empty` |
|---|---:|---|---:|
| answerable (graded) | 220 | 0 for all 220 | 0 |
| no-answer | 24 | 0 for all 24 | 0 |

So the keyword arm is a strict **no-op on this corpus**: it abstains on none of the 24 no-answer queries, and it flags none of the 220 answerable ones as weak. (The measurement is a one-off run over `Store.Candidates`, not a shipped path — see the cost note at the end of this section.) That is the property that makes it safe to ship on by default — a clear keyword hit is never withheld — and it is also why it buys nothing here. Every fixture query, answerable or not, has *some* keyword hit at rank 0, so the floor cannot separate the two sets; only a calibrated decision over the vector leg can, and that is arm B, which stays off until it has a measured threshold. `context.abstain_cosine` is how a user sets one.

The same run confirms the ranked metrics are untouched. `ghost bench` output on `origin/main` (`a34e07a8`) and on this branch is byte-identical:

```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
hybrid             0.520   0.712   0.763    0.902    0.818
```

`ghost bench` does not route through `assemble.Run` — by design (Decision 5 of the assembler spec, so the existing hybrid floor keeps meaning what it means), which is why the delta is exactly 0.000 on NDCG@10 and R@5 rather than merely inside the 0.005 gate.

Cost: the runner now searches each of the 244 queries (24 no-answer plus the 220 answerable contrast) under **all three conditions** and scores every returned row from its own vector, which takes `ghost bench` to about **6.7s** on a laptop-class machine — less than the ~9s the hybrid-only version cost, because the deep report reads the runner's hybrid measurement instead of searching the no-answer set a second time. Scoring the window rather than a short list is the price; a future change that wants the same numbers faster has to make the production search hand back its legs (`searchHybridLegs` already does, for explain mode).

Cost of the arm-A table in this section (a one-off, not shipped): producing it ran `Store.Candidates` for all 244 queries and read each candidate's keyword rank, which is repository code but not a path any command takes. `ghost bench` itself is unaffected by #580 — it still calls `store.SearchHybrid`/`SearchFTS`/`SearchVector` directly, which is the asymmetry Decision 5 of the assembler design specifies so the existing hybrid floors keep meaning what they mean.

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
