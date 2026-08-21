# Eval Cycle Harness — Design

## Goal

Ghost's staleness pipeline (`ghost supersede` → `ghost resolve` → `ghost reflect`) has unit tests and retrieval benchmarks, but nothing that exercises the full MCP cycle end-to-end against a seeded project and grades the *behavior*: did supersede link the right pairs, did resolve mark exactly the resolved-evidence memories, did reflect merge duplicates without dropping anything important. This harness runs that cycle on demand against a scratch-isolated database with a hand-authored, annotated corpus, and produces a graded scorecard plus a full misclassification listing for human/agent review.

Two constraints shape the design:

1. **Zero Anthropic API spend.** All LLM calls (classify + consolidate) run through the `opencode` CLI, billed to whatever provider opencode is configured with.
2. **Zero risk to production data.** Every run uses throwaway `XDG_DATA_HOME`/`XDG_CONFIG_HOME` scratch dirs — the isolation pattern proven by the 2026-07-27 real-world eval suite.

## Part 1: Enabler — opencode-backed classifiers (code change)

Today `buildClassifyProvider` (cmd/ghost/main.go) requires either `ANTHROPIC_API_KEY` or the `claude` binary on PATH; with neither, `ghost resolve`/`ghost supersede` exit with an error even when `opencode` is installed.

Change: when no API key is configured, fall through to `ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)` — claude first, opencode second — instead of erroring. When an API key *is* set, behavior is unchanged (API primary). The `ai.CLIProvider` already satisfies the `ai.Provider` interface the classifiers use and implements exactly this precedence, so this is wiring, not new machinery. `ghost reflect --tier opencode` (and auto-tier via `CLIProvider`) already work this way.

Unit tests cover: key+claude (unchanged), key only (unchanged), no-key+opencode (new: works), no-key+neither (errors).

## Part 2: Annotated corpus

`eval/cycle/corpus/` holds one JSONL file of memory specs for a fake project, `acme-migration` (~55 memories): a multi-week service-migration storyline that naturally produces decisions, gotchas, reversed choices, investigation notes, and stable conventions.

Every memory carries optional grading annotations alongside the standard save fields:

- `expected_superseded_by` — key of the newer memory that should supersede this one (used on the older member of each contradicted pair)
- `expected_resolved` — true for resolved-evidence memories (investigation notes, cost estimates, closed-experiment changelogs whose findings are captured elsewhere)
- `merge_group` — identifier grouping near-duplicates reflect should collapse to one survivor

Target composition:

| Group | Count | Purpose |
|---|---|---|
| Supersede pairs | ~16 (8 pairs) | contradicted early/late facts (deploy target, ports, credentials locations, tooling choices) |
| Resolved evidence | ~12 | concluded-work notes resolve should mark |
| Near-duplicate clusters | ~9 (3 clusters) | near-identical config facts reflect should merge |
| Stable distractors | ~18 | conventions/preferences/architecture/gotchas that must survive untouched |

Distractors are graded implicitly: anything annotated neither supersede-target nor resolved nor merged must still exist after the full cycle.

## Part 3: Runner

`eval/cycle/main.go`, a standalone main package mirroring the `bench/longmemeval` layout (`go run ./eval/cycle`). Phases:

1. **Isolate** — create `<tmp>/evalcycle-<runid>/{data,config}`; spawn every ghost process with `XDG_DATA_HOME=<scratch>/data`, `XDG_CONFIG_HOME=<scratch>/config`; seed a minimal `config/ghost/config.yaml` (no api.key, so classifiers take the CLI path). Remove the scratch dir at exit unless `--keep`.
2. **Inject** — drive a real `ghost mcp` subprocess over stdio using the `modelcontextprotocol/go-sdk` client; call `ghost_memory_save` once per corpus entry (the genuine save path, including embedding-worker notification); record returned memory IDs keyed by corpus key.
3. **Drain gate** — poll the scratch SQLite DB until `memory_embeddings` row count equals the injected memory count (supersede candidate generation needs vectors; requires local Ollama with nomic-embed-text up). Timeout → fatal with diagnostic hint.
4. **Stages**, each dry-run first, grade, then apply:
   - `ghost supersede acme-migration` — parse `  <id8>  supersedes  <id8>` lines; precision/recall vs `expected_superseded_by` annotations (direction-aware).
   - `ghost resolve acme-migration` — parse confirmed `<id8>` lines; P/R vs `expected_resolved`.
   - `ghost reflect acme-migration --tier opencode` — grade at set level from the final memory list fetched via `ghost_memories_list` on the still-open MCP session: per-merge-group collapse (exactly one survivor ≈ pass), survival of distractors, and dropped-important count. The single `ghost mcp` child stays alive from injection through this final listing so no second injection path exists. Reflect apply is behind a flag since it rewrites the memory set (reversible via `--restore` snapshot either way).
5. **Report** — dated Markdown scorecard (per-stage precision/recall/counts) written next to the corpus as `results/<date>-report.md`, followed by an itemized misclassification table (expected vs got, with content excerpts) for eyeball judgment.

Grading maps the 8-char ID prefixes printed by the CLI back to corpus keys via the IDs captured at injection.

## Judging

Metrics give the quantitative view; the intended workflow is a human or agent reading the misclassification table and attributing each miss (threshold too tight, prompt wording, classifier confusion, annotation error). That part is deliberately not automated.

## Out of scope / deferred follow-ups

- GitHub Actions `workflow_dispatch` wiring (needs opencode auth plumbed as a secret; local runs need none).
- A deterministic PR slice (injection integrity + pre-LLM cosine candidate generation).
- A model-pinning flag for the opencode subprocess (uses opencode's configured default model).
