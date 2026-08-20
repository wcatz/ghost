# MemoryAgentBench Conflict-Resolution Benchmark — Design

## Goal

Ghost has two supersede-adjacent benchmarks today, and neither exercises the actual classifier:

- Phase 3 (`internal/bench/staleness.go`) hand-authors save-v1/save-v2 pairs and tests whether `SearchParams.RecencyWeight`/`SupersedeDemote` re-ranks them correctly. It never runs `ghost supersede` itself — the `supersedes` links it scores against are asserted directly, not produced by the LLM classifier.
- `internal/supersede`'s own unit tests use a mocked `Classifier`.

Nothing runs the real Haiku classifier over data it didn't author, to see whether it actually catches contradicting facts. This benchmark closes that gap using MemoryAgentBench's `Conflict_Resolution` split ([HF: `ai-hyz/MemoryAgentBench`](https://huggingface.co/datasets/ai-hyz/MemoryAgentBench), ICLR 2026), whose `fact_sh`/`fact_mh` tasks were purpose-built to test exactly this: fact updates that must be tracked correctly against a haystack of distractors.

## Data

The `Conflict_Resolution` split is a single parquet file with 8 rows — one per (question-type, context-size) combination: `{fact_sh, fact_mh} × {6k, 32k, 64k, 262k}`. Each row has:

- `context`: a flat, numbered list of subject-relation-object fact sentences (e.g. `"16. Chanel was founded by Coco Chanel."`), with **no session/turn structure** — confirmed empirically (`metadata.haystack_sessions` is `None` for every row in this split). Contradicting facts appear as later-numbered lines restating the same subject+relation with a different object (e.g. line 34 restates line 16 with `"Andy Warhol"`) — synthetic/counterfactual content so no LLM's parametric knowledge can shortcut the answer. The gold answer always matches the **last-stated** version.
- `questions` (100 per row) / `answers` (list of acceptable strings per question) / `metadata.qa_pair_ids`.
- `metadata.source` names the row, e.g. `factconsolidation_sh_6k`.

`fact_sh` questions are single-hop (one fact lookup). `fact_mh` chains 2+ facts (e.g. "country of citizenship of the spouse of the author of X") and requires a decomposition/reasoning step no pure-retrieval harness can score honestly — **out of scope for this design**, revisit only if a generation step is added later.

### Conversion step

`bench/memoryagentbench/convert.py` (pyarrow; same role as `bench/longmemeval/phase4/merge_retrieval.py`):
1. Reads a locally-downloaded `data/Conflict_Resolution-00000-of-00001.parquet`.
2. Filters to rows whose `metadata.source` matches `factconsolidation_sh_*`.
3. Writes one JSON object per line (JSONL) with `{source, context, questions, answers, qa_pair_ids}` — `context` passed through raw.

The regex split of `context` into an ordered `facts: []string` list (on `^\d+\.\s`, list order = temporal order — line N is stated after line N-1) happens in Go (`splitFacts`, `bench/memoryagentbench/dataset.go`), not in `convert.py`. Keeping the split in Go — the language the rest of the harness and its tests are written in — makes it a `go test`-covered pure function instead of untested Python; `convert.py` stays a dumb format-and-filter step with nothing benchmark-specific to get wrong.

Output is not committed, matching the existing convention that external benchmark data (`longmemeval_s_cleaned.json`) is reproduced locally, not vendored.

## Go harness — `bench/memoryagentbench/main.go`

Standalone `package main`, structurally mirrors `bench/longmemeval/main.go`. Per demo row:

1. Open an owned `*sql.DB` + `memory.NewStore(db, logger)`, keeping the raw `db` handle alongside the store — the same pattern `internal/bench/staleness.go` uses for its `backdate` helper, needed for step 2.
2. Seed every fact via `store.Create` (`category: "fact"`, `source: "bench"`), then immediately backdate it (`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, duplicated from `internal/bench/staleness.go`'s `backdate`) to a strictly increasing timestamp matching list order. **This is required, not cosmetic**: `supersede.orient()` (`internal/supersede/supersede.go`) decides newer/older by `updated_at`, and a plain batch-insert gives every row the same timestamp (SQLite `datetime('now')`), making orientation undefined for every candidate pair.
3. Embed every fact and every question via a `cachedEmbedder` duplicated from `bench/longmemeval/embed.go` (`nomic-embed-text:v1.5` over Ollama, content-hash-keyed append-only JSONL cache — no shared package extraction, matching the existing per-benchmark-self-contained convention in `bench/`).
4. Run the real pipeline under test: `supersede.SelectCandidates` then `supersede.Run(ctx, store, haikuClassifier, projectID, threshold, apply=true, logger)` — the actual `internal/supersede/haiku.go` classifier, real Anthropic API calls. `threshold` defaults to `0.80`, matching `ghost supersede`'s own default (`cmd/ghost/main.go`), overridable via a `--threshold` flag for consistency with the CLI.
5. Score every question twice (ablation):
   - **Baseline**: `store.SearchHybrid` with `SupersedeDemote: false` — does plain hybrid ranking already favor the latest-stated fact, or does the stale original often win on literal similarity alone?
   - **With supersede**: same search, `SupersedeDemote: true` (production default), run after step 4's links exist.
   - A question scores a hit at `@k` if any of its gold `answers` appears as a case-insensitive substring of the top-`k` results' `Content` — same metric shape as MemoryAgentBench's own `substring_exact_match`, kept retrieval-only (no generation step).
6. Print a per-demo table: baseline vs with-supersede accuracy@1/@5, plus the supersede pass's own `Result` (`Candidates`/`Confirmed`/`Created`) so a reader can see how many contradiction pairs the classifier was offered vs. actually confirmed.

## Scope and cost for v1

- `fact_sh` only (see multi-hop exclusion above).
- Default run covers `sh_6k` and `sh_32k` (a few hundred facts each; candidate pairs bounded by `supersede.maxNeighbors = 8` per fact, so LLM calls stay proportional to fact count). `sh_64k`/`sh_262k` are reachable via an explicit `--source` flag value but not run by default — the README documents the expected cost jump (roughly 5x/40x the haystack size), mirroring `phase4`'s `cost_estimate.py` convention.
- No CI gate. Every run spends real Anthropic API credits (Haiku, per candidate pair) — same reason Phase 1's hybrid floor is manual-only and Phase 3 ships report-only.
- `ghost resolve` is not exercised by this benchmark. Its keyword prefilter (`"resolved"`, `"shipped"`, `"deprecated"`, `"reverted"`, etc.) is tuned for closed-thread project language and does not match FactConsolidation's neutral declarative sentences — noted explicitly so a reader doesn't expect resolve numbers out of this harness.
- The other three MemoryAgentBench splits (`Accurate_Retrieval`, `Long_Range_Understanding`, `Test_Time_Learning`) are not part of this design; a future phase could cover them separately.

## Reporting

Once a real run exists, its numbers get a new phase section in `docs/benchmarks.md` (next available number after the current Phase 4), following the existing phase-numbering and "one honest command reproduces this" convention. Per-question logs follow the `--out` pattern already used by `bench/longmemeval`.
