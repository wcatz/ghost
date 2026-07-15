# Benchmark plan

This document is the methodology for publishing retrieval-quality numbers honestly. The guiding rule: **a score only exists if anyone can re-run the harness with one command** — fixed seeds, published judge prompts (where a judge is used at all), and per-question logs.

**Status:** Phase 1 (LongMemEval-S retrieval) and Phase 2 (`ghost bench`) have shipped with published numbers below. Phase 3 (staleness suite) ships report-only in CI. Phase 4 (end-to-end with the official judge) is next.

## Why these benchmarks and not others

- **LongMemEval** ([arXiv 2410.10813](https://arxiv.org/abs/2410.10813), ICLR 2025) is the consensus long-term-memory benchmark as of mid-2026: 500 questions, each with a haystack of chat sessions. The 470 answerable questions carry official evidence labels (`answer_session_ids`); the remaining 30 are abstention cases with no evidence labels, excluded from retrieval scoring. Crucially it supports a **retrieval-only evaluation** using those labels — no LLM judge, no API cost, fully deterministic.
- **LOCOMO** is skipped deliberately. Public audits found ~6.4% of its answer key wrong, its standard judge accepts a majority of intentionally wrong answers, and trivial baselines (full-context, even filesystem+grep) beat specialized memory systems on it. A 2026 reader discounts LOCOMO numbers; we won't publish one.
- **Zep's DMR** is skipped — 60-message conversations that fit trivially in any context window; Zep itself moved on from it.

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
- **Honest nuance: on this chat-style benchmark, vector-only ties hybrid** (vector edges R@1/MRR/NDCG, hybrid edges deep recall R@5/R@10). On the dev-facts `ghost bench` dataset below, hybrid beats vector decisively (NDCG 0.989 vs 0.946) — exact identifiers (ports, versions, hostnames) need the keyword leg. Fusion is the robustness play across both data shapes, which is exactly why a memory system for coding agents ships it.
- **Remaining headroom is at R@1** (0.532 overall; `multi-session` 0.371, `temporal-reasoning` 0.379) — R@10 is close to saturated, so the next win is ranking, not recall.
- Reproduce: `go run ./bench/longmemeval --data <longmemeval_s_cleaned.json> --condition fts|vector|hybrid --embed-cache <cache.jsonl>`. The append-only content-hash cache makes reruns and interruptions cheap.
- The hybrid+graph ablation is deliberately not run here: the graph bonus is disabled in production defaults after the Phase-2 sweep measured it degrading ranking; it stays under measurement in `ghost bench` until a redesign beats graph=0.

## Phase 1b — end-to-end anchors (for later comparison)

Published end-to-end (answer-accuracy) numbers use a GPT-4o judge and a generator that dominates the score — see Phase 4. Retrieval-only numbers above are not comparable to those percentages.

## Phase 2 — `ghost bench`: an in-repo dataset + CI regression floors — SHIPPED

`ghost bench` runs a self-authored graded dataset (in `internal/bench/testdata/`) with a committed real `nomic-embed-text:v1.5` embedding fixture, so CI runs the vector/hybrid conditions with no Ollama. The harness (`internal/bench/`) drives Ghost's production `SearchFTS`/`SearchVector`/`SearchHybrid` over a fresh in-memory store and scores judge-free IR metrics.

Current numbers (v1 dataset: 22 memories spanning all 8 categories, 14 graded queries; retrieval-only, no LLM judge; fully deterministic — reproduce with `ghost bench`):

```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.786   0.964   1.000    0.964    0.965
vector-only      0.786   0.929   0.964    0.952    0.946
hybrid           0.857   0.964   1.000    1.000    0.989
hybrid+graph     0.500   0.964   1.000    0.780    0.824
```

Two findings, both honest:

- **Hybrid fusion earns its keep.** Hybrid NDCG@10 (0.989) beats both single legs (FTS 0.965, vector 0.946) — the 70/30 RRF weighting is a net win on this dataset. `TestBenchRegressionFloors` asserts this relationship so a regression trips CI.
- **The graph-expansion bonus hurts, so it now ships disabled.** `hybrid+graph` is *worse* than plain hybrid (NDCG 0.824, R@1 0.500) — the additive bonus at its former 0.15 default lifts semantically-adjacent neighbors above exact matches. Following the parameter sweep below, production defaults ship with `GraphWeight 0`; the ablation opts into the candidate 0.15 weight so the signal stays under measurement.

The dataset is deliberately a v1 starter (all 8 categories represented); growing it toward ~150 memories / ~40 graded queries is planned. Regression tests assert **metric floors** (a little below observed), not exact rankings, since RRF scores can tie.

### Parameter sweep (`ghost bench --sweep`)

The RRF fusion is parameterized (`memory.SearchParams`), and `ghost bench --sweep` grid-searches the vector-leg weight (FTS = complement) × the graph-bonus weight — 36 combinations over the same dataset, one prepared store, link graph built once. Findings from the first sweep (full table: run `ghost bench --sweep`):

- **The graph bonus degrades retrieval monotonically.** `graph=0` and `graph=0.02` tie for best at every leg weighting (0.02 is too small to reorder anything — effectively off); at vec 0.3–0.7, `0.05` costs ~2.5 NDCG points and `0.10` costs ~9 (worse still at vec ≥ 0.8); the former `0.15` default put the production configuration (NDCG 0.824, rank 24 of 36, in a display tie spilling into the bottom third) far below every graph-off point. The additive strength-scaled bonus, at any effective magnitude, lifts semantically-adjacent neighbors above exact matches on this dataset.
- **Leg weights are robust.** With the graph off, vec 0.3–0.7 all land within 0.989–0.992 NDCG; only vec ≥ 0.8 degrades. The shipped 70/30 weighting is fine; there is no evidence for changing it from a 14-query dataset.
- **Outcome: production defaults now ship with `GraphWeight 0`.** The link graph is still built (it powers the Obsidian mirror's graph view and future link-aware features), and the bonus mechanism remains behind `SearchHybridParams`. A redesign — e.g. relation-aware or seed-confidence-gated expansion — ships only when it beats `graph=0` in this sweep.

## Phase 3 — staleness suite (the flagship)

Deterministic scenarios for the failure users actually complain about: agents acting on superseded facts ("prod runs Postgres 14" retrieved after the migration to 16). Modeled on the MemTrace error taxonomy ([arXiv 2605.28732](https://arxiv.org/abs/2605.28732)) and STALE probe design ([arXiv 2605.06527](https://arxiv.org/abs/2605.06527)):

- Save fact v1; later save superseding v2; assert search ranks v2 above v1 (**fresh-wins rate**, **fresh@1**), including for queries that presuppose the outdated state, and across update chains (v1→v2→v3).
- Deletion regressions: reflection must never drop pinned or manual memories (codifies the existing empty-set guard and snapshot behavior).
- Runs in CI in seconds. No LLM judge.

This suite was designed to *fail* at first — production search had no recency signal in ranking (decay lived only in `GetTopMemories`, not `SearchHybrid`). At the shipped default it still reports **fresh-wins 0.083** (fresh-found 1.000 — the update is always retrieved, just out-ranked by its shorter, older original). It lands in CI as **report-only**; scenarios graduate to enforced assertions as the fix ships.

### The recency prior (mechanism shipped, default off)

`SearchParams.RecencyWeight` adds a freshness prior to the final ranking window: `final = base * (1 + RecencyWeight · recency(age))`, `recency = 1/(1 + age_days/RecencyTau)`, age from each memory's `created_at`. It reorders **within** the already-returned top-`limit` set, so it can never drop a result that would otherwise be returned, and at the default `RecencyWeight 0` it is a hard no-op — production ranking is byte-identical (the `ghost bench` NDCG@10 0.989 and `hybrid ≥ single legs` floors are unchanged, verified in CI).

Turned on in the sweep, it **flips the staleness suite from fresh-wins 0.083 to 1.000** (`TestStalenessRecencyProof`, w=2/τ=30). It is provably inert on the graded benchmarks: those datasets seed via `store.Create`, which never sets `created_at`, so every candidate shares a timestamp and the recency factor is identical across them — no reorder possible at any weight (`TestRecencyDoesNotPerturbGradedBench`).

**Why it stays off as a global default — the recency-trap experiment.** The predicted risk was that a global recency prior can't tell "superseded" from "old-but-still-true." A second fixture (`internal/bench/testdata/recency_trap.jsonl`) tests the opposite of staleness: the *older* memory is the correct answer, with a newer keyword-overlapping distractor that recency would wrongly promote (`correct-wins` = correct outranks every trap). Sweeping `RecencyWeight` against both suites at once (`TestRecencyFrontier`) is not a gentle tradeoff — it's a cliff:

```text
recency   staleness-fresh   trap-correct   min(both)
0.00      0.083             0.929          0.083
0.05      0.750             0.214          0.214   ← best min(both)
0.10      0.917             0.071          0.071
0.15      0.979             0.000          0.000
0.25+     1.000             0.000          0.000
```

At *every* weight that meaningfully helps staleness, the trap collapses. The best achievable `min(both)` is 0.214 — i.e. there is no global recency weight where both old-but-correct and newer-supersedes retrieval are acceptable, because the only signal (age) is exactly the thing that conflates the two cases. **Verdict: the recency prior is not defaultable; it ships off permanently as a global default** and remains a per-query / sweep-tuning tool.

**The real fix is targeted, not global:** LLM-classified `supersedes` links (the schema relation exists, unused) consumed by a demote that fires *only when a memory's actual superseder co-occurs in the results*. That never touches the trap scenarios — correct-old and trap-new are unrelated facts with no supersedes edge between them — so it can flip staleness without the collateral damage the frontier shows. It needs the reflection-LLM creator (the cosine linking worker is rejected: symmetric similarity can't assign direction, the same failure that got the graph-expansion bonus disabled). That is the next roadmap item; the recency prior stays as the measured baseline it must beat.

## Phase 4 — end-to-end LongMemEval-S (leaderboard-comparable)

Only after phases 1–3: the official harness (`evaluate_qa.py`) with the **standard GPT-4o judge** — substituting a different judge makes numbers non-comparable, which is a known problem with some published scores. Fixed generator model, temperature 0, single deterministic run, per-case results JSON and full logs committed, an explicit note proving the memory system never saw oracle context, and generator model stated prominently (it dominates the score). Estimated cost: ~$30–80 in API calls.

Reference points, all judged with the official GPT-4o harness but with **different generators** (which dominate the score — compare within-generator only): Zep 71.2% and full-context 60.2% (GPT-4o generator); Mastra 94.87% (gpt-5-mini generator; 84.23% with GPT-4o); agentmemory 96.2% (Claude Opus 4.6 generator, temperature 0).

## Reporting rules (all phases)

1. Harness, datasets, and judge prompts live in this repo.
2. Fixed seeds, temperature 0; single-run results labeled as such.
3. Per-category tables with sample sizes; raw per-question logs attached to the release.
4. Token cost and latency reported next to accuracy.
5. Negative or mediocre results get published too.
