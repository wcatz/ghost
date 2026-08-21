# Eval Cycle Findings — First Judged Run

**Date:** 2026-08-21 · **Branch:** `eval-cycle-harness` · **Harness:** `eval/cycle` (spec: `docs/superpowers/specs/2026-08-21-eval-cycle-harness-design.md`, plan: `docs/superpowers/plans/2026-08-21-eval-cycle-harness.md`)

## What ran

`GHOST_OPENCODE_MODEL=opencode/deepseek-v4-flash go run ./eval/cycle --keep`

1. Scratch-isolated ghost (`XDG_DATA_HOME`/`XDG_CONFIG_HOME` in a temp dir, `ANTHROPIC_API_KEY` stripped) — production DB untouched, zero Anthropic spend.
2. 55-memory annotated corpus (`acme-migration`) injected through a real `ghost mcp` stdio session (`ghost_memory_save` only), chronology restamped to corpus order (1 min/memory).
3. Embedding drain gate: 55/55 via local Ollama (nomic-embed-text).
4. `ghost supersede --apply`, `ghost resolve --apply`, `ghost reflect --tier opencode --apply` — all classify/consolidation calls rode the claude→opencode CLI path on deepseek-v4-flash.

Four harness iterations were needed to reach a trustworthy grade: uppercase-id parsing, corpus pairs rewritten from "contradictions" (cosine 0.70–0.74, below supersede's 0.80 same-fact threshold) to minimal-edit paraphrases, chronology restamping, and grader tokenization mirroring `memory.tokenizeContent`.

## Scorecard (final run)

| Stage | Precision | Recall | Detail |
|---|---|---|---|
| Supersede | 0.71 | 0.62 | TP=5 FP=2 FN=3; 11 candidate pairs at cosine ≥0.80 |
| Resolve | 1.00 | 0.50 | TP=6 FP=0 FN=6; keyword prefilter kept 10/45 |
| Reflect (set-level) | — | — | 55 → 27 memories; 2/3 merge groups collapsed cleanly |

The two supersede FPs are **intra-cluster links** (e.g. `redis_maxmemory_a` "superseded" by `_c`): semantically defensible dedup, an annotation-scheme gap rather than misbehavior.

## Findings

### F1 — Production bug: Upsert duplicate-strengthening corrupts supersession direction (HIGH) — FIXED in this PR

When a newer paraphrase saves and triggers Upsert's duplicate detection, the strengthening path bumped the **older** row's `updated_at = datetime('now')` (internal/memory/store.go:653). Supersede's candidate generation derives newer/older from timestamps, so a later genuine update could be linked **backwards** (old supersedes new). Reproduced twice before the harness's restamp workaround. **Fixed here**: strengthen no longer touches `updated_at` (regression test `TestStoreUpsert_StrengthenPreservesUpdatedAt`; closes #335). The harness restamp remains for second-granularity burst-injection ties.

### F2 — Resolve prefilter recall gap (MEDIUM)

The keyword prefilter drops evidence phrasings like "cost estimate", "postmortem", "closed spike", "downtime window" before the LLM sees them (10/45 kept). The classifier itself was perfect on everything it reached — P=1.00 in both runs; every miss was prefilter-caused. Vocabulary expansion or embedding-assisted candidacy is the fix direction.

### F3 — Reflect consolidation silently deletes actionable facts (HIGH)

With deepseek-v4-flash consolidation, three clusters of real value were deleted outright rather than merged:
- `bastion_ssh_port` trio (operational access fact — port 2222 appears nowhere post-run)
- `gotcha_docker_tty` (actionable pitfall)
- `pref_short_prs`, `pref_rebase_no_merge` (workflow preferences), plus `dep_caddy28` version pin

Meanwhile the merges that did fire were high quality: NATS-rejection evidence folded into the queue fact, the conventions quartet merged losslessly into one PR-workflow memory, staging trio collapsed to one line, and all eight superseded old members vanished while every newer member survived — exactly right. The risk is asymmetric: merges good, deletions unguarded. Suggest a consolidator prompt guard ("never drop operational config/gotchas/preferences without folding them into a survivor") and/or tier quality gating.

### F4 — Supersede classifier conservatism (LOW)

3 of 8 true minimal-edit updates classified "neither". KEEP-biased philosophy makes this tolerable; recall improves if candidates carry their similarity scores into the prompt.

### F5 — Run-to-run LLM nondeterminism (INFO)

Resolve's confirmed set wobbles ±1 across identical inputs; supersede link sets varied more before the corpus rewrite. Grading must use floors, never exact-match expectations.

### F6 — Harness learnings (baked in)

Chronology restamping after injection; grader tokenization mirrors ghost's own; raw stage outputs persisted next to reports; `ids.json` per scratch dir; `--grade-only` re-grading mode.

## Corrected reflect judgment (final-state audit)

Surviving 27 rows audited by hand against the corpus: 8/8 supersede *new* facts present, 0 old facts lingering; resolved-evidence rows preserved untouched by design; staging + redis groups collapsed to single survivors; conventions quartet merged losslessly. Genuine losses are exactly F3's list.

## Follow-ups

1. Fix Upsert `updated_at` refresh on duplicate-strengthening (+ regression test: two near-dup saves → assert supersede direction) — **filed as task**.
2. Resolve prefilter vocabulary/embedding-assist expansion.
3. Consolidator no-delete guard for config/gotcha/preference categories.
4. CI `workflow_dispatch` wiring for the harness (needs opencode auth-as-secret).

## PR validation run (same day, post-review fixes)

Re-run on the final branch state (`GHOST_OPENCODE_MODEL=opencode/deepseek-v4-flash`, kept scratch audited by hand):

| Stage | Precision | Recall | Detail |
|---|---|---|---|
| Supersede | 0.83 | 0.62 | TP=5 FP=1 FN=3; sole FP is an intra-cluster link; direction stable |
| Resolve | 1.00 | 0.42–0.67 | P perfect every run; R wobbles with prefilter + classifier variance |
| Reflect | — | — | 55 → 28; **all 3 merge groups collapsed**; hand-audited real loss: 1 preference |

Hand audit of the 7 distractor flags: 6 were lossless merges (conventions quartet → one PR-workflow memory; redis/caddy/restic pins → one versions memory; postgres pin folded into the db-host fact); `pref_rebase_no_merge` survived verbatim. Genuine deletion: `pref_short_prs` alone. Reflect quality is materially better than the first judged run — consolidation nondeterminism cuts both ways, which strengthens F3's recommendation for a no-delete guard rather than model tuning.

Known grader limitation: set-level survival heuristics under-count rewritten/merged content even with ghost-mirrored tokenization; final-state hand audits remain the source of truth for reflect.
