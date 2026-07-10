# Benchmark plan

Ghost publishes no retrieval-quality numbers yet. This document is the methodology for changing that honestly. The guiding rule: **a score only exists if anyone can re-run the harness with one command** — fixed seeds, published judge prompts (where a judge is used at all), and per-question logs.

## Why these benchmarks and not others

- **LongMemEval** ([arXiv 2410.10813](https://arxiv.org/abs/2410.10813), ICLR 2025) is the consensus long-term-memory benchmark as of mid-2026: 500 questions, each with a haystack of chat sessions. The 470 answerable questions carry official evidence labels (`answer_session_ids`); the remaining 30 are abstention cases with no evidence labels, excluded from retrieval scoring. Crucially it supports a **retrieval-only evaluation** using those labels — no LLM judge, no API cost, fully deterministic.
- **LOCOMO** is skipped deliberately. Public audits found ~6.4% of its answer key wrong, its standard judge accepts a majority of intentionally wrong answers, and trivial baselines (full-context, even filesystem+grep) beat specialized memory systems on it. A 2026 reader discounts LOCOMO numbers; we won't publish one.
- **Zep's DMR** is skipped — 60-message conversations that fit trivially in any context window; Zep itself moved on from it.

## Phase 1 — LongMemEval-S retrieval-only (judge-free)

The first published numbers. A Go harness that, per question, ingests the haystack sessions into a fresh Ghost store, runs Ghost's real search paths, and scores against the official evidence labels on the 470 non-abstention questions.

- **Metrics:** session-level Recall@1/5/10, MRR@10, NDCG@10.
- **Ablations (each an architecture claim under test):**
  1. FTS5-only (no embeddings)
  2. vector-only
  3. hybrid RRF (the shipped 70/30 fusion, k=60)
  4. hybrid + graph-expansion bonus (weight 0.15, after the linking worker runs)
- **Cost:** $0 API. Wall-clock is dominated by locally embedding the haystack through Ollama (`nomic-embed-text:v1.5`); the FTS5-only ablation runs in minutes.
- **Published anchors for context:** the LongMemEval paper's flat-index baseline (session-level Recall@5 ≈ 0.64 on the -M variant) and reported ~95% Recall@5 hybrid results on -S.

## Phase 2 — `ghost bench`: an in-repo dataset + CI regression floors

A `ghost bench` subcommand over a small self-authored dataset (~150 memories across Ghost's 8 categories, ~40 graded queries), with committed real `nomic-embed-text:v1.5` embedding fixtures so CI needs no Ollama.

- Ghost's search entrypoint (`SearchHybrid`) takes the query vector as a parameter, so the harness drives the exact production FTS/hybrid/graph code paths deterministically.
- Regression tests assert **metric floors** (e.g. NDCG@10 ≥ target per ablation), not exact rankings — RRF fused scores tie easily and exact goldens would be brittle.
- Every published table states dataset size, embedding model, and commit hash.
- A follow-up refactor extracts the RRF fusion into a parameterized function, enabling `ghost bench --sweep` — an empirical basis for the currently hand-tuned knobs (70/30 fusion weights, graph bonus 0.15, cosine link threshold 0.70).

## Phase 3 — staleness suite (the flagship)

Deterministic scenarios for the failure users actually complain about: agents acting on superseded facts ("prod runs Postgres 14" retrieved after the migration to 16). Modeled on the MemTrace error taxonomy ([arXiv 2605.28732](https://arxiv.org/abs/2605.28732)) and STALE probe design ([arXiv 2605.06527](https://arxiv.org/abs/2605.06527)):

- Save fact v1; later save superseding v2; assert search ranks v2 above v1 (**fresh-wins rate**, **fresh@1**), including for queries that presuppose the outdated state, and across update chains (v1→v2→v3).
- Deletion regressions: reflection must never drop pinned or manual memories (codifies the existing empty-set guard and snapshot behavior).
- Runs in CI in seconds. No LLM judge.

This suite is expected to *fail* in places at first — current search has no recency signal in ranking (decay applies to project-context reads via `GetTopMemories`, not to `SearchHybrid` or the session hook), and `supersedes` links exist in the schema but nothing creates them yet. That's the point: the suite specifies the desired behavior before those features ship, and documents progress honestly. It lands in CI as **report-only** (results printed, never failing the build); individual scenarios graduate to enforced assertions as the features they specify ship.

## Phase 4 — end-to-end LongMemEval-S (leaderboard-comparable)

Only after phases 1–3: the official harness (`evaluate_qa.py`) with the **standard GPT-4o judge** — substituting a different judge makes numbers non-comparable, which is a known problem with some published scores. Fixed generator model, temperature 0, single deterministic run, per-case results JSON and full logs committed, an explicit note proving the memory system never saw oracle context, and generator model stated prominently (it dominates the score). Estimated cost: ~$30–80 in API calls.

Reference points, all judged with the official GPT-4o harness but with **different generators** (which dominate the score — compare within-generator only): Zep 71.2% and full-context 60.2% (GPT-4o generator); Mastra 94.87% (gpt-5-mini generator; 84.23% with GPT-4o); agentmemory 96.2% (Claude Opus 4.6 generator, temperature 0).

## Reporting rules (all phases)

1. Harness, datasets, and judge prompts live in this repo.
2. Fixed seeds, temperature 0; single-run results labeled as such.
3. Per-category tables with sample sizes; raw per-question logs attached to the release.
4. Token cost and latency reported next to accuracy.
5. Negative or mediocre results get published too.
