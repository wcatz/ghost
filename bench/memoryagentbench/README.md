# MemoryAgentBench Conflict-Resolution — supersede ablation

Runs Ghost's real `ghost supersede` pipeline (the actual classifier, run via
the `opencode` CLI rather than the Anthropic API) against MemoryAgentBench's
`Conflict_Resolution`
split ([HF: `ai-hyz/MemoryAgentBench`](https://huggingface.co/datasets/ai-hyz/MemoryAgentBench),
ICLR 2026), and scores retrieval before/after supersede links exist. See
[the design doc](../../docs/superpowers/specs/2026-08-20-memoryagentbench-supersede-benchmark-design.md)
for the full rationale.

Single-hop (`fact_sh`) only — multi-hop (`fact_mh`) needs a query-decomposition
step this harness deliberately doesn't have (retrieval-only, no generation).

## Setup

Run everything below from this directory (`bench/memoryagentbench/`):

```bash
pip install pyarrow
curl -sL -o Conflict_Resolution-00000-of-00001.parquet \
    "https://huggingface.co/datasets/ai-hyz/MemoryAgentBench/resolve/main/data/Conflict_Resolution-00000-of-00001.parquet"
python3 convert.py --parquet Conflict_Resolution-00000-of-00001.parquet --out demos.jsonl
```

## Run

```bash
# requires the `opencode` CLI on PATH (or cli.opencode_binary in
# ~/.config/ghost/config.yaml) — no ANTHROPIC_API_KEY needed
# also requires a local Ollama serving `nomic-embed-text:v1.5` (the same
# model Ghost uses in production) — pass --ollama <url> if it's not on
# localhost:11434
mkdir -p ~/.cache/ghost-bench
go run . --data demos.jsonl \
    --embed-cache ~/.cache/ghost-bench/mabench-embed-cache.jsonl \
    --out per-question.jsonl
```

`--threshold` (default `0.80`, matching `ghost supersede`'s own default) sets
the minimum cosine similarity for a candidate pair to be offered to the
classifier at all.

The downloaded `.parquet`, the converted `demos.jsonl`, the embed cache, and
`per-question.jsonl` are all local run artifacts — none of them are committed
(`*.jsonl` is gitignored in this directory; the `.parquet` just isn't checked
in by convention).

## Scope and cost

- Default `--sources` runs `factconsolidation_sh_6k,factconsolidation_sh_32k`
  only — a few hundred facts each, so the classifier sees at most
  `facts × 8` (`supersede.maxNeighbors`) candidate pairs, deduped: at most
  that many `opencode run` subprocess calls.
- `factconsolidation_sh_64k`/`factconsolidation_sh_262k` are reachable via
  `--sources factconsolidation_sh_64k` etc., but scale the haystack ~5x/40x —
  expect a proportional jump in candidate pairs and `opencode` calls (each is
  a subprocess spawn, so wall-clock — not API cost — is the constraint at
  that scale). Not run by default.
- No CI gate: every run shells out to `opencode` once per candidate pair.
- `ghost resolve` is not exercised here — its keyword prefilter
  (`"resolved"`, `"shipped"`, `"deprecated"`, etc.) doesn't match
  FactConsolidation's neutral factual sentences.

## Reading the table

```
source                          n     base@1     base@5  supersede@1  supersede@5   cand   conf   made
factconsolidation_sh_6k       100      0.XXX      0.XXX        0.XXX        0.XXX    NNN    NNN    NNN
```

`base@k` scores plain hybrid search (`SupersedeDemote: false`); `supersede@k`
scores the same questions with `SupersedeDemote: true` (production default)
after the real classifier's verdicts are applied — both against the same
post-supersede store, so the difference isolates what the ranking switch
alone contributes. `cand`/`conf`/`made` are `supersede.Result`'s
`Candidates`/`Confirmed`/`Created`: how many same-subject pairs the
classifier was offered, how many it called SUPERSEDES, and how many links
were actually written.

Numbers get published in `docs/benchmarks.md` under a new phase once there's
a real run to report — see the design doc.
