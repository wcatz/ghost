# Category-Aware Session Injection Selection Design

> **Status:** Proposed — not yet implemented.
> **Related prior work:** `2026-08-03-hook-injection-cost-reduction` (landed) and `2026-08-03-subagent-injection-gating-design` (landed), `2026-08-20-search-time-decay-design` (landed). This feature builds on those but is **new** — none of the prior work re-weights injection *selection* by category; it ranks purely by composite decayed score.

## Problem

Ghost's `SessionStart` hook injects the top project memories into every session's system prompt. Today the selection is purely rank-by-`DecayRankingSQL` (importance × category-aware time-decay × pin-exemption) — it does **not** prefer one category over another beyond what decay already encodes.

The measured/estimated footprint is ~2–3.5k tokens for 15 project memories + 8 globals + tasks + decisions (`docs/superpowers/plans/2026-08-03-hook-injection-cost-reduction.md`, memory `CAAA0E9DCDBF53524BEEBF56240D0A99`). That overhead is justified **only if the slots are spent on high-signal content**.

The current mix spends several slots on low-value-for-injection memories:

- **Descriptive** `architecture` / `fact` memories (project layout, god-package sizes, "this component is 1706 LOC") that the model can reconstruct by reading source — near-zero marginal value in a session prompt, they are *derivable*.
- **Behavioral / experiential** `gotcha` / `convention` / `preference` / `decision` memories (SOPS workflow, mr-slave is the build host, DCO sign-off, blobs-deadlock tel the same story) that are *irreplaceable* — no amount of source reading recovers them, and re-discovering them costs turns or causes incidents.

A `gotcha` and an `architecture` memory at the same decayed score currently get equal injection airtime even though the gotcha is almost always worth more per token for agentic coding.

## Goal

Make the session-injection selection **category-aware**: bias the injected set toward the high-signal, hard-to-derive categories (`gotcha`, `convention`, `preference`, `decision`) while still retaining the best of the decaying, descriptive categories — without growing the token budget.

Combined with the formatting-optimization subordinate goal (option (c) both), the net effect is **same or smaller footprint, higher utility per token**.

## Non-Goals

- **Not** a full replacement of `DecayRankingSQL`. Search (`ghost_memory_search`) and `GetTopMemories` keep their current ranking semantics.
- **Not** removing memory categories.
- **Not** a per-user UI; knobs are config-file flags only.
- **Not** touching the MCP resource path's parity contract beyond documenting it (the resource is a separate, distinct payload — see Scope).

## Current State (verified in code, 2026-09-08)

- `internal/mcpinit/hook.go`:
  - `sessionMemoriesCap = 15` (line 308), `globalsCap = 8` (line 296)
  - Project ranking uses `memory.DecayRankingSQL` (shared constant) `ORDER BY (...) DESC LIMIT cap*2`, then near-duplicate demotion via `memory.DemotionPenalties` + `memory.StableDemote`, then `memories[:sessionMemoriesCap]`.
  - Globals ranked by `pinned DESC, importance DESC, updated_at DESC`, capped at 8, with `globalsDemotionThreshold = 0.85`.
  - Content truncated: project at 200 bytes, globals at 300 bytes (`truncateUTF8`).
- `internal/memory/store.go`:
  - `DecayRankingSQL` (line 735) — the shared composite-score fragment.
  - `GetTopMemories` (line 749) — the MCP-tool path, `WHERE project_id=? OR project_id='_global'`, rank-only.
- Two distinct injection paths exist:
  1. **Hook** (`loadSessionContext` → `formatSessionContext`): 15 project + 8 globals + tasks + decisions + learned.
  2. **MCP resource** (`ghost://project/{id}/context`, `mcpserver.go:buildProjectContext`): 20 project + 15 globals + decisions + learned. Not automatically injected; only read if the client pins/lists it.

## Design

### Core idea: category-priority blending

Introduce a **category weight** applied to the *injection-selection* ranking (NOT to the search/`GetTopMemories` path) so the hook's 15-slot budget is spent to satisfy two goals in order:

1. **Hard floor for high-signal categories** — reserve a minimum number of slots for `gotcha` / `convention` / `preference` / `decision` if the project has them.
2. **Decayed-score fill** — the remaining slots are filled by the current `DecayRankingSQL` ranking so the best *of the rest* (including descriptive `architecture`/`fact`) still gets in.

This is a **two-pass selection**, not a wholesale re-rank:

- **Pass 1 (prioritized):** take the top `categoryQuota` by `DecayRankingSQL` from categories in `{gotcha, convention, preference, decision}`.
- **Pass 2 (fill):** fill up to `sessionMemoriesCap - len(pass1)` from `DecayRankingSQL` overall, excluding the already-chosen IDs.
- Apply the existing near-duplicate demotion to the merged result, then truncate in-place (same post-processing as today).

**Rationale for two-pass over a single weighted formula:** a single score like `score * categoryBoost` is hard to reason about and can be gamed by importance inflation; the two-pass model makes the guarantee explicit and testable ("at most N descriptive slots, at least M behavioral slots when available"). It also cleanly bounds behavior even when categories are sparse.

### Config surface

Add an `injection` config block (new) with sane defaults, keeping behavior identical to today when unset (backwards compatible):

```yaml
injection:
  # Slots reserved for high-signal, hard-to-derive categories.
  # 0 disables the two-pass behavior (pure DecayRankingSQL, current behavior).
  behavior_floor: 8        # 0..sessionMemoriesCap; default 8 of 15
  behavior_categories:     # what counts as "behavioral"
    - gotcha
    - convention
    - preference
    - decision
  # Optional per-category boost for the pass-1 ordering (default all 1.0).
  category_weights: {}     # e.g. {gotcha: 1.0, convention: 0.9, ...}
```

