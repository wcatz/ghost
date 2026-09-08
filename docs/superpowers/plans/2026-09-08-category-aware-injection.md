# Category-Aware Session Injection Selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bias the `SessionStart` hook's 15-slot injection budget toward high-signal, hard-to-derive categories (`gotcha`/`convention`/`preference`/`decision`) without growing the token footprint, and shrink per-memory formatting to a leaner `[category] «content»` shape. Net effect: same or smaller footprint, higher utility per token.

**Spec:** `docs/superpowers/specs/2026-09-08-category-aware-injection-design.md`

**Decided parameters (2026-09-08 sign-off):**
- `behavior_floor` default = **8** (of 15)
- compact formatting: shipped **unconditional**
- MCP resource path: **doc-only** (no behavior change)

**Architecture:** All behavior changes are confined to `internal/mcpinit/hook.go` (add a category-priority two-pass selection replacing the current single-pass in `loadSessionContext`, and a compact render in `formatSessionContext`). Config surface added to `internal/config/config.go` + `config.example.yaml` + defaults. Tests in `internal/mcpinit/hook_test.go`. No schema changes, no new dependencies.

---

## Setup

- [ ] **Step 0: Isolated worktree**

```bash
cd /home/wayne/git/ghost
git worktree add .worktrees/category-aware-injection -b feat/category-aware-injection
cd .worktrees/category-aware-injection
go build ./... && go test ./internal/mcpinit/... ./internal/memory/... ./internal/config/...
```

Expected: build succeeds, existing tests pass (clean baseline).

---

### Task 1: Add `injection` config block

