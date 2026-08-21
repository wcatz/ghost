# Benchmark plan

This document is the methodology for publishing retrieval-quality numbers honestly. The guiding rule: **a score only exists if anyone can re-run the harness with one command** — fixed seeds, published judge prompts (where a judge is used at all), and per-question logs.

**Status:** Phase 1 (LongMemEval-S retrieval), Phase 2 (`ghost bench`), and Phase 4 (end-to-end retrieve→generate→judge with DeepSeek v4 Pro) have shipped with published numbers below. Phase 3 (staleness suite) ships report-only in CI. The official GPT-4o leaderboard-comparable run has not been executed yet.

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
- **CI gating:** only the **fts** floor (`R@5 ≥ 0.74`, `NDCG@10 ≥ 0.72`) is enforced automatically on PRs — it needs no Ollama and finishes fast. The **hybrid** floor (`R@5 ≥ 0.91`, `NDCG@10 ≥ 0.89`) is run **manually** (`workflow_dispatch`) or locally, not on a schedule: the cold embedding pass is CPU-bound (the ~12h above), too slow for any CI cap. Because `nomic-embed-text:v1.5` is deterministic, a cold run computes the same vectors as a warm one, so those hybrid floors are fully established by the warm local numbers here — CI need not re-derive them.

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
```

Two findings, both honest:

- **Hybrid fusion earns its keep.** Hybrid NDCG@10 (0.989) beats both single legs (FTS 0.965, vector 0.946) — the 70/30 RRF weighting is a net win on this dataset. `TestBenchRegressionFloors` asserts this relationship so a regression trips CI.
- **The graph-expansion bonus was evaluated and removed.** An additive link-graph bonus (former 0.15 default) lifted semantically-adjacent neighbors above exact matches, and a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth. It shipped at `GraphWeight 0` and has now been removed entirely (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`). The link graph is retained for the Obsidian mirror and `supersedes` ranking.

The dataset is deliberately a v1 starter (all 8 categories represented); growing it toward ~150 memories / ~40 graded queries is planned. Regression tests assert **metric floors** (a little below observed), not exact rankings, since RRF scores can tie.

### Parameter sweep (`ghost bench --sweep`)

The RRF fusion is parameterized (`memory.SearchParams`), and `ghost bench --sweep` grid-searches the vector-leg weight (FTS = complement) — 6 combinations over the same dataset, one prepared store. Findings from the first sweep (full table: run `ghost bench --sweep`):

- **Leg weights are robust.** Vec 0.3–0.7 all land within 0.989–0.992 NDCG; only vec ≥ 0.8 degrades. The shipped 70/30 weighting is fine; there is no evidence for changing it from a 14-query dataset.
- **Outcome: the 70/30 leg weighting ships unchanged, and the graph bonus was removed.** With the leg weights robust across vec 0.3–0.7, there is no evidence to change the shipped 70/30 split. The graph-expansion bonus was removed rather than kept disabled (see the spec linked above); the link graph is still built for the Obsidian mirror and `supersedes`.

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

**Both halves now ship. Creation:** `ghost supersede <project> [--apply]` (`internal/supersede`) proposes newer→older candidate pairs from cosine-similar memories (tighter than the 0.70 'related' floor), confirms each with a single Haiku call, and writes star `supersedes` links (`source='llm'`). It is re-runnable and self-heals after reflection cascade-deletes links (`ReplaceNonManual` reinserts memories with new IDs — a re-run rebuilds the links, exactly as the cosine worker rebuilds 'related'). The cosine worker is rejected as the creator itself — symmetric similarity can't assign direction (the failure that got the graph bonus disabled), so similarity only *proposes* and the LLM *confirms + directs*. The classifier prompt is biased toward NO (a false supersedes buries a valid memory), and on a labeled set of genuine-vs-parallel pairs it scored 8/8 (`TestHaikuClassifierLive`, run manually with an API key; skipped in CI).

**Consumption has now graduated; creation stays opt-in.** `DefaultSearchParams` ships `DecayEnabled: true` and `SupersedeDemote: true`, so production `SearchHybrid` / `SearchHybridAll` (i.e. `ghost_memory_search` and `ghost_search_all`, including their FTS-only fallbacks) apply decay reordering and consume `supersedes` links. Creation of `supersedes` links remains opt-in — links only exist if the user ran `ghost supersede --apply` — so the demote is still a hard no-op for anyone who has not asked for it, and the numbers above are what back the flip: decay alone moves staleness fresh-wins 0.083 → 1.000 with recency-trap correct-wins unchanged at 0.929 (`TestDecayFrontier`); the demote does the same 0.083 → 1.000 with trap flat at 0.929 (`TestSupersedeDemoteClearsFrontier`). Either half alone clears the frontier, and both on keeps it cleared. It was flipped because an eval run found the opposite failure: a memory the user had explicitly marked as replaced still outranked its replacement in live search. `TestProductionSearchDemotesSuperseded` (internal/memory) guards the production entry points; `SearchHybridParams` still takes explicit params for the sweep harness.

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

| Provider | Endpoint | Notes |
|----------|----------|-------|
| `openai` (default) | `api.openai.com` | Leaderboard-comparable with gpt-4o |
| `openai` + `--api-base-url` | Any OpenAI-compatible | **OpenCode Go** (`https://opencode.ai/zen/go`), DeepSeek direct, etc. |
| `anthropic` | `api.anthropic.com` | Internal check, not leaderboard-comparable |

OpenCode Go ($10/mo) provides DeepSeek V4 Pro at ~$0.66-1.32/M input tokens (peak/off-peak), fitting the full 500-question benchmark within the $60/mo usage limit. The `--api-base-url` flag routes requests through any OpenAI-compatible endpoint; a `User-Agent` header is included for Cloudflare compatibility, and `GoUsageLimitError` responses auto-sleep until the rate limit resets.

Results (2026-08-20, DeepSeek v4 Pro as both generator and judge, **500 questions** including 30 abstention, `topk_context=5`):

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
| **Ghost (hybrid)** | **96.2%** | DeepSeek V4 Pro | This repo |
| Mem0 | 94.4% | Not specified | [mem0.ai/research](https://mem0.ai/research) — "managed platform, proprietary optimizations not in OSS SDK" |
| Hindsight | 91.4% | Gemini-3 Pro | [arxiv 2512.12818](https://arxiv.org/abs/2512.12818), [benchmarks](https://benchmarks.hindsight.vectorize.io/) — independently validated by Virginia Tech + Washington Post |
| Supermemory | 85.2% | Gemini-3 | [supermemory.ai/research](https://supermemory.ai/research/longmembench/) — self-reported |

**Read carefully:** These numbers are **not directly comparable** across rows. Each uses a different generator model and (in Ghost's case) a different judge. Within the same generator+judge pair, differences are meaningful — across pairs, they're directional only. Ghost's self-judged score carries the same caveat as every other system that judges its own output.

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

## Classifier fallback verification (2026-07-26)

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
  eligibility re-check, expected non-determinism across two separate Haiku
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

## Reporting rules (all phases)

1. Harness, datasets, and judge prompts live in this repo.
2. Fixed seeds, temperature 0; single-run results labeled as such.
3. Per-category tables with sample sizes; raw per-question logs attached to the release.
4. Token cost and latency reported next to accuracy.
5. Negative or mediocre results get published too.