- `behavior_floor` default: 8 (just over half of 15; matches the observation that behavioral categories dominate value).
- `behavior_floor: 0` → exactly today's behavior (the two-pass degenerates to a single pass).
- `category_weights` multiplies the `DecayRankingSQL` score *only within* Pass 1 (ordering preference among behavioral categories); omitted categories get 1.0.

Config struct (`internal/config/config.go`):

```go
// InjectionConfig controls how the SessionStart hook selects which project
// memories to inject. It biases the limited slot budget toward high-signal,
// hard-to-derive categories (gotcha/convention/preference/decision) without
// growing the total footprint.
type InjectionConfig struct {
    BehaviorFloor     int               `koanf:"behavior_floor"`
    BehaviorCategories []string         `koanf:"behavior_categories"`
    CategoryWeights   map[string]float64`koanf:"category_weights"`
}
```

Add `Injection InjectionConfig \`koanf:"injection"\`` to `Config`, defaults in `defaults`, and document in `config.example.yaml`.

### Formatting optimization

- The per-memory formatting in `formatSessionContext` is ~40–60 bytes of tokens (`[category]` `` `ID` `` `(importance[pin][tags])` + `«...»`).
- **Keep** the `«...»` data delimiters (mandatory — indirect-injection defense, `quoteData`).
- **Drop** the full 32-hex ID from the injected list (keep it only in debug/verbose). The ID adds ~35 bytes but is nearly never used by the model in the injected block; when it needs the ID it calls `ghost_memory_search`/`ghost_project_context`. Replace with a compact `[category]` label only. (Note: the MCP *resource* path keeps IDs, since that's the interactive query surface.)
- **Drop** the `(importance …)` and `tags:` prefixes from the injected list; category is the only discriminator the model needs up front.

Net effect per memory: ~45–60 bytes saved → ~700–900 bytes (~200–250 tokens) shaved off the typical 15-memory injection, on top of the retained gains from the 08-03 plan.

### Resource/hook parity

- The MCP resource (`ghost://project/{id}/context`, 20+15) remains the "full detail" surface and is **unchanged** — it already serves IDs, pins, tags, learned context for pinned/on-demand reads.
- Document in both the hook's tool description and the resource's description that the hook is a *condensed* selection and the resource is the *complete* one, so a client that pins the resource still gets full fidelity. No code change needed to the resource; only doc wording.

### Open tasks and decisions are unchanged

Task and decision injection already favor `active`/`pending`/`blocked` (open) items; this feature does not alter them.

## Backwards Compatibility

- `behavior_floor: 0` (or an absent `injection` block) reproduces today's exact selection. The change is additive/config-gated.
- The formatting shrink (`ID`/`importance`/`tags` removal) changes the *shape* of the injected block unconditionally. This is a deliberate behavior change with a test to lock it; flag it in the plan/PR so downstream parsers (the opencode plugin's context file) are checked.
- Existing `hook_test.go` assertions that grep for exact memory IDs / `(importance` in injected output must be updated in the same change.

## Testing / Bench

- **Unit tests** (`internal/mcpinit/hook_test.go`):
  - `TestInjection_RespectsBehaviorFloor`: a project with ≥9 behavioral + ≥6 descriptive memories injects ≥ `behavior_floor` behavioral and fills the rest; total ≤ 15.
  - `TestInjection_SparseBehaviorals`: a project with only 3 behavioral memories still fills the other 12 slots from descriptive — floor is a *max* of available, not a hard over-inject.
  - `TestInjection_FloorZero_MatchesLegacy`: with `behavior_floor: 0`, selection equals the old behavior (regression/parity pin).
  - `TestInjection_FormattingDropIdAndImportance`: injected lines are `- [category] «…»` with no `(importance` and no full 32-hex ID.
- **Bench / evals:** extend `internal/bench` with an injection-budget suite (or reuse the eval-cycle harness) asserting that a large real-ish corpus (like the ghost project's own 58 memories) yields a focused, category-balanced injection and measurably fewer total bytes than the current formatting. This mirrors how `docs/benchmarks.md` documents retrieval quality.

## Risks / Open Questions

- **Is category always a good proxy for value?** A stale/wrong `gotcha` injected at high weight is worse than a current `architecture` note. Mitigation: `behavior_floor` is bounded (not 100%), decay still applies within Pass 1, and the existing near-duplicate demotion + `resolve`-marking surfaces stale behavioral memories too. Accept for v1; revisit with a data-driven study if needed.
- **Floor vs. availability semantics:** floor is a *cap on behaviorals that get injected by default*, and pass 2 fills the remainder — it never invents behavioral slots when none exist. Confirm this reading in review.
- **Config naming:** `behavior_floor` / `behavior_categories` are novel keys. Confirm they read well and match Ghost's existing config vocabulary (`linking.demotion_threshold`, etc.).

## Decision Points to Confirm Before Implementation

1. Default `behavior_floor` (proposal: 8) — or start conservative at 0 (opt-in) and validate the value via bench before enabling by default.
2. The formatting shrink — accept unconditional, or gate behind `injection.compact_format: true` default-on for the opencode plugin only initially?
3. Whether to touch the MCP resource path at all (proposal: doc-only, no behavior change).

## Out of Scope (tracked separately)

- **Two non-syncing Ghost DBs** (laptop vs mr-slave) — separate feature, not part of injection selection.
- **Content truncation cap (2000 chars) loss** — separate memory-fidelity fix, unrelated to selection.
- **Reflection cross-contamination** — separate reflection-quality work.