**Files:**
- Modify: `internal/config/config.go` (struct + defaults)
- Modify: `internal/config/config.example.yaml`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestInjectionConfigDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Injection.BehaviorFloor != 8 {
		t.Errorf("injection.behavior_floor = %d, want 8", cfg.Injection.BehaviorFloor)
	}
	want := map[string]bool{"gotcha": true, "convention": true, "preference": true, "decision": true}
	if len(cfg.Injection.BehaviorCategories) != len(want) {
		t.Fatalf("behavior_categories = %v, want 4 entries", cfg.Injection.BehaviorCategories)
	}
	for _, c := range cfg.Injection.BehaviorCategories {
		if !want[c] {
			t.Errorf("unexpected behavior category %q", c)
		}
	}
	if cfg.Injection.CategoryWeights != nil {
		t.Errorf("category_weights default should be nil, got %v", cfg.Injection.CategoryWeights)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/... -run TestInjectionConfigDefaults -v`
Expected: FAIL — `cfg.Injection` field doesn't exist yet (compile error) or zero values.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to the `Config` struct (after `Linking`):

```go
	Injection  InjectionConfig  `koanf:"injection"`
```

And the struct + defaults:

```go
// InjectionConfig controls how the SessionStart hook selects which project
// memories to inject. It biases the limited slot budget toward high-signal,
// hard-to-derive categories (gotcha/convention/preference/decision) without
// growing the total footprint. behavior_floor of 0 disables the bias entirely
// (pure DecayRankingSQL selection, the historical behavior).
type InjectionConfig struct {
	BehaviorFloor      int                `koanf:"behavior_floor"`
	BehaviorCategories []string           `koanf:"behavior_categories"`
	CategoryWeights    map[string]float64 `koanf:"category_weights"`
}
```

In `defaults` map:

```go
	"injection.behavior_floor":      8,
	"injection.behavior_categories": []string{"gotcha", "convention", "preference", "decision"},
	// category_weights intentionally absent — nil means "all 1.0"
```

- [ ] **Step 4: Add to config.example.yaml**

Under a new `injection:` block, document the keys. Match the existing example style (annotated defaults).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: all PASS including `TestInjectionConfigDefaults` and existing tests (no defaults displaced).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config.example.yaml internal/config/config_test.go
git commit -m "feat(config): add injection config block (behavior_floor, behavior_categories, category_weights)"
```

---

### Task 2: Two-pass category-priority selection in `loadSessionContext`

**Files:**
- Modify: `internal/mcpinit/hook.go` (the memory query + cap block in `loadSessionContext`, ~lines 441-485)
- Test: `internal/mcpinit/hook_test.go`

**Reference (read-only):** `internal/memory/store.go:749-767` `GetTopMemories` and `internal/memory/store.go:735` `DecayRankingSQL`.

- [ ] **Step 1: Write the failing tests**

```go
// TestInjection_RespectsBehaviorFloor: ≥9 behavioral + ≥6 descriptive
// memories -> at least behavior_floor (8) behavioral injected, rest filled
// from descriptive, total ≤ sessionMemoriesCap (15).
func TestInjection_RespectsBehaviorFloor(t *testing.T) {
	// fixture: project p1 with 10 gotcha + 10 architecture memories,
	// all same importance so decay is the only discriminator.
	// Expect >= 8 gotcha in output, total == 15.
}

// TestInjection_SparseBehaviorals: only 3 gotcha memories -> those 3 get in,
// remaining 12 filled from architecture/fact (floor is a max, not over-inject).
// Expect exactly 3 gotcha, 12 total, no phantom category-invention.
func TestInjection_SparseBehaviorals(t *testing.T) { /* ... */ }

// TestInjection_FloorZero_MatchesLegacy: behavior_floor=0 -> selection equals
// current behavior (rank-only by DecayRankingSQL). Regression parity pin.
func TestInjection_FloorZero_MatchesLegacy(t *testing.T) { /* ... */ }
```

Use the existing `HandleSessionStartHook` fixture pattern from `TestSessionInjectionRespectsNewCap` (xdg temp dir, `memory.OpenDB`, insert project + memories, `json.Marshal` cwd, parse output, count occurrences of category labels in the `**Memories (...):**` block).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcpinit/... -run 'TestInjection_(RespectsBehaviorFloor|SparseBehaviorals|FloorZero_MatchesLegacy)' -v`
Expected: FAIL — current single-pass rank-only selection won't guarantee the behavioral floor.

- [ ] **Step 3: Implement the two-pass selection**

In `internal/mcpinit/hook.go`, inside `loadSessionContext`, load the config once and replace the single-pass query block with a two-pass selection. Fetch an over-fetch set (e.g. `sessionMemoriesCap*3`) so both passes have enough candidates:

```go
	cfg, _ := config.Load()
	floor := sessionMemoriesCap
	if cfg != nil {
		floor = cfg.Injection.BehaviorFloor
	}
	if floor < 0 {
		floor = 0
	}
	if floor > sessionMemoriesCap {
		floor = sessionMemoriesCap
	}
	behav cats := cfg.Injection.BehaviorCategories (or default 4)
	weights := cfg.Injection.CategoryWeights

	// Pass 1: behavioral memories, ordered by DecayRankingSQL * category_weight.
	rows, err := db.Query(`
		SELECT id, category, content, pinned FROM memories
		WHERE project_id = ? AND resolved_at IS NULL
		ORDER BY (`+memory.DecayRankingSQL+`) DESC
		LIMIT ?
	`, projectID, sessionMemoriesCap*3)
	// ...scan all into a slice of sessionMemory (with Category)...

	// Partition: behavioral vs rest (by behaviorCategories set).
	// Rank both by (DecayRankingSQL * weight). Weight = weights[cat] or 1.0.
	// Compute DecayRankingSQL score in Go for ordering within each partition
	// (reuse the formula — see store.go decayFactor / the Go mirror).

	// Select: take min(floor, len(behavioral)) behavioral highest-ranked, then
	// fill from rest (excluding chosen IDs) up to sessionMemoriesCap.
	// Preserve stable order.
```

**Important:** the existing code computes SQL-side ordering; for the weighted pass-1 ordering you'll need the score in Go. **Reuse the existing Go mirror** `decayFactor` in `internal/memory/vector.go:250` (it mirrors `DecayRankingSQL` exactly — do not duplicate the formula). Multiply by the per-category weight.

Then apply the **existing** demotion + truncation unchanged (they already run on the merged `memories` slice — keep `DemotionPenalties`/`StableDemote`/`memories[:sessionMemoriesCap]` as-is after the two passes produce the merged list).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mcpinit/... -run 'TestInjection_(RespectsBehaviorFloor|SparseBehaviorals|FloorZero_MatchesLegacy)' -v` then the full `go test ./internal/mcpinit/... -v`
Expected: all PASS (and existing cap/backfill tests still green).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpinit/hook.go internal/mcpinit/hook_test.go
git commit -m "feat(mcpinit): two-pass category-priority injection selection"
```

---

### Task 3: Compact injection formatting (drop memory ID)

**Files:**
- Modify: `internal/mcpinit/hook.go` (`formatSessionContext`, line 223-225)
- Modify: `internal/mcpinit/hook_test.go` (assertions that grep for the 32-hex ID / old shape)
- Check: opencode plugin context file expectations (`internal/mcpinit/opencode_ghost.ts` embeds the render — verify no downstream parser depends on the ID)

- [ ] **Step 1: Write the failing test**

```go
// TestFormatSessionContext_CompactMemoryLine: injected memory lines are
// "- [category] «content»" with no backticked 32-hex ID and no importance/
// tags prefix.
func TestFormatSessionContext_CompactMemoryLine(t *testing.T) { /* ... */ }
```

Also add a negative guard: output must NOT contain a regex like `[0-9A-F]{32}` in the memories section.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpinit/... -run TestFormatSessionContext_CompactMemoryLine -v`
Expected: FAIL — current line is `- [category] \`ID\` «content»`.

- [ ] **Step 3: Implement**

In `formatSessionContext`, change the memory rendering block:

```go
	// Compact form: category + content only. The 32-hex memory ID and any
	// importance/tags prefix are dropped — they're near-unused by the model
	// in the injected block (the interactive ghost_project_context / search
	// tools surface the ID when needed) and cost ~35-60 bytes per memory.
	// The «...» delimiters are retained as the indirect-injection defense.
	for _, m := range memories {
		fmt.Fprintf(&sb, "- [%s] %s\n", m.Category, quoteData(m.Content))
	}
```

(Leave the globals and tasks/decisions rendering unchanged.)

- [ ] **Step 4: Update existing tests that assert the old shape**

Search `internal/mcpinit/hook_test.go` for assertions on the memories block format (e.g. any that match a backticked ID after `[category]`). Update to the compact form.

- [ ] **Step 5: Check opencode plugin expectations**

Inspect `internal/mcpinit/opencode_ghost.ts` — confirm it just surfaces the opaque context string (does not regex-parse memory IDs). If it does parse IDs, it must be updated in the same change. Verify with a grep for `[0-9A-F]{32}` / `ghost_` / backtick-ID patterns in the .ts.

- [ ] **Step 6: Run full hook tests**

Run: `go test ./internal/mcpinit/... -v`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/mcpinit/hook.go internal/mcpinit/hook_test.go internal/mcpinit/opencode_ghost.ts
git commit -m "feat(mcpinit): compact injected memory lines (drop 32-hex ID, keep category + content)"
```

---

### Task 4: MCP resource doc wording (doc-only)

**Files:**
- Modify: `internal/mcpserver/mcpserver.go` (the `ghost_project_context` tool description, ~line 581)
- Modify: `docs/architecture.md` (data-flow / injection section)

- [ ] **Step 1: Update the tool description**

The `ghost_project_context` description already says "NOT needed at session start (hook already injects this)". Strengthen it to make the distinction explicit:

```
"Get Ghost's accumulated knowledge about a project: the full top-20 memories (with IDs, pins, tags) plus learned context — the COMPLETE detail surface. The SessionStart hook injects a condensed 15-memory selection without IDs; use this when you need the full set, IDs, or pins, or after switching projects mid-session."
```

- [ ] **Step 2: Update architecture.md**

In the "Data Flow → MCP Server" section, add a one-line note distinguishing the hook's condensed selection from the resource's complete payload.

- [ ] **Step 3: Verify no tests assert the old description**

Run: `go test ./internal/mcpserver/... ./internal/mcpinit/... -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/mcpserver/mcpserver.go docs/architecture.md
git commit -m "docs(mcpserver): clarify hook (condensed) vs resource (complete) injection surfaces"
```

---

### Task 5: Bench / evals

**Files:**
- New or modify: `internal/bench/` injection-budget suite (or extend eval-cycle harness)

- [ ] **Step 1: Add an injection-budget bench suite**

Measure bytes of the rendered injection block over the real ghost-project corpus (58 memories) before/after, asserting:
- total bytes not greater than the legacy render over the same corpus;
- `behavior_floor` satisfied (≥8 behavioral when available) for a representative corpus.

Reuse the existing `internal/bench` grading style (see `runner.go`/`staleness.go`). Wire a `ghost bench` flag (e.g. `--injection`) or a standalone `TestBenchInjectionBudget`.

- [ ] **Step 2: Run and record results**

Run the bench; record numbers in `docs/benchmarks.md` (bytes before vs after, behavioral hit-rate). This is the evidence that the trade is a net win, matching the `docs/benchmarks.md` convention for retrieval quality.

- [ ] **Step 3: Commit**

```bash
git add internal/bench/ docs/benchmarks.md
git commit -m "feat(bench): injection-budget suite proving category-priority + compact render"
```

---

### Task 6: Full regression pass and vet

**Files:** none (verification-only)

- [ ] **Step 1: go vet**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 2: Full test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 3: Manual smoke test in this repo**

```bash
go run ./cmd/ghost hook session-start <<< '{"cwd": "'"$(pwd)"'", "source": "startup"}' | head -c 2000
```

Expected: `## Ghost context: ghost`, ≤15 memories in compact `[category] «...»` form (no backticked 32-hex IDs), ≥8 gotcha/convention/preference/decision if the project has them, ≤8 globals, still-dropped duplicate content.

```bash
go run ./cmd/ghost hook session-start <<< '{"cwd": "'"$(pwd)"'", "source": "compact"}'
```
Expected: one-line pointer (unchanged from 08-03 work).

```bash
# Verify floor=0 reproduces legacy selection
ghost hook session-start <<< '{"cwd": "'"$(pwd)"'", "source": "startup"}' \
  > /tmp/floor8.txt
GHOST_INJECTION_FLOOR=0 ... compare (via a config file with behavior_floor: 0)
```
Expected: floor=0 output matches the pre-change behavior ordering.

---

## Execution Handoff

Plan complete at `docs/superpowers/plans/2026-09-08-category-aware-injection.md`. Spec at `docs/superpowers/specs/2026-09-08-category-aware-injection-design.md`.

**Execution options:**
1. **Subagent-driven (recommended)** — dispatch a fresh subagent per task, review between tasks.
2. **Inline execution** — run tasks in-session with checkpoints.

After implementation, use `superpowers:finishing-a-development-branch` to merge/PR — never push directly to `main`.
