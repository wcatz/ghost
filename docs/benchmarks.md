# Benchmarks and methodology

Ghost publishes benchmark results together with the harness, inputs, and limitations needed to reproduce them. The guiding rule is simple: **a score is useful only when someone can re-run the evaluation and understand what it measures**.

## At a glance

| Evaluation | What it measures | Headline result |
|---|---|---|
| LongMemEval-S retrieval | Judge-free retrieval against official evidence labels | Hybrid Recall@5 **93.0%**, Recall@10 **97.3%** on 470 answerable questions (measured pre-task-prefix — re-baseline pending, see Phase 1) |
| `ghost bench` | Deterministic in-repo retrieval regression suite | Hybrid NDCG@10 **0.818** on 220 queries and 551 memories; paired 95% CI over `vector-only` **+0.018** [+0.003, +0.034] |
| `ghost bench --context` | The **block** a caller receives, not its order | Context precision **0.138** (304/2200 rows); the item cap shortened **220/220** queries and dropped **8% of the graded rows it reached**; contamination **0.000** — a fact about this corpus, which holds no contaminable row |
| `ghost bench --passive` | The **passive** blocks — session start, `ghost context`, `ghost_project_context`, the project resource — over a synthetic four-project store that holds resolved, expired, not-yet-valid, out-of-scope, superseded and near-duplicate rows | Withheld leakage **0.000** on every surface (and non-zero when a filter is disabled, which the test checks); expected-row recall **1.000** on every surface; the session-start count lines **PASS** the honesty check (8 of 8, after #897) |
| LongMemEval-S end-to-end | Retrieve → generate → judge with DeepSeek v4 Pro | **96.2%** blended accuracy across 500 questions (its hybrid retrieval leg is pre-task-prefix too — see Phase 4) |
| Staleness suite | Fresh-fact ranking without breaking older-but-correct facts | Fresh-wins **1.000**, fresh@1 **0.521** (0.583 state / 0.458 premise) — the top slot is the stale answer on about half the premise probes |
| Recency-trap suite | Old-but-correct memory against newer distractors | **0.929** in a never-decay category (invariant under decay, as claimed) and **0.417** in a decaying one — but **1.000** there when the correct memory is pinned |
| Ranking-state suite | Graded corpus carrying `created_at` spread and `supersedes` edges | Demote alone **1.000** R@1, decay alone **0.071**, shipped pair **0.214** against **0.571** with both off — the two paths do not compose, because the rows decay pushes down are the rows the demote promotes |
| Maintenance-state suite | Ranking over a corpus with resolved, shared and superseded rows | Hybrid live-wins **0.810** on 21 questions; the graded table cannot see this class of change at all |
| No-answer queries | What search returns when nothing in the corpus answers the query | False-positive rate **1.000** in every condition at the shipped `search.min_similarity: 0` — and still **0.875** for the shipped hybrid path at a 0.50 cosine, against the keyword leg's **0.625**; mean top cosine **0.584** vs **0.741** answerable, and 51/220 answerable queries sit at or below the no-answer maximum |
| Storyline eval (`eval/storyline`) | Whether a **reversal** recorded mid-stream reaches later sessions marked as old | **9/10** on the one shipped arc (local run, `opencode-go/glm-5.3-flash`); the store held the reversal correctly and the failing check is that the session-start block did not mark the stale half |

These rows are not one leaderboard. Retrieval metrics, end-to-end answer accuracy, a staleness fixture, a recency-trap fixture, a ranking-state fixture, a maintenance-state fixture and a false-positive count answer different questions. Competitor scores also use different generators and judges, so cross-system comparisons are directional unless the evaluation protocol is identical.

**Status:** LongMemEval-S retrieval, `ghost bench`, and the documented end-to-end run have shipped. The staleness, recency-trap, ranking-state, maintenance-state, no-answer and context-assembly suites are report-only in CI. The storyline eval is local-only and gates nothing (the CI wiring is #338, not done). The official GPT-4o leaderboard-comparable run has not been executed.

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
- **Warming the hybrid cache across CI dispatches (#771):** the cold pass is ~12h of CPU-bound embedding, longer than any job cap, and a run left to the cap stores nothing. In the cancelled run 36501751853 the save step started immediately after the embed step was cancelled — the two log lines are 6 ms apart — and then failed about 6 s in: the cancelled step's `longmemeval` child was never reaped, so it kept appending to the cache file while `actions/cache/save` archived it, `tar` refused the moving target (`file changed as we read it`), and the action downgraded that to a **warning** — so nothing was stored while the step still reported `success`. The harness therefore takes `--embed-deadline`: past that budget of wall clock spent in embed calls, the pass stops at a batch boundary with everything it computed already appended to the cache file, prints `cache warmed N/M vectors … re-dispatch` **instead of** the metrics table, and exits `3`, a status distinct from the floor-violation `1`. Exiting on its own, well inside the cap, is what both completes the bench step and leaves the file quiescent enough for `tar` to archive it; the next dispatch then restores the partial cache and embeds the remainder. The budget counts embed time only, so a dispatch whose restored cache is already complete is never cut off while scoring and does reach a full result. A partial run is never a benchmark result: it prints no metrics and no floor verdict, so it cannot be mistaken for a regression or used to re-derive the floors. **Reading a hybrid job's log:** if the pass stopped at its budget, `Save embedding cache` runs and its step should read `success` — but that alone does not prove the entry exists, so check for a `Failed to save` warning in that step's log. A `Save embedding cache` step rendered **`skipped`** means the `grew` gate was false, which has more than one cause, so read the bench step's own last line rather than assuming:

- `embed cache grew: A -> B` — the cache grew, so the gate should have been true. A `skipped` save step here means the bench step was killed before the runner processed its `$GITHUB_OUTPUT` (the `grew` write comes immediately before this echo, and a step rendered `skipped` produces no log of its own, so read the JOB log).
- `embed cache unchanged at N bytes` — the cache did not grow, which is the case for a fully warm pass, and that line alone does not tell you which of three it was. `FLOOR VIOLATION:` is the only discriminator, because it is printed solely from the complete-pass branch:
  - a green bench step, no violation line: a routine warm dispatch — every question scored, floors met, nothing new to store;
  - a `FLOOR VIOLATION:` line: a **complete** measurement that failed the bar. Nothing was embedded, but a real number exists and the floors ARE telling you something. While the floors are unrebaselined this is the expected shape of most dispatches, so check for the line before concluding a run measured nothing. Do not substitute the shell's `::error::bench/longmemeval failed with exit 1` here — it prints for every non-zero status, including an Ollama or dataset failure, so it says nothing about whether a number exists;
  - a red bench step with no violation line: it failed before scoring anything (an Ollama embed error, a missing dataset, a bad flag).
- no size line at all — the bench step never reached the point of recording one: the job hit its cap, or an earlier step (the model pull, the build) failed. Nothing was stored; the next dispatch resumes from the previous snapshot.
- **CI gating:** only the **fts** floor (`R@5 ≥ 0.74`, `NDCG@10 ≥ 0.72`) is enforced automatically on PRs — it needs no Ollama and finishes fast. The **hybrid** floor (`R@5 ≥ 0.91`, `NDCG@10 ≥ 0.89`) is run **manually** (`workflow_dispatch`) or locally, not on a schedule: the cold embedding pass is CPU-bound (the ~12h above), too slow for any CI cap. Because `nomic-embed-text:v1.5` is deterministic, a cold run computes the same vectors as a warm one, so the manual gate is justified by the warm local numbers here without CI re-deriving them — **but those numbers were measured before the harness adopted the task prefixes, and that space has since changed, so until the hybrid run below is re-baselined the manual hybrid gate is a floor-check, not a valid regression signal**: it can tell you a prefixed run is below a bar, not that this change made it worse (see the re-baseline note below). **Re-baseline pending:** the results table, the per-class claims above and the committed per-question logs predate the harness adopting the `search_document: `/`search_query: ` prefixes (the vector space the published numbers were measured in can no longer be reproduced by this harness), so a prefixed hybrid run measures a different space than the one that set these floors — re-run it to re-baseline the hybrid numbers, then restore the gate's regression meaning. **How the next dispatches get there (#771):** re-dispatch the manual `hybrid` job until one reports a complete pass (exit 0 — the partial exits print `cache warmed N/M vectors`, so N/M approaching M and a final exit 0 is the signal). That complete run is the one whose OVERALL row is a prefixed-space measurement; take R@5 and NDCG@10 from it, apply the same ~0.01–0.02 margin as the fts floor, and record the run id in the results table above. Until that run exists, the hybrid floors below the job are floor-CHECKED, not validated, and no dispatch's exit status is evidence of a regression.

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

- **Hybrid fusion earns its keep — and the gate behind that claim is deliberately weaker than the claim.** Hybrid NDCG@10 (0.818) beats both single legs (FTS 0.749, vector 0.800) — the 70/30 RRF weighting is a net win on this dataset. **The margin over the vector leg is 0.018 and the paired 95% percentile-bootstrap interval over that margin is [+0.003, +0.034]** (20 000 resamples, fixed PCG seed — and `ghost bench` prints both rows verbatim in its "Fused vs one leg at a time" table, so this interval is a number the command produces rather than one only a test logs), so the win is real and thin: under a query's worth of margin at the interval's lower edge. The win over FTS is 0.069 [+0.047, +0.092] and not in question. `TestBenchRegressionFloors` no longer compares the two means — the old `hybrid >= vector` on the 0.018 point estimate failed the build on a 0.001 dataset edit while the evidence said nothing had changed. What it gates instead is that **fusion is not materially worse**: the lower edge of the interval must clear **−0.02**, which is 4.4 queries' worth of NDCG at n = 220. Two things follow, and both are worth stating plainly. The tolerance is **larger than the effect it protects**, so the gate cannot fire while fusion's 0.018 advantage reverses by less than 4.4 queries; and "earns its keep" is therefore a claim the data makes (the interval excludes zero) rather than one the gate enforces. The gate is one-sided for its own reason: fusion is a robustness play, and a gate demanding it win on every corpus would be a gate on the dataset rather than on the architecture. Absolute numbers are lower than the v1 starter because v2 deliberately adds paraphrase queries where lexical overlap is weak (the FTS leg's R@1 falls to 0.467; vector and hybrid carry those).
- **The graph-expansion bonus was evaluated and removed.** An additive link-graph bonus (former 0.15 default) lifted semantically-adjacent neighbors above exact matches, and a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth. The former `GraphWeight` setting and the bonus are now removed entirely (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`). The link graph is retained for the Obsidian mirror and `supersedes` ranking.

**What this table cannot see.** The v2 corpus is the *graded retrieval* dataset, and it is deliberately clean: every memory is seeded in one pass under the same `created_at`, so the decay factor is identical across every candidate — inert, and pinned by `TestDecayDoesNotPerturbGradedBench`. That sameness is now written down rather than incidental: `Seed` stamps the whole corpus itself (`corpusStamp`), because `store.Create` stamped each row with its own `datetime('now')` and a seed loop that straddled a second boundary gave two tied rows different ages, which reordered them — the second half of [#708](https://github.com/wcatz/ghost/issues/708), found by measurement after the id half was fixed. It also holds no resolved row, no `_global` row and no `supersedes` edge. A ranking change that acts on any of that measures 0.000 on this table, which is exactly what happened when the resolved/`_global` demotion shipped: measured on one fixture, `f3a80f7` (pre-#634) and `main` both read 0.818 here. That is a property of the corpus, not a bug in the harness, so the coverage lives elsewhere: the [maintenance-state suite](#phase-3b--maintenance-state-suite-report-only) and the [no-answer queries](#no-answer-queries-the-abstention-baseline-report-only).
The v2 dataset overshoots the original ~150/~40 growth target (551/220) to give distractor density room for paraphrase grading. Regression tests assert **metric floors** (a little below observed), not exact rankings, since RRF scores can tie.
### Parameter sweep (`ghost bench --sweep`)

The RRF fusion is parameterized (`memory.SearchParams`), and `ghost bench --sweep` grid-searches the vector-leg weight (FTS = complement) — 6 combinations over the same dataset, one prepared store. Every point but the default also prints a **paired 95% interval against the default**, so the findings below are read off the tool's own output rather than asserted alongside it. This is one captured run of that command:

```
params                     R@1    R@10   MRR@10  NDCG@10  vs default (paired 95%)
vec=0.70                 0.520   0.763    0.902    0.818  this is the default  <- current default
vec=0.80                 0.518   0.763    0.900    0.817  -0.0014 [-0.0059, +0.0024]
vec=0.60                 0.519   0.763    0.901    0.816  -0.0019 [-0.0061, +0.0019]
vec=0.90                 0.509   0.771    0.891    0.812  -0.0056 [-0.0160, +0.0038]
vec=0.50                 0.509   0.755    0.890    0.809  -0.0087 [-0.0184, +0.0008]
vec=0.30                 0.494   0.737    0.875    0.790  -0.0280 [-0.0440, -0.0128]
```

- **Leg weights remain robust, and the default still wins.** On 220 queries, vec 0.70 (shipped default) tops the grid at NDCG 0.818, with vec 0.80 at 0.817 and vec 0.60 at 0.816 — 0.002 and 0.006 below, all three inside the intervals below; vec 0.50 and vec 0.30 fall away, 0.30 sharply (0.790). The earlier v1 sweep's "0.3–0.7 flat" band does not fully carry over — the paraphrase-heavy queries reward a stronger vector leg — but there is still no evidence to move off 70/30. **The top five points are not separable, and the table should not be read as a ranking of them.** That is measured rather than asserted, and the table above is the measurement: the intervals for 0.80, 0.60, 0.90 and 0.50 all contain zero, and 0.30's excludes it outright. So the grid establishes a **shape**: a broad plateau of indistinguishable points from 0.50 to 0.90, one bad corner (0.30), and nothing about which point inside the plateau is best. (These intervals are narrower than the cross-condition ones above because two grid points differ in one fusion weight only, so most per-query differences are exactly zero — which is the point: the plateau is flat, not merely close.) `vec=0.50` is the one row that changed when the sweep became reproducible, and it changed a conclusion rather than a digit: at −0.0100 [−0.0194, −0.0012] it read as separable from the default, and at −0.0087 [−0.0184, +0.0008] it does not — so 0.50 belongs inside the plateau, not at its edge.
- **The sweep reproduces, and the reason is the tie-break.** A grid point weighting its two legs EQUALLY is where RRF scores collide, and the store resolves a collision with two inputs that were being redrawn on every run: it breaks tied fused scores **by memory id**, and it re-sorts the window by a **decay factor** built from `created_at`. The benchmark used to seed every row through `store.Create`, so the id came from the column's `hex(randomblob(16))` default and `created_at` came from `datetime('now')` per row. `vec=0.50`'s NDCG@10 therefore took four values (0.807, 0.808, 0.809, 0.810) over ten runs of one binary, and its paired interval crossed zero, while the other five points — which barely tie — were byte-identical every time ([#708](https://github.com/wcatz/ghost/issues/708)). Both inputs are now a function of the fixture: a seeded row is stored under `bench:<project>:<key>` (`corpusID`) and carries one stamp per seeding pass (`corpusStamp`), so a tie resolves by the dataset's own key order and a tied pair of different categories is ordered by their categories rather than by which side of a second boundary they landed. Nothing in the ranking changed: `store.Create` still mints its own ids, `ghost bench`'s three-abiations table is **byte-identical before and after** (the ablations never tie, so a derived id cannot move them), and the five unaffected sweep rows are unchanged to the digit. The table above is five runs of one binary, byte-identical, and the report now says so in its own footer rather than telling the reader to discount a row.

- **Outcome: the 70/30 leg weighting ships unchanged, and the graph bonus was removed.** With the leg weights robust across the upper half of the grid, there is no evidence to change the shipped 70/30 split. The graph-expansion bonus was removed rather than kept disabled (see the spec linked above); the link graph is still built for the Obsidian mirror and `supersedes`.

## Context assembly (`ghost bench --context`)

Every other table in this file asks **which row came first**. Recall@1, MRR@10 and NDCG@10 are statements about ORDER, and they are invariant under the decision that actually costs a caller money: NDCG@10 gives the tenth row the same credit as the first, so a block carrying ten rows where two would have answered scores *identically* to the two-row block while costing five times the tokens. Nothing in a ranking metric can see a row that should never have been in the block at all, because ordering presumes the set is right.

`ghost bench --context` measures the block. It assembles one context block per graded query through the same path `ghost_memory_search` takes — `Store.Candidates` → `internal/assemble.Run` — at that tool's own budget (**10 items, 16000 response bytes** = `2 × memory.MaxContentLen`, `CondHybrid`), because a context metric measured against any other budget is a metric about a surface nobody ships. It prints this section and returns, so the ordering tables it is read against are **not above it** — they are what plain `ghost bench` prints. Here is one captured run:

```text
context assembly (ghost_memory_search's own block: 10 items / 16000 bytes; report-only, no gate)
  project bench, 220 queries measured, 220 answered, 2200 admitted rows

  A CONTAMINATED row is one internal/assemble classifies as one it should not have carried
  (Result.Leaks, from the verdicts its own stages recorded). A metric that re-read the corpus
  instead would be a second implementation of rules the pipeline already applies, and free to
  drift from them until a leak was reported as clean. These numbers are about the BLOCK a
  caller receives. NDCG@10 and MRR@10 are the ordering numbers -- they are in the table plain
  `ghost bench` prints, not this one -- and neither can see any of it.

  metric                                value  population
  result rate                 1.000 (220/220)  queries that admitted at least one row
  context precision          0.138 (304/2200)  graded-relevant of the admitted rows
  contamination                0.000 (0/2200)  admitted rows the assembler flags

  contamination by arm         rows  what the arm is
  resolved                        0  resolved_at is set: retired evidence the ranking demotes rather than drops
  expired                         0  its validity window has closed
  not_yet_valid                   0  its validity window has not opened yet
  out_of_scope                    0  it names a different place than the request asked for
  other_project                   0  it sits in a bucket the request did not name
  a zero here is a reading of THIS corpus: the graded one holds no resolved, out-of-window or
  cross-bucket row, so there is nothing for an arm to catch. docs/benchmarks.md says which.

  budget adherence                      value  population
  largest response                 2411 bytes  cap 16000 bytes, 0 responses over it
  largest block, tokens            336 (est.)  bytes/4 rounded up per row; an estimate, there is no tokenizer here
  item cap trimmed          0.499 (2191/4391)  of the rows that reached it; 1.000 (220/220) of answered queries, 0.082 (27/331) of the graded ones among them
  response fit trimmed         0.000 (0/2200)  of the rows that reached it; 0.000 (0/220) of answered queries, 0.000 (0/304) of the graded ones among them

  cost per answered query               value  population
  estimated tokens         296.609 (65254/220)  mean over answered queries; bytes/4 per row, an estimate, no tokenizer here
  rendered bytes           2232.964 (491252/220) bytes  mean over answered queries; the complete response, framing and verdict line included

  admitted rows by bucket      rows            share  queries
  bench                        2200 1.000 (2200/2200)      220

  dominant bucket, mean rows per answered query  10.000 (2200/220)
```

**What each metric is, and which of them a ranking metric could have told you.**

| Metric | What it answers | Why the ranking tables cannot |
|---|---|---|
| **context precision** | Of the rows a caller receives, how many does the corpus grade relevant? | Not recall. Recall asks whether a relevant row was found; this asks how much of what *was* found was worth carrying. |
| **contamination** | Of those rows, how many does `internal/assemble` classify as ones it should not have carried? | A ranking metric only orders a set it was given. A retired, out-of-window or wrong-project row ranks perfectly well; only the block's *composition* can be wrong. |
| **result rate** | Of the queries, how many returned a block at all? | Every other ratio here has "admitted rows" or "answered queries" under it, so a system that returned nothing would post perfect precision and perfect cleanliness. Measured, not excluded. |
| **budget adherence** | Did the block the tool shipped fit the budget the tool sent — and what did the cap cost? | NDCG is computed over a truncated window, so the truncation is invisible by construction. |
| **diversity** | How is the block spread across buckets — the request's project, or `_global`? | Depends on the set, not the order. |
| **token cost** | What did one answered query's block cost, in bytes and estimated tokens — as a mean and as a maximum? | A metric about how much context to *spend*, which is only meaningful once the set is fixed. Printed as two numbers because a block's cost and its worst case are different budgets, and a caller who sizes from the mean alone under-reserves. |

Three findings, all from the table above:

- **The budget is the binding constraint on every query, and it costs graded-relevant rows.** The item cap shortened **220 of 220** answered queries and cut **2191 of the 4391 rows that reached it** (`0.499`). Of the **331 graded-relevant rows that reached the cap, 27 were cut** — `0.082`, or 8% of the relevant evidence the budget was offered. The same 27 are 1.2% of the 2191 rows it cut, so the cap is overwhelmingly discarding low-relevance rows, which is what a bottom-of-the-ranking trim should do; the 8% is the part that is not, and it is a direct argument about `limit`: the shipped 10 is not a neutral default, it is a policy that discards 8% of what it found. A caller who needs the rest asks for it and pays for it in tokens. **Two things about the 331, both of which a reader is entitled to.** It is the graded population that *reached the budget*, not every graded row in the corpus: a row stage 2 or stage 3 dropped never entered a block, so this report never scored it and its relevance is unmeasured — the figure is the budget's cost among the rows the budget had a choice about. And the denominator is the graded rows, not the admitted ones: "relevant rows cut, over the rows the caller received" divides two different populations and prints 0.012, a smaller and quieter number that means nothing.
- **A block costs 296.6 estimated tokens and 2233 bytes, and the worst one cost 336 and 2411.** The mean sits close under the maximum because the item cap binds on every query, so most blocks are near-full rather than short — a caller sizing a context budget from the mean alone would under-reserve by about 12%, which is why the report prints both. The byte figure is the **complete rendered response**, framing and verdict line included, because that is what a caller receives; the token figure is the assembler's own bytes/4 estimate and there is no tokenizer in the pipeline, so it is an estimate everywhere it appears. Both are means over **answered** queries, so a query that returned nothing costs nothing here — true of the bill, false of the outcome, which is why the result rate is printed beside them.
- **The byte cap never binds, and the item cap always does.** The largest rendered response was 2411 bytes against a 16000-byte cap — 15% — so the response-fit pass fired on no query: it ran on all 220 and dropped nothing, which is `0.000 (0/2200)` rather than `n/a`, because it *was* measured. This is not a coincidence but a consequence: a larger `limit` makes the *byte* cap harder to hit, not easier, so the two bounds are not interchangeable and a report that folded them into one "budget" number would advise raising the limit that raises the problem. They are counted separately, and with their own populations, for that reason: a row the fit pass drops has already passed the item cap, so the cap saw every row and the fit pass only the ones the cap left. The cap's 4391 and the fit pass's 2200 are different numbers about the same run, not a contradiction.
- **A trim ratio is only meaningful if it can fall.** All three budget ratios are fractions of a stated population, and the query ratio's is **every answered query** rather than the trimmed ones — otherwise it reads `1.000` on any corpus where the cap binds everywhere *and* on any corpus where the budget had been removed entirely, and the report cannot tell a budget that binds from one that was deleted. The graded corpus cannot demonstrate that (220/220 is true there), so it is pinned on a 12-row fixture whose two queries straddle the cap: one reaches all twelve and is trimmed, one reaches three and is not, and the ratio must read `0.500 (1/2)`.
- **This corpus cannot measure contamination, and the report says so rather than printing 0.000 as if it could.** The graded corpus holds no resolved row, no out-of-window row, no scope contradiction and no `_global` row (see [what this table cannot see](#phase-2--ghost-bench-an-in-repo-dataset--ci-regression-floors--shipped)), and the only contamination arm reachable through a real `assemble.Run` is `resolved` — stage 2 drops an out-of-window row and stage 3 drops a scope contradiction *before* either can be admitted. So all five arms read 0 and the 0.000 is a **property of the corpus, not evidence the filters work**. A gate on it would be a gate on a tautology, which is why this section is report-only. The measurement is carried by an 8-row in-test fixture (`contextFixture`, `internal/bench/context_test.go`) that holds a `resolved` row, an expired row, a not-yet-valid row and a `_global` row, and asserts the assembler withholds the first two and flags the third.

**Contamination is classified by `internal/assemble`, not by the bench.** `assemble.Result.Leaks()` (`internal/assemble/leak.go`) reports a leak by reading the verdicts the assembler's own stages recorded in its trace — `ValidityState`, `ScopeMatched`, `ProjectMatch`, and the row's `resolved_at`. A bench-local predicate would be a *second* implementation of rules the pipeline already applies, free to drift from them until a leak was reported as clean by the one component whose entire job is to say otherwise. The bench refuses rather than guesses in the other direction too: a result with no trace, or an admitted row the trace never reached, is a measurement whose inputs are missing, and `measureQuery` errors rather than reporting a clean zero for it.

**Determinism.** The context report is measured at a **fixed instant** (`bench.ContextInstant()`, 2026-06-01T12:00:00Z) rather than the wall clock, so two runs of one binary print byte-identical output — verified in the PR. The ablation tables deliberately keep their wall-clock seed: pinning that would age the corpus by however long ago the constant was written and move the published NDCG numbers for a reason that has nothing to do with retrieval. The fixed clock also has to sit well clear of every validity boundary the corpus states (its nearest are 2020-06-01 and 2099-01-01); `TestContextInstantSitsInsideEveryGradedWindow` holds a 90-day margin, so a corpus edit that added a window closing next quarter fails loudly instead of quietly restating the table.

**What this does not measure: session-start injection.** The block measured here is the one `ghost_memory_search` assembles. The passive blocks (session start, `ghost context`, `ghost_project_context`) are a different request shape and are measured by [`ghost bench --passive`](#passive-context-ghost-bench---passive) instead (grading of pinned rows there is [#924](https://github.com/wcatz/ghost/issues/924)). The **cost** figures here do cover the rendered search response in full, framing and verdict line included, because that is what a caller receives and pays for; they do not cover a session-start injection.

**CI cost.** The context metrics are measured on the existing graded dataset at report time, not by a new test that reloads the 551-row corpus: the fixture is 8 rows, and `internal/bench`'s test time is unchanged within noise (14.36 s on `98ffe9c5` before this section, 11.7–13.5 s after it over six runs, so within noise; the new tests themselves read 0.17 s).

## Passive context (`ghost bench --passive`)

`ghost bench --context` cannot tell one version of the passive surfaces from another, for two reasons that are both about the corpus. It measures the block `ghost_memory_search` returns, which is a *query* request; the session-start block, `ghost context` and `ghost_project_context` are *passive* requests (no query, a bucket per project, a policy per bucket) built by `assemble.Run` in passive mode. And its 551-row corpus holds one project and no resolved, expired, out-of-window or `_global` row, so every filter a passive surface applies is a no-op on it and its contamination figure is `0.000` whatever the code does. `ghost bench --passive` is the measurement those two facts call for: a synthetic corpus built to discriminate, read through the production entry points.

**The corpus** (`internal/bench/passive_corpus.go`, deterministic, offline, no embedding, no LLM, clock fixed at `bench.ContextInstant()`): four projects and `_global`, 212 rows. Each large project (`alpha`, `beta`, `gamma`) holds 10 live rows (importance 0.95 down to 0.50), one pinned low-importance old row, three superseded rows each beside the row that replaced it, three near-duplicates of live rows (explicit `duplicate` edges, because nothing embeds offline), 30 live low-importance filler rows so every budget cuts, and the rows that must never appear: three `resolved`, three `expired` (`valid_until` in 2020), two `not yet valid` (`valid_from` in 2099) and two scoped to `env=staging`. The withheld rows carry the highest importance and the newest `created_at` in their project, so a filter that stopped working would put them at the top of the block, not somewhere a cap would hide them. `delta` is small on purpose: all of it fits under every cap, so it is the one place a near-duplicate or a superseded row is shown beside what it restates. `_global` carries the same classes. Every validity stamp is in `memory.StoredStampLayout` and asserted to parse, because an unreadable stamp is kept as unset and a fixture of them would show zero leakage for the wrong reason.

Each row is graded **expected** (live rows, and the pinned row on session start), **withheld** (resolved, expired, not yet valid; and out-of-scope rows, but only on a read that carries a scope) or **optional** (filler, superseded, near-duplicate, the pinned row on the two union surfaces, and out-of-scope rows on a read with no scope — an unscoped request matches every row, which is the documented rule, not a leak). Superseded and near-duplicate rows are optional because the project bucket *reorders* them behind the row that replaced them and leaves the drop to the cap (`DemoteOnlyWhenOverCap`); only `_global` drops them. The duplicate rate is what measures them.

**The surfaces**, each called through the function production calls, at its production budget, at the fixed clock:

| Surface | Entry point | Budget |
|---|---|---|
| session start and `ghost context` | `mcpinit.SessionBlockAt` → `loadSessionContextFrom` → `loadSessionPassive` → `assemble.Run` | `sessionPassiveBudget`: 15 project rows + 8 `_global` rows |
| the same, with `injection.session_scope` = `env=production` | the same | the same, so out-of-scope rows are withheld |
| `ghost_project_context` | `mcpserver.ProjectContextAt` → `projectContextBlock` → `projectContextMemories` → `assemble.Run` | `projectContextBudget(project, 20)`: one union bucket |
| `ghost://project/{id}/context`, `recall_project` | `mcpserver.ProjectResourceAt` → `buildProjectContext` | cap 20, plus a second request for `## Global` at 15 |

`SessionBlockAt`, `ProjectContextAt` and `ProjectResourceAt` are the only production changes this bench needed: each is the code the surface already ran, moved behind a function that takes the clock and leaves out the side effects of a session starting (the Obsidian mirror, the session counter, the query-key write). The hook, the CLI and the tool call the same functions as before. The configuration is the compiled defaults with the two knobs the passive budget reads stated in code, so a `GHOST_*` variable on the machine cannot move the report.

**The metrics**, each a fraction of a named population like the context report's:

| Metric | Reads |
|---|---|
| withheld leakage | withheld rows rendered / withheld rows within the read's reach. **Must be 0.** |
| expected-row recall | expected rows rendered / expected rows within the reach |
| cross-project | rows of another project rendered / rows rendered |
| `_global` share | `_global` rows rendered / rows rendered |
| budget use | rows rendered / the row caps the surface's budget states |
| budget cut | admissible rows left out / admissible rows |
| duplicate rate | rendered rows that restate, or are replaced by, another rendered row / rows rendered |
| header honesty | session start only: every `N shown of M total — K not shown …` line against the rows rendered and the corpus's own withheld and ranked-out counts |

Rows are recognised by their backticked id in the rendered text, never by reading `assemble.Result`: a figure read off the result is a statement about a value the caller never received. The honesty check's truth is the fixture's, not the product's: a bucket's *withheld* rows are the unresolved ones its kind says a stage removes, its *cut* rows are the eligible ones the block did not render, and the header is honest when "shown" is the number of lines rendered and "not shown … ranked by" is exactly the cut. The parser reads both the header an unfixed tree prints and the one #897 (PR #912) prints, so the same check runs before and after.

**What it reports on this tree** (`internal/bench/testdata/passive_report.golden`, pinned by `TestPassiveBaseline/pinned`):

```text
surface                               leakage   recall  cross-project  header honesty
session start / ghost context         0/47      1.000   0/88           PASS (8 of 8)
session start, production scope       0/58      1.000   0/87           PASS (8 of 8)
ghost_project_context (limit 20)      0/47      1.000   0/80           n/a
project resource / recall_project     0/47      1.000   0/103          n/a
```

- **No filter leaks on this tree.** Resolved, expired, not-yet-valid and (when asked for a scope) out-of-scope rows are rendered on none of the four surfaces. That is a reading of a corpus that *can* leak, which the contamination arms of `--context` are not. `TestPassiveMeasurementCatchesADisabledFilter` re-seeds the store with the validity windows removed, and then with the withdrawals removed, and the same measurement reports non-zero leakage on every surface, naming only rows of the kind that filter exists for. Deleting stage 2 from `internal/assemble` by hand does the same: session-start leakage went from `0.000 (0/47)` to `0.617 (29/47)`, all of it expired and not-yet-valid rows. `TestPassiveScorerFlagsWhatItIsGiven` plants a violation in a hand-built block to show the scorer is not vacuous.
- **The session-start header's counts agree with the rows rendered.** This check failed before #897 (PR #912): withheld rows were counted into "N not shown, ranked by a composite score" (alpha's header read `15 shown of 54 total — 39 not shown` when the ranking cut 34 and validity withheld 5). The tree now prints the tally from the trace and the check passes on all eight count lines. Two attribution rules are in the check, because the product's tally follows them: an out-of-scope row is excluded by the scope clause in the fetch, so it is in neither the total nor the withheld count; and the `_global` policy drops superseded and duplicate losers, which the tally may name as withheld, so the header may name between none and all of them as withheld and the ranked-out count moves with it.
- **The pinned row is graded `optional` on the two union surfaces, and the doc says why.** The union bucket behind `ghost_project_context` and the project resource reads with `OrderDecay` and no `TwoPass`, `BehaviorFloor` or pinned-first order, deliberately matching `GetTopMemories`; session start shows the pinned row through its category floor, not through the pin. A pin exempts a row from decay and is not documented as a slot guarantee on that read; whether it should be is a separate question, so the bench asserts nothing about it. The pinned rows are in a decaying category (`gotcha`): in the never-decay categories (preference, convention, fact) a pin changes nothing, so a pinned row there would test nothing.
- **Near-duplicates and superseded rows are shown only where there is room.** The duplicate rate is `0.045` at session start (4/88: `delta`'s two near-duplicates and its two superseded rows, each beside the row it restates or was replaced by), and `0.000` on the tool and the resource, whose union is full — the rows the project bucket demotes are the rows its cap then cuts.

**Limits.** The corpus is synthetic and small; a figure on it is a statement about these 212 rows. Recall and the budget figures move when the corpus moves, so the golden is the thing to review, not a floor. The out-of-scope row is only withheld where a scope is asked for (`injection.session_scope` ships empty), and `ghost_project_context` carries no scope at all, so on that surface a scoped row is optional by construction. Nothing here ranks relevance — there is no query — so a block can score `1.000` recall and still be the wrong block for a task. The bench is report-only and gates nothing; the leakage test is the one assertion, and it asserts that the figure is zero on a corpus that can make it non-zero.

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

At *every* weight that meaningfully helps staleness, the trap collapses. The best achievable `min(both)` is 0.214 — i.e. there is no global recency weight where both old-but-correct and newer-supersedes retrieval are acceptable, because the only signal (age) is exactly the thing that conflates the two cases. **Verdict: the blanket age-only recency prior is not defaultable and was removed.** Category-aware decay resolves the cliff because the **never-decay half** of the trap suite's memories is `fact` category, which never decays — under `TestDecayFrontier`, which is scoped to that half, the frontier collapses to two points `decay-off 0.083/0.929 → decay-on 1.000/0.929`, so staleness flips while the trap stays flat. The suite as a whole is no longer all `fact` — the scenarios added for this issue are `decision`, `gotcha`, `dependency`, `architecture` and `pattern`, and they do decay, which is the 0.833 → 0.417 cost in the table below. The exemption is a property of the `fact` category and not of the suite, which is why `TestDecayFrontier` is scoped to the never-decay half rather than run over all of it, and it is that exemption which lets `DecayEnabled` ship on by default.

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
                        staleness fresh-wins   recency-trap correct-wins (never-decay half)
both off                0.083                  0.929
decay on (DefaultSearchParams)          1.000                  0.929   ← decay alone flips staleness, trap untouched (never-decay half: fact never decays)
supersede demote on     1.000                  0.929   ← likewise, trap untouched (never-decay half: no supersession edge)
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

The comparison's other half is that the **headline** corpus is still the inert reference this suite is measured against, and that claim lives in `TestDecayDoesNotPerturbGradedBench` rather than here. It is asserted in the two forms the property actually has. **Decay is measured**: hybrid NDCG@10 and recall@10 are identical with `DecayEnabled` off and on. **The demote is structural**: no memory in the corpus declares a `supersedes` edge, and a row with no edge to consume is the demote's documented no-op (`demoteSuperseded`); what this suite's page cannot show is the demote *measured* on that corpus, because sweeping the demote toggle there would be two more passes over 551 memories and the toggle is `true` in both points the decay comparison sweeps. The "no edges, no effect" claim is measured once, on the recency-trap suite instead, where it costs a tenth of a second: `TestSupersedeDemoteClearsFrontier` runs it with the demote off as well as on and requires the two trap scores to be equal. So the corpus-side fact (no edges declared) and the ranking-side fact (no edges means no reorder) are both asserted, in the two places each is cheap to assert. A corpus that gains an age or an edge is reported by name and field, rather than quietly making this section's comparison unlike-for-like. The check sits where the corpus is already loaded for a cost reason as well: 551 memories with 768-dim vectors, and a corpus-wide test costs this package ~10s of a CI budget it does not have to spare — #677's sixth corpus-wide test is what failed `build-and-test` at 600.038s, and overlapping the five fixed it (the measurements are in `internal/bench/sweep_test.go`). What that budget is and how much of it is left are recorded in the `internal/bench` package-map bullet, which is the only place either number is written: #708's reproducibility test is the most recent thing to have spent from it, and it spent in the one form that cannot be trimmed — a second corpus load and a second pass over all 220 queries. The level itself is not re-asserted anywhere — that is `TestBenchRegressionFloors`' job. `TestRankedStateFixtureCarriesState` is the anti-vacuity guard — enough memories, at least eight distinct ages among decaying rows, at least six edges, **exactly one** three-deep chain (the one this section describes), and both classes of edge (one decay cannot move on its own, one with a wide age gap) — because a fixture that lost its state would score identically in all four rows and every delta above would be a difference between nothing and nothing.

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
| compact render, pre-#907 row shape (category + content) | **732 bytes** |
| legacy render, the shape before that (32-hex ID per line) | 852 bytes |
| byte saving the compact shape made over the legacy one | **120 bytes (~14%)** |
| shipped render — the 15 row lines through `assemble.Item.Line()` (#907) | **942 bytes** |
| shipped block, whole (headings, count lines, closing instruction) | **1459 bytes** |
| shipped-block budget the test now enforces | **2048 bytes** |

This evidences that the trade is a net win on selection: the category-priority
selection hits its behavioral floor even when high-importance descriptive rows
dominate the raw decay ranking. Ran 2026-09-08 on the
category-aware-injection worktree.

**What #907 did to the render columns, and the choice behind it.** Session-start
rows are rendered through the shared `assemble.Item.Line()` now, so one renderer
puts the labels on every surface — and every row carries a 32-hex id, an
importance and the validity/confidence/agent/source_ref/origin labels instead of
only its category and content. The choice was the shared renderer rather than a
session-start line kept compact with the label logic factored out beside it: two
row shapes that agree on labels but not on bytes would be two shapes for a
reader who compares session start with search. The cost is measured on the same
corpus and selection: **942 bytes of rows against the compact shape's 732 (+210
bytes, +29%) and the legacy with-id shape's 852 (+90 bytes, +11%), 1459 bytes
for the whole block** (measured 2026-10-07).

The compact and legacy columns are therefore the pre-#907 measurement of two
shapes neither of which reaches an agent any more; they stay because the test
still compares them and the saving they record was real when it shipped, but
they are no longer evidence about the shipped block — the claim "the compact
render shrinks (never grows) the injected block" describes the old trade, not
this one. The test now renders the shipped block through `formatSessionContext`
and fails if it grows past the 2048-byte budget, so the figure guarded is the
one an agent pays. Selection is untouched by the row shape: the behavioral floor
is still 8/8 and the cap is still 15.

## Storyline evals (`eval/storyline`) — local only, one shipped arc

`eval/storyline` measures a property no single-shot benchmark can: what a project
looks like **across** sessions when the ground truth changes mid-stream. A
reversal that the store holds correctly and the session-start block still reports
as current is invisible to every suite above, because each of them renders a
block once against a corpus whose answers never contradict each other.

One storyline ships, `reversed-decision` (project `northwind-api`): sessions 1
and 2 establish a decision, session 3 records its reversal, and the grade asks
whether the run's own lifecycle caught up —

| check | what it reads |
|---|---|
| `injection-present:session-N` | the block really reached session N (and is the size the session saw) |
| `carry-forward:session-N` | the previous session's record is in session N's block |
| `stale-original:session-N` | a record the store marks superseded is **not** in session N's block unmarked |
| `stale-original:final-block` | same, for the block rendered after the arc's own lifecycle |
| `supersede-edge:<newer>` | a live `supersedes` edge exists **and points the way the store's own stamps say it must** |
| `reversal-live:<newer>` | the replacement was not itself resolved (a supersede that resolves both is not a reversal) |
| `final-block-carries:<newer>` | the replacement survives into the last block |
| `judge:followed-reversal` | only with `-judge`: an LLM reading the final answer against the reversal |

The grade reads **store state** — `memory_links` rows and `memories` stamps — not
the CLI's stdout. Stdout is kept in the report because a warning is evidence, but
a phase that prints a verdict it did not write cannot pass a check.

Run it (local only, nothing is wired into CI):

```sh
go run ./eval/storyline -model opencode-go/glm-5.3-flash \
  -opencode-auth-file ~/.local/share/opencode/auth.json
```

Defaults need only the checkout: `-storyline reversed-decision`, `-repo .`,
`-results-dir eval/storyline/results`, and a 3-minute embedding drain. `-keep`
retains the scratch tree (`<repo>/.sandbox/`, holding the built binary, the store
and the raw output of every phase) for post-mortem; without it the tree is
removed. The run exits non-zero when a check fails, so it can be driven from a
script — but a failing check is a **finding about Ghost**, not a runner bug, and
the report is the artifact.

Isolation is eval/cycle's, reused rather than reinvented: the data dir, config dir
and HOME all point inside the run's own scratch tree, an inherited override of any
of them is dropped, `ANTHROPIC_API_KEY` is stripped, and an opencode credential is
copied into the scratch data dir (`-opencode-auth-file`) so the sandboxed
sessions authenticate without touching yours. Two further pins are the run's own:
the model is resolved **once** and passed to everything — the sessions, the judge,
`GHOST_OPENCODE_MODEL` in the child env, and the report's `model:` line — so the
arc is one model's behaviour end to end, and an inherited `GHOST_OPENCODE_MODEL`
in your shell cannot decide the sessions alone (`internal/ai` would otherwise
read it out of the runner's own environment while the phases used the default).
The phases pass `--source opencode`
rather than letting `ghost supersede`/`ghost resolve` resolve a harness by walking
the ancestry of whatever launched the runner (which would bill Claude for
verdicts about an opencode-driven arc, and fail outright when the sandbox holds
only opencode's credential).

Every record is written through the real MCP `ghost_memory_save`, the block is
rendered by the real `ghost context`, and the two lifecycle phases are the real
`ghost supersede`/`ghost resolve`. The only thing the runner reaches past the CLI
for is a chronology restamp: `created_at` has second granularity, so a reversal
seeded in the same second as the claim it reverses would leave the supersedes
direction an arbitrary tie-break — and the direction check would then grade
harness timing as a model failure. Restamping is metadata only.

**Measured 2026-09-29, `opencode-go/glm-5.3-flash`, local run, 9/10 checks pass:**

```
storyline: reversed-decision (northwind-api) — Long-lived project with a reversed early decision
building ghost binary from the checkout under test...
stage 1/3: 995 injected bytes, 533 answer bytes, 2 record(s) recorded
stage 2/3: 1248 injected bytes, 420 answer bytes, 2 record(s) recorded
stage 3/3: 1529 injected bytes, 442 answer bytes, 0 record(s) recorded
  embeddings drained: 6/6
PASS injection-present:session-1 … PASS carry-forward:session-2
PASS injection-present:session-3 … PASS carry-forward:session-3
FAIL stale-original:session-3 — session-store-redis (SESSION_STORE=redis) is
     superseded by session-store-postgres and is still in session-3 with nothing
     marking it as old
PASS stale-original:final-block … PASS supersede-edge:session-store-postgres
PASS reversal-live:session-store-postgres … PASS final-block-carries:session-store-postgres
error: 1 of 10 checks failed: stale-original:session-3
```

The finding is real and it is a *rendering* gap, not a storage gap: `ghost
supersede` linked the pair and `ghost resolve` stamped the Redis decision
`resolved_at` in the same run, and the final block correctly omits it. But
session 3's block — rendered before the arc's lifecycle ran — carried both the
current Postgres decision and the stale Redis one with nothing marking the old
claim. A reader had to resolve the contradiction themselves; the model in fact
did, correctly, in prose. Read the failing check as "the block does not say which
of two contradicting records is current", not as "the store lost the reversal".

**Known limits, stated so the number is not over-read.** The `stale-original`
match is deliberately narrow: it looks for the record's own verbatim text, so a
paraphrased restatement of the stale claim would pass it. `--judge` covers the
paraphrase case, and is off by default because it costs a model call and its
verdict is not deterministic. And the arc's lifecycle runs **after** the last
session by construction, so the runner cannot yet demonstrate the production
ordering (a supersede caught in session 2's lifecycle changing session 3's
block); it can only show what the block does when the reversal is already stored
and unresolved. Both are follow-ups, not claims.

## Reporting rules (all phases)

1. Harness, datasets, and judge prompts live in this repo.
2. Fixed seeds, temperature 0; single-run results labeled as such.
3. Per-category tables with sample sizes; raw per-question logs attached to the release.
4. Token cost and latency reported next to accuracy.
5. Negative or mediocre results get published too.
