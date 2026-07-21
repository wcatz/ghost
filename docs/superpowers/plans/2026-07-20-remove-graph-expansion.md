# Remove Graph-Expansion Ranking Bonus Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the disabled link-graph expansion ranking bonus and its bench ablation, leaving a short rationale in its place, while keeping the link graph itself (Obsidian export + supersedes depend on it).

**Architecture:** This is a *removal* plan, not a feature. There are no new failing tests to write; the discipline is to keep the build and the existing suite green at every commit. Order matters: remove the consumers (bench ablation, then the searcher's graph pass) before the definitions (`GraphNeighbors`), so each commit compiles. The link graph, `memory_links`, the linking worker, and the `supersedes` machinery are explicitly out of scope and must remain untouched.

**Tech Stack:** Go 1.26, `go build ./...`, `go vet ./...`, `go test ./...`. No new dependencies.

**Standing constraints:** feature branch + PR only; no `Co-Authored-By`/AI attribution in commits; `go vet ./...` before every commit.

---

## File Structure

| File | Change |
|---|---|
| `internal/bench/runner.go` | Modify — drop `CondHybridGraph`, `candidateGraphWeight`, the graph-build ablation, and now-unused imports/`discard` type |
| `internal/bench/sweep.go` | Modify — reduce `SweepGrid` to the vec-weight axis only; drop the `graph=` label |
| `internal/bench/runner_test.go` | Modify — drop `CondHybridGraph` from expected conditions |
| `internal/bench/regression_test.go` | Modify — remove the tracked hybrid+graph note block |
| `internal/bench/sweep_test.go` | Modify — rework `TestSweep`/`TestSweepGrid` for a graph-free grid; drop unused imports |
| `internal/bench/graph_test.go` | Delete — it exists only to self-verify the graph ablation |
| `internal/memory/vector.go` | Modify — remove `Graph*` params, `applyGraphBonus`, the `fuseAndRank` graph block + `projectID` param, and graph prose |
| `internal/memory/links.go` | Modify — remove `GraphNeighbor` struct and `GraphNeighbors` method |
| `docs/benchmarks.md` | Modify — replace the hybrid+graph ablation/sweep-axis text with an "evaluated and removed" note |
| `docs/architecture.md` | Modify — drop graph from the bench-conditions line, the pipeline diagram, and the links.go description |
| `CLAUDE.md` | Modify — rewrite the "graph-expansion bonus ships disabled" line to "evaluated and removed" |

---

## Task 1: Remove the graph ablation from the bench harness

**Files:**
- Modify: `internal/bench/runner.go`
- Modify: `internal/bench/sweep.go`
- Modify: `internal/bench/runner_test.go`
- Modify: `internal/bench/regression_test.go`
- Modify: `internal/bench/sweep_test.go`
- Delete: `internal/bench/graph_test.go`

Doing bench first means the searcher's graph params have no bench consumer when Task 2 deletes them.

- [ ] **Step 1: Trim `runner.go` — condition constants**

In `internal/bench/runner.go`, remove the `CondHybridGraph` line from the const block:

```go
// Condition names, stable for reporting.
const (
	CondFTS    = "fts-only"
	CondVector = "vector-only"
	CondHybrid = "hybrid"
)
```

Then delete the `candidateGraphWeight` const and its comment entirely (the whole block from `// candidateGraphWeight is the graph-bonus weight...` through `const candidateGraphWeight = 0.15`).

- [ ] **Step 2: Trim `runner.go` — the `Run` function body**

Replace the `Run` doc comment and the graph-ablation tail. The function now runs three conditions. Replace from the `// Run evaluates all four ablations...` comment through the `return` statement with:

```go
// Run evaluates the fts, vector, and hybrid ablations over the seeded store
// and query set. linkThreshold is retained in the signature for call-site
// stability but is no longer used (the graph ablation it fed was removed).
func Run(ctx context.Context, store *memory.Store, queries []Query, linkThreshold float32) ([]Result, error) {
	_ = linkThreshold
	fts, err := runCondition(ctx, CondFTS, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchFTS(ctx, q.ProjectID, q.Text, scoreK))
	})
	if err != nil {
		return nil, err
	}
	vec, err := runCondition(ctx, CondVector, queries, func(q Query) ([]string, error) {
		return idsFromScored(store.SearchVector(ctx, q.ProjectID, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}
	hybrid, err := runCondition(ctx, CondHybrid, queries, func(q Query) ([]string, error) {
		return idsFromMemories(store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK))
	})
	if err != nil {
		return nil, err
	}

	return []Result{fts, vec, hybrid}, nil
}
```

Note: keep `linkThreshold` in the signature (callers in `main.go` and tests pass it) but discard it with `_ = linkThreshold` so the build stays green without touching call sites.

- [ ] **Step 3: Trim `runner.go` — unused imports and the `discard` type**

The graph ablation was the only user of the `linking` worker, `slog`, `time`, and the `discard` writer. Delete the `discard` type and its `Write` method at the bottom of the file:

```go
// (delete these lines)
// discard is an io.Writer that drops the linking worker's log output during a
// benchmark run.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
```

Then update the import block to drop `log/slog`, `time`, and `github.com/wcatz/ghost/internal/linking`:

```go
import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)
```

- [ ] **Step 4: Reduce `SweepGrid` to the vec-weight axis in `sweep.go`**

In `internal/bench/sweep.go`, replace `SweepGrid` (the graph-weight axis is gone):

```go
// SweepGrid returns the default parameter grid: vector-leg weight (FTS weight
// is its complement, keeping the legs normalized to 1). RRF k stays at the
// production default — sweep one axis at a time.
func SweepGrid() []memory.SearchParams {
	vecWeights := []float64{0.3, 0.5, 0.6, 0.7, 0.8, 0.9}
	grid := make([]memory.SearchParams, 0, len(vecWeights))
	for _, vw := range vecWeights {
		p := memory.DefaultSearchParams()
		p.VecWeight = vw
		// Round the complement so e.g. 1-0.7 is exactly 0.3 and the grid
		// contains a point == DefaultSearchParams (float identity matters
		// for the "current default" marker).
		p.FTSWeight = math.Round((1-vw)*100) / 100
		grid = append(grid, p)
	}
	return grid
}
```

- [ ] **Step 5: Drop the `graph=` label in `Sweep`**

In `internal/bench/sweep.go`, in `Sweep`, change the condition label so it no longer prints a graph weight:

```go
		cond := fmt.Sprintf("vec=%.2f", p.VecWeight)
```

Also update the `Sweep` doc comment's parenthetical: change `whose link graph is already built (the graph pass reads links at query time, so one store serves every point)` to `over an already-seeded store (one store serves every point)`.

- [ ] **Step 6: Update `runner_test.go` expected conditions**

In `internal/bench/runner_test.go`, `TestRunAllConditions`, drop the graph condition:

```go
	wantConds := []string{CondFTS, CondVector, CondHybrid}
```

The rest of the test is unchanged (the loop already iterates whatever conditions `Run` returns).

- [ ] **Step 7: Remove the tracked hybrid+graph note in `regression_test.go`**

In `internal/bench/regression_test.go`, delete the entire trailing block that references `CondHybridGraph` and `candidateGraphWeight` — from the `// Tracked, not asserted:` comment through its closing `}`. The function now ends after the hybrid-vs-single-leg assertions.

- [ ] **Step 8: Rework `sweep_test.go`**

Replace `TestSweep` so it uses two graph-free points (two different leg weightings) and drops the `CondHybridGraph` cross-check:

```go
func TestSweep(t *testing.T) {
	store, queries := sweepFixture(t)
	ctx := context.Background()

	// Two points: the production default and an off-default leg weighting.
	def := memory.DefaultSearchParams()
	alt := def
	alt.VecWeight = 0.9
	alt.FTSWeight = 0.1
	points, err := Sweep(ctx, store, queries, []memory.SearchParams{def, alt})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}

	// Sorted by NDCG@10 descending.
	if points[0].Result.NDCG10 < points[1].Result.NDCG10 {
		t.Errorf("points not sorted by NDCG: %.3f then %.3f", points[0].Result.NDCG10, points[1].Result.NDCG10)
	}

	// Cross-check the default point against the ablation runner: default
	// params == the hybrid ablation.
	byCond := byCondition(runTestdata(t))
	find := func(p memory.SearchParams) Result {
		for _, pt := range points {
			if pt.Params == p {
				return pt.Result
			}
		}
		t.Fatalf("sweep point not found for %+v", p)
		return Result{}
	}
	if got, want := find(def).NDCG10, byCond[CondHybrid].NDCG10; got != want {
		t.Errorf("default sweep point NDCG %.6f != hybrid ablation %.6f", got, want)
	}
}
```

Replace `TestSweepGrid` for the vec-only grid (6 points, no graph-off assertion):

```go
func TestSweepGrid(t *testing.T) {
	grid := SweepGrid()
	if len(grid) != 6 {
		t.Fatalf("grid size %d, want 6 (6 vec weights)", len(grid))
	}
	def := memory.DefaultSearchParams()
	foundDefault := false
	for _, p := range grid {
		if got := p.FTSWeight + p.VecWeight; got < 0.999 || got > 1.001 {
			t.Errorf("leg weights not normalized: fts=%.2f vec=%.2f", p.FTSWeight, p.VecWeight)
		}
		if p.RRFK != def.RRFK {
			t.Errorf("non-swept knobs must stay at defaults: %+v", p)
		}
		if p == def {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Error("grid must include the production default point")
	}
}
```

Then, since `sweepFixture` still builds the link graph via the linking worker (harmless, and it mirrors production seeding), leave its imports. The graph references are gone, so `TestSweep`/`TestSweepGrid` no longer touch `candidateGraphWeight` or `GraphWeight`/`GraphSeeds`/`GraphHops`. Verify the file's imports (`context`, `io`, `log/slog`, `testing`, `time`, `linking`, `memory`) are all still used by `sweepFixture` — they are — so no import edits are needed here.

- [ ] **Step 9: Delete `graph_test.go`**

```bash
git rm internal/bench/graph_test.go
```

Its sole purpose was to assert the graph ablation builds links; the ablation is gone.

- [ ] **Step 10: Build, vet, and test the bench package**

Run: `go build ./... && go vet ./... && go test ./internal/bench/... -count=1`
Expected: PASS. If `go vet` reports an unused import in `runner.go`, remove it (Step 3 should have handled `slog`/`time`/`linking`).

- [ ] **Step 11: Commit**

```bash
git add internal/bench/
git commit -m "chore(bench): remove graph-expansion ablation from benchmark harness

The graph-bonus arm (CondHybridGraph, candidateGraphWeight, the graph-weight
sweep axis) measured a mechanism that is being removed. Drop it; the fts,
vector, hybrid conditions and the staleness/recency suites are unchanged."
```

---

## Task 2: Remove the graph bonus from the hybrid searcher

**Files:**
- Modify: `internal/memory/vector.go`

- [ ] **Step 1: Remove the `Graph*` fields from `SearchParams`**

In `internal/memory/vector.go`, delete these three fields from the `SearchParams` struct:

```go
	GraphWeight float64 // additive link-graph bonus weight; 0 skips the graph pass
	GraphSeeds  int     // top-ranked results used as graph-expansion seeds
	GraphHops   int     // link-traversal depth from each seed
```

- [ ] **Step 2: Rewrite `DefaultSearchParams` and its comment**

Replace the `DefaultSearchParams` doc comment and body with a version that drops the graph defaults and records the removal rationale:

```go
// DefaultSearchParams returns the production fusion parameters.
//
// A link-graph expansion bonus was evaluated and removed. It was structurally
// dominated by simply retrieving a deeper vector-k: links are built from cosine
// similarity and the vector leg is also cosine, so the bonus only re-surfaced
// cosine-neighbors a larger k already reaches — a public LongMemEval-S kill
// experiment confirmed its recoveries were a strict subset of deeper-k's, with
// no headroom at production depth. The link graph is retained for Obsidian
// export and supersedes ranking. See docs/architecture.md.
func DefaultSearchParams() SearchParams {
	return SearchParams{
		FTSWeight:       0.3,
		VecWeight:       0.7,
		RRFK:            60,
		RecencyWeight:   0,
		RecencyTau:      30,
		SupersedeDemote: false,
	}
}
```

- [ ] **Step 3: Delete `applyGraphBonus`**

Delete the entire `applyGraphBonus` method — from its `// applyGraphBonus adds an additive RRF-style bonus...` comment through the closing `}` of the function.

- [ ] **Step 4: Remove the graph pass from `fuseAndRank` and drop its `projectID` param**

In `fuseAndRank`, update the doc comment (remove graph references):

```go
// fuseAndRank runs the shared hybrid pipeline: RRF-fuse the two result legs,
// then rank, truncate, and hydrate.
func (s *Store) fuseAndRank(ctx context.Context, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
```

Update the tie-break comment (it referenced graph seeds) to:

```go
	// Ranking sorts break score ties by ID so identical searches return
	// identical orderings — the candidate sets come from map iteration, which
	// would otherwise make tie order random per call.
```

Delete the entire graph block:

```go
	// (delete this whole block)
	if p.GraphWeight > 0 && p.GraphSeeds > 0 {
		// Preliminary ranking to pick graph seeds.
		prelim := make([]string, 0, len(idSet))
		for id := range idSet {
			prelim = append(prelim, id)
		}
		byScoreThenID(prelim)

		// Third signal: link-graph expansion from top seeds (additive-only).
		s.applyGraphBonus(ctx, projectID, scores, idSet, prelim, limit, p)
	}
```

- [ ] **Step 5: Update the two `fuseAndRank` call sites**

Both callers drop the `projectID` argument. Around line 406 (in `SearchHybridParams`):

```go
	return s.fuseAndRank(ctx, ftsResults, vecResults, limit, p)
```

Around line 521 (the FTS-fallback path that passed `""`):

```go
	return s.fuseAndRank(ctx, ftsResults, vecResults, limit, DefaultSearchParams())
```

- [ ] **Step 6: Fix the `SearchHybrid` doc comment**

Remove the graph clause:

```go
// SearchHybrid combines FTS5 keyword search with vector similarity using
// Reciprocal Rank Fusion (RRF). Falls back to FTS-only if queryVec is nil.
```

- [ ] **Step 7: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./internal/memory/... -count=1`
Expected: PASS. `go vet` must report no unused variable/import. (`byScoreThenID` is still used by the final ranking; `scores`/`idSet` are still used.)

- [ ] **Step 8: Commit**

```bash
git add internal/memory/vector.go
git commit -m "refactor(memory): remove disabled graph-expansion bonus from searcher

Delete the Graph* SearchParams, applyGraphBonus, and the fuseAndRank graph
pass. The bonus shipped at GraphWeight 0 and was dominated by a deeper
vector-k (links and the vector leg are both cosine). fuseAndRank no longer
needs projectID. The link graph itself is unchanged."
```

---

## Task 3: Remove `GraphNeighbors` from the memory store

**Files:**
- Modify: `internal/memory/links.go`

`GraphNeighbors` had exactly one consumer (`applyGraphBonus`), removed in Task 2, so it is now dead.

- [ ] **Step 1: Delete `GraphNeighbor` and `GraphNeighbors`**

In `internal/memory/links.go`, delete the `GraphNeighbor` struct (from its `// GraphNeighbor is a memory reached...` comment through the closing `}`) and the entire `GraphNeighbors` method (from `// GraphNeighbors walks the link graph outward...` through its closing `}`). Leave `CreateLink`, `GetLinks`, `MarkLinkScanned`, `UnscannedEmbeddedMemoryIDs`, `GetEmbedding`, `EmbeddingStats`, `LinkStats`, `SupersedesWithin`, and `InvalidateLink` intact.

- [ ] **Step 2: Confirm no other references**

Run: `grep -rn "GraphNeighbor" --include=*.go`
Expected: no output (all references removed).

- [ ] **Step 3: Build, vet, test the whole module**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/memory/links.go
git commit -m "refactor(memory): drop now-dead GraphNeighbors link traversal

Its only caller (the graph-expansion bonus) was removed. The memory_links
table and all edge CRUD (CreateLink/GetLinks/SupersedesWithin/...) stay."
```

---

## Task 4: Update documentation

**Files:**
- Modify: `docs/benchmarks.md`
- Modify: `docs/architecture.md`
- Modify: `CLAUDE.md`

- [ ] **Step 1: `docs/benchmarks.md` — ablation section**

Remove the line (near line 31) beginning `- The hybrid+graph ablation is deliberately not run here...`. In the results table (near line 48), delete the `hybrid+graph     0.500   0.964 ...` row. Replace the finding bullet (near line 54) that begins `- **The graph-expansion bonus hurts...**` with:

```markdown
- **The graph-expansion bonus was evaluated and removed.** An additive link-graph bonus (former 0.15 default) lifted semantically-adjacent neighbors above exact matches, and a public LongMemEval-S kill experiment showed its recoveries were a strict subset of a deeper vector-k's, with no headroom at production depth. It shipped at `GraphWeight 0` and has now been removed entirely (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`). The link graph is retained for the Obsidian mirror and `supersedes` ranking.
```

- [ ] **Step 2: `docs/benchmarks.md` — sweep section**

In the sweep paragraph (near line 60), change `grid-searches the vector-leg weight (FTS = complement) × the graph-bonus weight — 36 combinations` to `grid-searches the vector-leg weight (FTS = complement) — 6 combinations`. Delete the graph-specific finding bullet (near line 62, `- **The graph bonus degrades retrieval monotonically.** ...`). Replace the "Outcome" bullet (near line 64) with:

```markdown
- **Outcome: the 70/30 leg weighting ships unchanged, and the graph bonus was removed.** With the leg weights robust across vec 0.3–0.7, there is no evidence to change the shipped 70/30 split. The graph-expansion bonus was removed rather than kept disabled (see the spec linked above); the link graph is still built for the Obsidian mirror and `supersedes`.
```

Leave the line-49-ish `hybrid` result row and the leg-weight robustness bullet (near line 63) intact.

- [ ] **Step 3: `docs/architecture.md`**

- Line ~43: change `runner.go              Graded conditions (fts/vector/hybrid/graph)` to `runner.go              Graded conditions (fts/vector/hybrid)`.
- Line ~52: change `links.go               Memory links: edge CRUD, recursive-CTE graph traversal` to `links.go               Memory links: edge CRUD (related/supersedes)`.
- Lines ~133-135: remove the `(optional additive graph bonus — 2-hop link expansion from ... GraphWeight=0 after a bench sweep showed it demoting exact ...)` parenthetical from the search-pipeline diagram. In its place add a single line after the pipeline: `A link-graph expansion bonus was evaluated and removed (dominated by a deeper vector-k; links and the vector leg are both cosine). The memory_links graph is retained for Obsidian export and supersedes ranking.`
- Line ~164: leave the `memory_links` table row as-is (still accurate).

- [ ] **Step 4: `CLAUDE.md`**

Replace the graph sentence on line ~33:

```markdown
- Memory links: `memory_links` edge table auto-populated by cosine similarity (internal/linking worker); links cascade-delete with memories and self-heal after reflection. A graph-expansion ranking bonus was evaluated and removed — dominated by a deeper vector-k (links and the vector leg are both cosine); the link graph is retained for Obsidian export and supersedes ranking (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`).
```

- [ ] **Step 5: Verify docs reference nothing removed**

Run: `grep -rn "GraphWeight\|GraphNeighbors\|hybrid+graph\|GraphSeeds\|GraphHops" docs/ CLAUDE.md`
Expected: matches only inside `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md` (the spec, which describes the removal) — no stale references in `benchmarks.md`, `architecture.md`, or `CLAUDE.md`.

- [ ] **Step 6: Commit**

```bash
git add docs/benchmarks.md docs/architecture.md CLAUDE.md
git commit -m "docs: record graph-expansion bonus as evaluated and removed

Update benchmarks.md (drop the hybrid+graph ablation row and graph sweep
axis), architecture.md (bench conditions, pipeline diagram, links.go), and
CLAUDE.md to reflect the removal and the retained link graph."
```

---

## Task 5: Final verification, PR, CI + CodeRabbit, merge

**Files:** none (verification + git/GitHub).

- [ ] **Step 1: Full green build**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: PASS across every package.

- [ ] **Step 2: Smoke-test that the link graph still works for Obsidian**

Run: `go run ./cmd/ghost obsidian export --help` to confirm the command still builds, and if a local Ghost DB is available, `go run ./cmd/ghost obsidian export` and spot-check that a memory note still renders its `Related` links section (confirms the linking worker and `GetLinks` path are intact). If no local DB is available, note that and rely on `go test ./internal/obsidian/... -count=1` passing.

- [ ] **Step 3: Rename the branch to match the change**

The branch was created as `docs/graph-expansion-stays-off` during brainstorming, but the change is now a code cleanup. If not yet pushed, rename it:

Run: `git branch -m chore/remove-graph-expansion`

(If it has already been pushed under the old name, skip the rename and keep the existing branch to avoid dangling remotes.)

- [ ] **Step 4: Push and open the PR**

```bash
git push -u origin chore/remove-graph-expansion
gh pr create --title "chore: remove disabled graph-expansion ranking bonus" --body "$(cat <<'EOF'
## What

Removes the link-graph expansion ranking bonus, which shipped disabled
(`GraphWeight = 0`), and its bench ablation. Leaves a short rationale in the
searcher, architecture doc, benchmarks doc, and CLAUDE.md.

## Why

The bonus is structurally dominated by simply retrieving a deeper vector-k:
links are built from cosine similarity and the vector leg is also cosine, so
the bonus only re-surfaces cosine-neighbors a larger k already reaches. A
public LongMemEval-S kill experiment confirmed its recoveries were a strict
subset of deeper-k's, with zero headroom at production retrieval depth. Since
the conclusion follows from the architecture, there is nothing to keep tuning.

Design + decision: `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`.

## Scope

- Removed: `GraphNeighbors`, the `Graph*` `SearchParams`, `applyGraphBonus`,
  the `fuseAndRank` graph pass, and the bench graph ablation/sweep axis.
- Kept (load-bearing): the linking worker, `memory_links`, edge CRUD, and the
  `supersedes` machinery — the `related` link graph also feeds Obsidian export.

## Tests

`go build ./... && go vet ./... && go test ./...` all pass. No fts/vector/hybrid
or staleness/recency coverage was lost.
EOF
)"
```

- [ ] **Step 5: Wait for CI and CodeRabbit**

Run: `gh pr checks --watch` to follow CI to green. Wait for CodeRabbit's review to post. Address any actionable CI failure or CodeRabbit finding with a follow-up commit on the same branch, then re-run `go test ./...` locally before pushing.

- [ ] **Step 6: Merge**

Once CI is green and CodeRabbit review is resolved, merge (per repo convention):

Run: `gh pr merge --squash --delete-branch`

(Confirm the squash-vs-merge preference with the repo's existing PR history if unsure; do not merge until checks are green.)

---

## Self-Review

**Spec coverage:**
- Spec "What is dead" → Tasks 1 (bench arm), 2 (`Graph*` params, `applyGraphBonus`, `fuseAndRank` pass), 3 (`GraphNeighbors`). ✅
- Spec "Load-bearing must NOT be touched" (linking worker, `memory_links`, CRUD, supersedes, Obsidian) → explicitly preserved; Task 5 Step 2 smoke-tests Obsidian. ✅
- Spec "The mention that stays" (comment + architecture.md + commit/decision record) → Task 2 Step 2 comment, Task 4 (architecture.md, benchmarks.md, CLAUDE.md), commit messages carry rationale. ✅
- Spec "Benchmark data policy unchanged" → Task 1 keeps fts/vector/hybrid + staleness/recency; only the graph arm removed. ✅
- Spec Testing (`go vet`/`go test` pass, Obsidian smoke) → Task 5 Steps 1–2. ✅

**Placeholder scan:** No TBD/TODO; every code step shows the exact resulting code or the exact block to delete. ✅

**Type consistency:** `Run` keeps its `(ctx, store, queries, linkThreshold)` signature (callers unchanged); `fuseAndRank` loses `projectID` and both call sites are updated in the same task; `SweepGrid`/`Sweep` names unchanged; `CondFTS/CondVector/CondHybrid` retained, `CondHybridGraph` removed everywhere it appears (runner.go, runner_test.go, regression_test.go, sweep_test.go). ✅
