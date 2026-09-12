# Phase 4 — end-to-end LongMemEval-S (retrieve → generate → judge)

Phase 1 (`bench/longmemeval`, see [docs/benchmarks.md](../../../docs/benchmarks.md))
scores **retrieval only**: does Ghost rank the gold session highly? Phase 4
closes the loop — feed Ghost's retrieved sessions to an LLM, have it answer,
and grade the answers with an LLM judge — so the number is comparable in shape
to the LongMemEval leaderboard's QA accuracy.

The pipeline is four stages. Retrieval is Ghost (Go); generation and judging
reuse the **original** LongMemEval prompt/grading code, with only the API
client swapped, so the numbers stay reproducible against the published harness.

```text
┌── 1. retrieve (Ghost) ─────────────────────────────────────────────┐
│ go run ./bench/longmemeval -data longmemeval_s_cleaned.json \       │
│     -condition hybrid -ollama http://localhost:11434 \              │
│     -embed-cache ~/.cache/ghost-bench/embed-cache.jsonl \           │
│     -retrieval-out ranked.jsonl                                     │
└────────────────────────────────────────────────────────────────────┘
┌── 2. merge into dataset ───────────────────────────────────────────┐
│ python merge_retrieval.py --dataset longmemeval_s_cleaned.json \    │
│     --retrieval ranked.jsonl --out merged.json                      │
└────────────────────────────────────────────────────────────────────┘
┌── 3. generate hypotheses ──────────────────────────────────────────┐
│ python phase4_run.py generate --provider … --model … \             │
│     --dataset merged.json --out hyp.jsonl                           │
└────────────────────────────────────────────────────────────────────┘
┌── 4. judge + report ───────────────────────────────────────────────┐
│ python phase4_run.py judge --provider … --model … \                │
│     --dataset merged.json --hyp hyp.jsonl                           │
└────────────────────────────────────────────────────────────────────┘
```

## Setup

The generation prompt assembly and the yes/no grading templates are imported
**verbatim** from a LongMemEval checkout — they are not vendored here (to avoid
duplicating upstream code and drifting from it):

```bash
git clone https://github.com/xiaowu0162/LongMemEval
export LONGMEMEVAL_SRC="$PWD/LongMemEval/src"   # the src/ dir
python -m venv venv && ./venv/bin/pip install tiktoken   # + LongMemEval deps
```

`phase4_run.py` imports `prepare_prompt` (from `src/generation/run_generation.py`)
and `get_anscheck_prompt` (from `src/evaluation/evaluate_qa.py`). Pass the
checkout with `--longmemeval-src` or the `$LONGMEMEVAL_SRC` env var.

## Keys (never logged)

- `openai`  → `OPENAI_API_KEY`
- `anthropic` → `ANTHROPIC_API_KEY`, or reads `api.key` under `[api]` in
  `~/.config/ghost/config.yaml` only if the user still has a legacy config
  file; new installs have none (mainline Ghost no longer uses `api.*`).

The driver never prints a key and never dumps the request headers.

## OpenAI-compatible providers (DeepSeek, OpenCode, etc.)

Use `--provider openai --api-base-url <URL>` to point at any
OpenAI-compatible API. The driver sends the same `Authorization: Bearer`
header and chat-completions body; only the base URL changes.

Examples:

```bash
# DeepSeek V4 Pro via their API
python phase4_run.py generate --provider openai --model deepseek-v4-pro \
    --api-base-url https://api.deepseek.com \
    --dataset merged.json --out hyp.jsonl
python phase4_run.py judge --provider openai --model deepseek-v4-pro \
    --api-base-url https://api.deepseek.com \
    --dataset merged.json --hyp hyp.jsonl

# OpenCode Go (Zen)
python phase4_run.py generate --provider openai --model deepseek-v4-pro \
    --api-base-url https://opencode.ai/zen/go \
    --dataset merged.json --out hyp.jsonl
```

DeepSeek models put short answers in `reasoning_content` when `max_tokens`
is tight — the driver falls back to `reasoning_content` automatically.

## Fidelity — matches the official harness

- Generation: single user message, `temperature=0`, `max_tokens=500` (non-CoT),
  `max_retrieval_length = model_max_length − 500 − 1000`, prompt built by the
  official `prepare_prompt` (default `flat-session`, `topk_context=5`,
  `history_format=json`, `useronly=false`, `merge_key_expansion=none`).
- Judging: official `get_anscheck_prompt(task, question, answer, response,
  abstention)`, `temperature=0`, `max_tokens=10`, `label = "yes" in resp.lower()`.
  Abstention questions (`*_abs`) get the abstention template automatically.
- Aggregation: overall accuracy = mean of labels; per-`question_type` mean with
  counts — identical to `evaluate_qa.py`.
- Both stages are **append-only and resume-safe**: a crash at question N keeps
  the first N, and re-running skips already-done `question_id`s.
- Retries with backoff on 429 / 5xx / 529 (overloaded).

Truncation uses tiktoken `o200k_base` (gpt-4o's tokenizer). For Claude this is
approximate but conservative — Claude's 200k context comfortably exceeds the
126500-token budget, so nothing gold gets truncated that gpt-4o would have kept.

## Comparability

- **`openai` with gpt-4o generator + gpt-4o judge** → leaderboard-comparable
  (this is the official setup: `gpt-4o-2024-08-06`, `o200k_base`).
- **`anthropic` (Claude generator/judge)** → **NOT** leaderboard-comparable. It
  is an internal *"Ghost retrieval + Claude generation, Claude-judged"* check.
  When generator and judge are the same family, note the self-preference caveat
  (the official setup has the same property — gpt-4o judged gpt-4o); a different
  strong judge (e.g. gen `claude-sonnet-5`, judge `claude-opus-4-8`) costs only
  a few dollars extra because the judge emits ~10 tokens per question.

## Cost (no API calls)

```bash
python cost_estimate.py --dataset merged.json --gen-model claude-sonnet-5 \
    --judge-model claude-sonnet-5 --topk 5
```

Snapshot for the full 470 answerable questions at `topk_context=5`:

| gen / judge | generate | judge | total |
|---|---|---|---|
| gpt-4o / gpt-4o | ~$18.8 | ~$0.9 | **~$20** |
| claude-sonnet-5 / claude-sonnet-5 | ~$23.3 | ~$1.2 | **~$24** |
| claude-sonnet-5 / claude-opus-4-8 | ~$23.3 | ~$6.0 | **~$29** |
| claude-opus-4-8 / … (generator) | ~$116 | — | **avoid** |

Prices are a static snapshot in `cost_estimate.py` — verify current rates.
The 30 abstention (`*_abs`) questions are excluded (Ghost's retrieval scoring
excludes them); a full leaderboard e2e would add them back.

## Example — full 470, Anthropic Sonnet

```bash
export LONGMEMEVAL_SRC=/path/to/LongMemEval/src
python phase4_run.py generate --provider anthropic --model claude-sonnet-5 \
    --dataset merged.json --out hyp.sonnet.jsonl
python phase4_run.py judge --provider anthropic --model claude-sonnet-5 \
    --dataset merged.json --hyp hyp.sonnet.jsonl
```
