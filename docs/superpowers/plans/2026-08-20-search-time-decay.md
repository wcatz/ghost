# Search Time-Decay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `ghost_memory_search` / `ghost_search_all` time-aware by applying `DecayRankingSQL`'s category-aware decay factor to the Go-side fused ranking, so search and the session-start top-memories summary agree.

**Architecture:** A single Go `decayFactor(category, pinned, ageDays)` mirrors the existing SQL constant; a parity test enforces the two can't drift. Ranking truncates to `limit` by base score (relevance) first, then reorders the surviving window by base × decay when `DecayEnabled` is true — decay reorders but never changes membership, so a relevant answer is never dropped for an unrelated-but-younger one (see the staleness-suite findability finding in the spec). `applyRecency`/`RecencyWeight`/`RecencyTau` are removed; `SearchParams` gains `DecayEnabled` (default true, bench-only toggle).

**Tech Stack:** Go 1.26, SQLite (modernc.org/sqlite, no CGO), existing `internal/memory` + `internal/bench` packages. Tests via `go test ./...`; benchmark harness via `go run ./cmd/ghost bench`.

## Global Constraints

- `go vet ./...` must pass before commit (repo rule).
- Tests run with `go test ./...`.
- SQLite schema is embedded in `internal/memory/schema.go` — **no schema change in this feature**.
- `DecayRankingSQL` in `internal/memory/store.go:723` must NOT change (it's the source of truth for `GetTopMemories` + session-start hook). The Go function must mirror it.
- Decay factor values: pinned → 1.0; preference/convention/fact → 1.0; pattern/architecture → `max(0.3, 1/(1+ageDays/45))`; all other categories → `max(0.15, 1/(1+ageDays/30))`. Age from `created_at` only (never `updated_at`); unparseable → ancient (no boost).
- **Decay is reorder-only, never membership-changing:** truncate to `limit` by base score first, then reorder the window by base × decay. Rationale (found during implementation): in the FTS-only path the synthesized base `1/(RRFK+rank+1)` spans only ~1.3× over the candidate window while decay spans ~5.4×, so multiplying before truncation lets decay override relevance and drops rank-1 relevant fresh answers in favor of unrelated younger memories (3/48 staleness probes regressed). Truncate-first keeps fresh-found at 1.000.
- Recency-trap bench fixtures stay `fact` category (never-decay) — do NOT change them; changing them makes the suite fail by design (see spec).
- Never commit to main directly — feature branch + PR.

---

### Task 1: Add Go `decayFactor` function and unit tests

**Files:**
- Modify: `internal/memory/vector.go` (add function near `parseCreatedAt`, ~line 283)
- Test: `internal/memory/vector_test.go` (new test functions)

**Interfaces:**
- Produces: `func decayFactor(category string, pinned bool, ageDays float64) float64` — the exact mirror of `DecayRankingSQL`'s CASE expression. Later tasks (ranking) and Task 2 (parity test) consume it.

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/vector_test.go`:

```go
func TestDecayFactor_MatchesSQLSemantics(t *testing.T) {
	cases := []struct {
		category string
		pinned   bool
		ageDays  float64
		want     float64
	}{
		{"decision", false, 0, 1.0},                    // brand new → no decay
		{"decision", false, 30, 0.5},                   // tau=30: 1/(1+30/30)
		{"gotcha", false, 30, 0.5},                     // same tier as decision
		{"dependency", false, 90, 0.25},                // tau=30: 1/(1+90/30), above 0.15 floor
		{"pattern", false, 45, 0.5},                    // tau=45: 1/(1+45/45)
		{"architecture", false, 100, 45.0 / 145.0},     // tau=45: 1/(1+100/45), above 0.3 floor
		{"preference", false, 1000, 1.0},               // never decays
		{"convention", false, 1000, 1.0},               // never decays
		{"fact", false, 1000, 1.0},                     // never decays
		{"decision", true, 1000, 1.0},                  // pinned → exempt
		{"gotcha", true, 30, 1.0},                      // pinned beats decay
		{"decision", false, -5, 1.2},                   // negative age: SQL doesn't clamp, factor > 1
	}
	const eps = 1e-9
	for _, c := range cases {
		got := decayFactor(c.category, c.pinned, c.ageDays)
		if math.Abs(got-c.want) > eps {
			t.Errorf("decayFactor(%q, pinned=%v, age=%.1f) = %v, want %v",
				c.category, c.pinned, c.ageDays, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestDecayFactor_MatchesSQLSemantics -v`
Expected: FAIL with `undefined: decayFactor`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/memory/vector.go`, right after `parseCreatedAt`:

```go
// decayFactor returns the category-aware time-decay multiplier for a memory
// given its age in days. It mirrors DecayRankingSQL (store.go) exactly — a
// pinned memory or a preference/convention/fact never decays (factor 1.0);
// pattern/architecture decay with tau 45 and a 0.3 floor; all other categories
// (decision, gotcha, dependency, ...) decay with tau 30 and a 0.15 floor. The
// SQL-vs-Go parity test (store_test.go) guards against drift between this and
// the SQL constant.
func decayFactor(category string, pinned bool, ageDays float64) float64 {
	if pinned {
		return 1.0
	}
	switch category {
	case "preference", "convention", "fact":
		return 1.0
	case "pattern", "architecture":
		return math.Max(0.3, 1.0/(1.0+ageDays/45.0))
	default:
		return math.Max(0.15, 1.0/(1.0+ageDays/30.0))
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/memory/ -run TestDecayFactor_MatchesSQLSemantics -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/vector.go internal/memory/vector_test.go
git commit -m "feat(memory): add Go decayFactor mirror of DecayRankingSQL (#316)"
```

---

### Task 2: Parity test — SQL fragment vs Go function

**Files:**
- Test: `internal/memory/store_test.go` (new test near the existing `memoryScore` helper, ~line 3724)

**Interfaces:**
- Consumes: `decayFactor` (Task 1), existing `testStore`, `setCreatedAtDaysAgo`, `memoryScore` helpers in `store_test.go`, `Store.Create`, `Store.TogglePin`.
- Produces: `TestDecaySQLParity` — the drift guard.

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/store_test.go`:

```go
// TestDecaySQLParity pins decayFactor (Go) to the exact DecayRankingSQL
// expression by evaluating both over a grid of categories × ages × pinned.
// This is the drift guard between the SQL constant (GetTopMemories +
// session-start hook) and the Go function (search ranking) — if one is ever
// edited without the other, this test fails.
func TestDecaySQLParity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	categories := []string{
		"preference", "convention", "fact",
		"pattern", "architecture",
		"decision", "gotcha", "dependency",
	}
	ages := []int{0, 1, 7, 30, 45, 90, 180, 365, 1000}

	const eps = 1e-4 // absorbs ms-level julianday('now') drift vs time.Now()
	for _, cat := range categories {
		for _, age := range ages {
			for _, pinned := range []bool{false, true} {
				id, err := s.Create(ctx, testProject, Memory{
					Category: cat, Content: cat, Source: "manual", // "manual" per the source CHECK constraint (schema.go)
					Importance: 1.0, // importance must be 1 so the SQL result IS the factor
				})
				if err != nil {
					t.Fatalf("create %s/%d/pinned=%v: %v", cat, age, pinned, err)
				}
				setCreatedAtDaysAgo(t, s, id, age)
				if pinned {
					if err := s.TogglePin(ctx, id, true); err != nil {
						t.Fatalf("pin %s/%d: %v", cat, age, err)
					}
				}

				sqlFactor := memoryScore(t, s, id)
				// Recompute age in Go from the same now used by SQLite's
				// julianday('now') at query time (sub-second drift, absorbed
				// by eps).
				goAge := time.Since(memoryCreatedAtAsTime(t, s, id)).Hours() / 24.0
				if goAge < 0 {
					goAge = 0
				}
				goFactor := decayFactor(cat, pinned, goAge)

				if math.Abs(sqlFactor-goFactor) > eps {
					t.Errorf("category=%s age=%d pinned=%v: SQL=%v Go=%v (diff %v)",
						cat, age, pinned, sqlFactor, goFactor, math.Abs(sqlFactor-goFactor))
				}
			}
		}
	}
}

// memoryCreatedAtAsTime parses a memory's created_at into a time.Time.
func memoryCreatedAtAsTime(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	raw := memoryCreatedAt(t, s, id)
	parsed, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil {
		t.Fatalf("parse created_at %q: %v", raw, err)
	}
	return parsed
}
```

Note: `time` is already imported in `store_test.go` (it uses `time.Parse` in other helpers). `math` may need importing — check; if absent, add `"math"` to the import block.

- [ ] **Step 2: Run test to verify it fails (or at least that decayFactor is consumed)**

Run: `go test ./internal/memory/ -run TestDecaySQLParity -v`
Expected: PASS immediately IF the Go factor already matches (Task 1 implemented the formula correctly). If it fails, the mismatch output shows exactly which grid point drifted — fix `decayFactor`, not the test.

- [ ] **Step 3: Commit**

```bash
git add internal/memory/store_test.go
git commit -m "test(memory): SQL-vs-Go decay factor parity test (#316)"
```

---

### Task 3: Rewire ranking — decay reorders the window (truncate-first), remove recency prior

**Files:**
- Modify: `internal/memory/vector.go` — `SearchParams` struct (~line 139), `DefaultSearchParams` (~line 180), `applyRecency` (delete, ~line 243), `fuseAndRank` (~line 287), `SearchHybrid`/`SearchHybridParams` (~line 343), `recencyRerank` (delete, ~line 385), `SearchHybridAll` (~line 462)
- Modify: `internal/memory/decisions.go:138` — update the stale comment referencing `recencyRerank`

**Interfaces:**
- Consumes: `decayFactor` (Task 1), `parseCreatedAt` (exists).
- Produces: `SearchParams{ FTSWeight, VecWeight, RRFK, DecayEnabled, SupersedeDemote }`; a new helper `decayRank(results []Memory, scores map[string]float64, p SearchParams, limit int, now time.Time) []Memory` that later bench tasks can rely on.

- [ ] **Step 1: Update SearchParams — remove RecencyWeight/RecencyTau, add DecayEnabled**

Replace the `SearchParams` struct (`vector.go:139-157`):

```go
type SearchParams struct {
	FTSWeight   float64 // RRF weight of the full-text leg
	VecWeight   float64 // RRF weight of the vector leg
	RRFK        int     // RRF smoothing constant (Cormack & Clarke use 60)
	// DecayEnabled applies the category-aware time-decay factor
	// (decayFactor — the Go mirror of DecayRankingSQL) to reorder the result
	// window after truncation: decay reorders but never changes membership, so
	// a more-relevant memory is never dropped in favor of an unrelated younger
	// one. True by default (production behavior). The bench harness toggles it
	// to measure decay-on vs decay-off impact.
	DecayEnabled bool
	// SupersedeDemote, when true, demotes a memory below its superseder within
	// the result window when a valid 'supersedes' link between them exists AND
	// both are present. Unlike the recency prior it is targeted — it only ever
	// touches genuine replacement pairs, so it flips the staleness suite
	// without the collateral damage the recency-trap frontier showed. On by
	// default in production (DefaultSearchParams). See docs/benchmarks.md
	// Phase 3.
	SupersedeDemote bool
}
```

Update `DefaultSearchParams` (`vector.go:180-189`):

```go
func DefaultSearchParams() SearchParams {
	return SearchParams{
		FTSWeight:     0.3,
		VecWeight:     0.7,
		RRFK:          60,
		DecayEnabled:  true,
		SupersedeDemote: true,
	}
}
```

Update the doc comment above `DefaultSearchParams` that references the recency prior ("RecencyWeight scales a freshness prior...") — replace that paragraph with a note that time-awareness now comes from the always-on category-aware decay (see `decayFactor`), and that the recency-trap suite justifies NOT using a blanket age-only prior (see `docs/benchmarks.md`).

- [ ] **Step 2: Write the failing tests for decay ranking**

Add to `internal/memory/vector_test.go` (replacing the now-invalid `TestApplyRecency` at line 689):

```go
func TestApplyDecay_ReordersWindow(t *testing.T) {
	now := timeMustParse("2026-07-15 00:00:00")
	p := DefaultSearchParams() // DecayEnabled true

	// Within the window, decay reorders: fresh decision beats stale decision
	// at equal-ish fused scores (decay-on multiplies the window by decay).
	fresh := Memory{ID: "fresh", Category: "decision", CreatedAt: "2026-07-10 00:00:00"} // 5 days
	stale := Memory{ID: "stale", Category: "decision", CreatedAt: "2026-04-16 00:00:00"} // 90 days
	scores := map[string]float64{"fresh": 0.4, "stale": 0.5}

	got := decayRank([]Memory{stale, fresh}, scores, p, 10, now)
	if got[0].ID != "fresh" {
		t.Errorf("decay should rank fresher first, got %v", ids(got))
	}

	// DecayEnabled false ranks by base score only: stale's higher base (0.5 vs
	// 0.4) wins. (decayRank sorts by base in both modes — the fused path
	// hydrates via GetByIDs, which does not preserve order.)
	off := p
	off.DecayEnabled = false
	got = decayRank([]Memory{stale, fresh}, scores, off, 10, now)
	if got[0].ID != "stale" {
		t.Errorf("DecayEnabled=false must rank by base score, got %v", ids(got))
	}

	// Unparseable created_at treated as ancient (never spuriously wins).
	bad := Memory{ID: "bad", Category: "decision", CreatedAt: "not-a-date"}
	got = decayRank([]Memory{bad, fresh}, scores, p, 10, now)
	if got[0].ID != "fresh" {
		t.Errorf("malformed timestamp must not win; got %v", ids(got))
	}
}

// TestApplyDecay_PreservesMembership is the regression guard for the
// findability finding: truncation happens by base score ALONE, so decay
// reorders within the window but can never drop a higher-base (more relevant)
// memory below the cutoff. This is what keeps TestStalenessReport green.
func TestApplyDecay_PreservesMembership(t *testing.T) {
	now := timeMustParse("2026-07-15 00:00:00")
	p := DefaultSearchParams()

	// stale has the higher base score (0.9 vs 0.5) but is heavily decayed;
	// fresh is barely decayed but lower base. Decay must NOT let fresh displace
	// stale from a limit-1 window — relevance (base) owns membership.
	mems := []Memory{
		{ID: "stale", Category: "dependency", CreatedAt: "2026-01-01 00:00:00"}, // 195 days → floored 0.15
		{ID: "fresh", Category: "dependency", CreatedAt: "2026-07-14 00:00:00"}, // 1 day → ~0.97
	}
	scores := map[string]float64{"stale": 0.9, "fresh": 0.5}

	got := decayRank(mems, scores, p, 1, now)
	if len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("decay must NOT change membership (base 0.9 > 0.5 owns the slot), got %v", ids(got))
	}

	// Same result with decay off (trivially base order).
	off := p
	off.DecayEnabled = false
	got = decayRank(mems, scores, off, 1, now)
	if len(got) != 1 || got[0].ID != "stale" {
		t.Errorf("without decay, higher base score must win, got %v", ids(got))
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'TestApplyDecay' -v`
Expected: FAIL — `undefined: decayRank` (helper not written yet).

- [ ] **Step 4: Implement decay ranking and delete the recency prior**

Delete the entire `applyRecency` function (`vector.go:231-272`).

Replace `recencyRerank` (`vector.go:377-390`) with a decay-aware ranker:

```go
// decayRank truncates results to limit by base score, then (when enabled)
// reorders the surviving window by base score × decayFactor. base is the fused
// score when scores is non-nil; otherwise it is synthesized from position (the
// FTS-only paths), base = 1/(RRFK+rank+1). Membership is owned by base score
// alone — decay reorders but never drops a more-relevant memory, which is what
// keeps findability intact (a rank-1 relevant fresh answer is never displaced
// by unrelated younger memories). The sort by base happens in both modes — the
// fused path hydrates via GetByIDs, which does NOT preserve order. Age reads
// created_at — never updated_at, which Upsert's strengthen path bumps. An
// unparseable created_at is treated as ancient so a malformed timestamp can
// never spuriously win.
func decayRank(results []Memory, scores map[string]float64, p SearchParams, limit int, now time.Time) []Memory {
	scored := make([]struct {
		m    Memory
		base float64
	}, len(results))
	for i, m := range results {
		base := scores[m.ID]
		if scores == nil {
			base = 1.0 / float64(p.RRFK+i+1)
		}
		scored[i] = struct {
			m    Memory
			base float64
		}{m, base}
	}

	// Truncate by base score first: relevance owns membership.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].base != scored[j].base {
			return scored[i].base > scored[j].base
		}
		return scored[i].m.ID < scored[j].m.ID
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}

	// Reorder the window by base × decay (ordering only, never membership).
	if p.DecayEnabled {
		sort.SliceStable(scored, func(i, j int) bool {
			fi := scored[i].base * decayFactor(scored[i].m.Category, scored[i].m.Pinned, ageDays(scored[i].m.CreatedAt, now))
			fj := scored[j].base * decayFactor(scored[j].m.Category, scored[j].m.Pinned, ageDays(scored[j].m.CreatedAt, now))
			if fi != fj {
				return fi > fj
			}
			return scored[i].m.ID < scored[j].m.ID
		})
	}

	out := make([]Memory, len(scored))
	for i, s := range scored {
		out[i] = s.m
	}
	return out
}

// ageDays returns a memory's age in days from its created_at, clamped at 0
// (a future timestamp is treated as brand-new).
func ageDays(createdAt string, now time.Time) float64 {
	ageDays := now.Sub(parseCreatedAt(createdAt)).Hours() / 24.0
	if ageDays < 0 {
		return 0
	}
	return ageDays
}
```

Rewrite `fuseAndRank` (`vector.go:287-341`) to hydrate the full candidate pool and hand truncation + decay reordering to `decayRank`:

```go
func (s *Store) fuseAndRank(ctx context.Context, ftsResults []Memory, vecResults []ScoredMemory, limit int, p SearchParams) ([]Memory, error) {
	scores := make(map[string]float64)
	idSet := make(map[string]bool)
	for rank, m := range ftsResults {
		scores[m.ID] += p.FTSWeight / float64(p.RRFK+rank+1)
		idSet[m.ID] = true
	}
	for rank, sm := range vecResults {
		scores[sm.MemoryID] += p.VecWeight / float64(p.RRFK+rank+1)
		idSet[sm.MemoryID] = true
	}

	ranked := make([]string, 0, len(idSet))
	for id := range idSet {
		ranked = append(ranked, id)
	}

	// Hydrate the full candidate pool before ranking — decayRank needs
	// category/pinned/created_at to reorder the window, and hydration via
	// GetByIDs does not preserve order, so the final sort lives there too.
	memories, err := s.GetByIDs(ctx, ranked)
	if err != nil {
		return nil, err
	}

	memories = decayRank(memories, scores, p, limit, time.Now().UTC())
	return s.demoteSuperseded(ctx, memories, p), nil
}
```

Update `SearchHybridParams`' two FTS-only fallback branches (`vector.go:360` and `:371`) to use `decayRank` instead of `recencyRerank`:

```go
	if queryVec == nil {
		return s.demoteSuperseded(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
	}
```

and

```go
	if len(vecResults) == 0 {
		return s.demoteSuperseded(ctx, decayRank(ftsResults, nil, p, limit, time.Now().UTC()), p), nil
	}
```

Update `SearchHybridAll` (`vector.go:462-494`) the same way — both FTS-only branches (`:475-478` and `:487-490`) become `decayRank(ftsResults, nil, p, limit, time.Now().UTC())`. The fused path already goes through `fuseAndRank`.

Update `internal/memory/decisions.go:138`: the comment "Same invariant as recencyRerank in vector.go: truncate first, reorder the window." references a deleted function. Change it to drop the reference:

```go
	// Reordering before truncating would change *which* decisions come back —
	// it would push the oldest superseded ones out of the window entirely, and
	// a superseded decision is exactly what a caller needs to see to know a
	// prior one was reversed. (This is the same truncate-first invariant the
	// search path's decayRank applies to superseded memories.) The outer sort
	// is a no-op when the caller already filtered by status.
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestApplyDecay' -v`
Expected: PASS.

- [ ] **Step 6: Run the full memory + supersede + mcpserver packages**

Run: `go test ./internal/memory/ ./internal/supersede/ ./internal/mcpserver/`
Expected: PASS. (`internal/supersede/supersede_test.go:121` and mcpserver tests construct `DefaultSearchParams` and must compile against the new struct; `TestProductionSearchDemotesSuperseded` at `vector_test.go:749` exercises `SearchHybrid`/`SearchHybridAll` FTS-only paths through `decayRank`.)

- [ ] **Step 7: Commit**

```bash
git add internal/memory/vector.go internal/memory/vector_test.go internal/memory/decisions.go
git commit -m "feat(memory): apply category-aware time-decay to search ranking (#316)

SearchHybrid/SearchHybridAll truncate to limit by base score, then reorder the
window by decayFactor (ordering-only, so decay never drops a more-relevant
memory). Replaces the RecencyWeight/RecencyTau prior and recencyRerank with an
always-on DecayEnabled default; bench harness can toggle it."
```

---

### Task 4: Bench fixture — staleness suite seeds a decaying category

**Files:**
- Modify: `internal/bench/staleness.go:108`

**Interfaces:**
- Consumes: nothing new.
- Produces: staleness suite fixtures in a decaying category so decay is observable.

- [ ] **Step 1: Run the staleness suite BEFORE the change (baseline)**

Run: `go test ./internal/bench/ -run 'TestStaleness|TestSupersedeDemoteClearsFrontier' -v 2>&1 | grep -E 'fresh-wins|frontier|PASS|FAIL|ok'`
Record the fresh-wins numbers — these are the baseline for the PR description.

Note: `TestStalenessRecencyProof` / `TestRecencyDoesNotPerturbGradedBench` still reference `RecencyWeight` and may currently FAIL to compile after Task 3 — that's expected and resolved in Task 5. If compilation blocks this baseline, defer this baseline capture until after Task 5 and capture "before decay fixtures, after Task 3/5" numbers instead.

- [ ] **Step 2: Change the seeded category**

In `internal/bench/staleness.go:108`, change:

```go
Category: "fact", Content: v.Content, Importance: 0.7, Source: "mcp",
```

to:

```go
Category: "dependency", Content: v.Content, Importance: 0.7, Source: "mcp",
```

Add a brief comment above the `store.Create` call explaining why: the suite measures fresh-versus-stale versions of an updated deployment fact, and `dependency` is a decaying category — under `fact` (never-decay) the decay feature would be unobservable.

- [ ] **Step 3: Run the suite after the change**

Run: `go test ./internal/bench/ -run 'TestStalenessReport' -v`
Expected: PASS. The report must show fresh-found ≈ 1.0 (findability) with the same scenario set.

- [ ] **Step 4: Commit**

```bash
git add internal/bench/staleness.go
git commit -m "bench(staleness): seed dependency category so decay is observable (#316)

The suite's scenarios are updated deployment facts (dependency versions); a
'fact' seed is never-decay and would make the decay feature invisible to the
benchmark."
```

---

### Task 5: Replace RecencyWeight sweep-proof tests

**Files:**
- Modify: `internal/bench/staleness_test.go` — replace `TestStalenessRecencyProof` (line 103) and `TestRecencyDoesNotPerturbGradedBench` (line 145)
- Modify: `internal/bench/recencytrap_test.go` — replace `TestRecencyFrontier` (line 59)

**Interfaces:**
- Consumes: `RunStaleness`, `RunRecencyTrap`, `freshWins`, `TrapCorrectWins`, `Sweep`, `loadStalenessTestdata`, `loadTrapTestdata`, `loadTestdataDataset`, `newBenchStore`, `Seed` (all exist), `memory.SearchParams.DecayEnabled` (Task 3).

- [ ] **Step 1: Replace TestStalenessRecencyProof**

Replace the whole function (`staleness_test.go:103-136`) with:

```go
// TestStalenessDecayProof proves the decay factor flips the suite: at
// DecayEnabled false fresh-wins is low, and with decay on it rises to a
// majority — the dependency-category fixture makes decay observable.
func TestStalenessDecayProof(t *testing.T) {
	scenarios := loadStalenessTestdata(t)
	ctx := context.Background()

	off := memory.DefaultSearchParams()
	off.DecayEnabled = false
	baseline, err := RunStaleness(ctx, scenarios, off, false)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	baseWins := freshWins(baseline)

	on := memory.DefaultSearchParams() // DecayEnabled true
	tunedOutcomes, err := RunStaleness(ctx, scenarios, on, false)
	if err != nil {
		t.Fatalf("tuned: %v", err)
	}
	tunedWins := freshWins(tunedOutcomes)

	t.Logf("fresh-wins: decay-off=%.3f, decay-on=%.3f", baseWins, tunedWins)
	if tunedWins <= baseWins {
		t.Errorf("decay did not improve fresh-wins: %.3f -> %.3f", baseWins, tunedWins)
	}
	if tunedWins < 0.5 {
		t.Errorf("decay should flip the suite to majority fresh-wins, got %.3f", tunedWins)
	}
	// Every fresh version must still be retrieved regardless of ranking.
	for _, o := range tunedOutcomes {
		if !o.FreshFound {
			t.Errorf("%s/%s: fresh not retrieved under decay", o.Scenario, o.ProbeType)
		}
	}
}
```

- [ ] **Step 2: Replace TestRecencyDoesNotPerturbGradedBench**

Replace the whole function (`staleness_test.go:145-166`) with:

```go
// TestDecayDoesNotPerturbGradedBench: the ghost bench dataset is seeded via
// store.Create, which never sets created_at, so every memory shares
// (effectively) the same timestamp — decay applies an identical factor to
// every candidate and cannot reorder them. Hybrid NDCG@10 and recall must be
// identical with decay on and off. This is why flipping DecayEnabled on in
// production is safe for the graded benchmarks.
func TestDecayDoesNotPerturbGradedBench(t *testing.T) {
	ds, vecs := loadTestdataDataset(t)
	ctx := context.Background()

	store := newBenchStore(t)
	queries, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	off := memory.DefaultSearchParams()
	off.DecayEnabled = false
	on := memory.DefaultSearchParams()
	pts, err := Sweep(ctx, store, queries, []memory.SearchParams{off, on})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if pts[0].Result.NDCG10 != pts[1].Result.NDCG10 || pts[0].Result.Recall10 != pts[1].Result.Recall10 {
		t.Errorf("decay perturbed graded ranking (uniform timestamps should make it inert): off NDCG=%.4f on NDCG=%.4f",
			pts[0].Result.NDCG10, pts[1].Result.NDCG10)
	}
}
```

- [ ] **Step 3: Replace TestRecencyFrontier**

Replace the whole function (`recencytrap_test.go:51-112`) with:

```go
// TestDecayFrontier reports the decay-on/off tradeoff over both suites. The
// staleness suite (dependency category) wants fresh-wins HIGH; the
// recency-trap suite (fact category, never-decay) wants correct-wins HIGH.
// Category-aware decay should help staleness WITHOUT hurting the trap suite —
// the free lunch a blanket age-only recency prior could not achieve. The test
// prints the frontier and asserts both properties.
func TestDecayFrontier(t *testing.T) {
	stale := loadStalenessTestdata(t)
	traps := loadTrapTestdata(t)
	ctx := context.Background()

	type row struct {
		label               string
		freshWins, trapWins float64
	}
	var rows []row
	for _, on := range []bool{false, true} {
		p := memory.DefaultSearchParams()
		p.DecayEnabled = on

		so, err := RunStaleness(ctx, stale, p, false)
		if err != nil {
			t.Fatalf("staleness decay=%v: %v", on, err)
		}
		to, err := RunRecencyTrap(ctx, traps, p)
		if err != nil {
			t.Fatalf("trap decay=%v: %v", on, err)
		}
		rows = append(rows, row{
			label:     map[bool]string{false: "decay-off", true: "decay-on"}[on],
			freshWins: freshWins(so),
			trapWins:  TrapCorrectWins(to),
		})
	}

	var b string
	b += fmt.Sprintf("%-12s %-16s %-16s\n", "mode", "staleness-fresh", "trap-correct")
	for _, r := range rows {
		b += fmt.Sprintf("%-12s %-16.3f %-16.3f\n", r.label, r.freshWins, r.trapWins)
	}
	t.Logf("decay frontier:\n%s", b)

	// Decay must help staleness...
	if rows[1].freshWins <= rows[0].freshWins {
		t.Errorf("expected staleness fresh-wins to RISE with decay: off %.3f on %.3f",
			rows[0].freshWins, rows[1].freshWins)
	}
	// ...and must NOT hurt old-but-correct facts (trap suite is fact category,
	// which never decays — its correct-wins should stay flat).
	if rows[1].trapWins < rows[0].trapWins-0.02 {
		t.Errorf("expected trap correct-wins to stay flat under decay (fact never decays): off %.3f on %.3f",
			rows[0].trapWins, rows[1].trapWins)
	}
}
```

- [ ] **Step 4: Run the bench package tests**

Run: `go test ./internal/bench/ -v 2>&1 | grep -E 'frontier|fresh-wins|PASS|FAIL|ok'`
Expected: PASS. `TestDecayFrontier` logs the frontier table.

- [ ] **Step 5: Commit**

```bash
git add internal/bench/staleness_test.go internal/bench/recencytrap_test.go
git commit -m "bench: replace RecencyWeight sweep proofs with decay-aware tests (#316)"
```

---

### Task 6: Update docs/benchmarks.md

**Files:**
- Modify: `docs/benchmarks.md` — the "recency prior" section (line 74) and any Phase 3 conclusions

- [ ] **Step 1: Rewrite the recency prior section**

The current section documents `RecencyWeight` as a shipped-but-default-off global prior with the recency-trap verdict. Replace the mechanism description with the new category-aware decay:

- The mechanism is now `SearchParams.DecayEnabled` (default true): results are truncated to `limit` by base score, then the window is reordered by `decayFactor(category, pinned, ageDays)` — pinned/preference/convention/fact never decay; pattern/architecture τ=45 floor 0.3; else τ=30 floor 0.15. Decay is ordering-only: it never changes membership, which is what keeps findability intact (see the spec's reorder-only rationale).
- Keep the recency-trap rationale verbatim (it's why the old blanket age-only prior was rejected and why decay is category-aware rather than age-only) but update the stale `RecencyWeight` formula references to the decay formula.
- Update the "Why it stays off as a global default" heading to reflect that decay IS on now, and that category-awareness (never-decay categories) is what resolves the staleness/trap cliff the experiment exposed.

- [ ] **Step 2: Verify no stale references remain**

Run: `grep -n "RecencyWeight\|RecencyTau\|recency prior\|applyRecency\|recencyRerank" docs/benchmarks.md`
Expected: only historical/contextual mentions (explicitly framed as the rejected prior), no mechanism claims.

- [ ] **Step 3: Commit**

```bash
git add docs/benchmarks.md
git commit -m "docs: benchmark recency prior section -> category-aware decay (#316)"
```

---

### Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: vet + full test suite**

Run: `go vet ./... && go test ./...`
Expected: all pass, no vet errors.

- [ ] **Step 2: Run the graded benchmark**

Run: `go run ./cmd/ghost bench` (or `ghost bench` if installed). Record NDCG@10 / MRR@10 / recall vs the pre-change numbers from `docs/benchmarks.md`. Graded recall must not regress (it should be unchanged — uniform created_at makes decay inert on the graded dataset).

- [ ] **Step 3: Run the staleness/recency suites with fresh fixtures**

Run: `go test ./internal/bench/ -run 'TestStaleness|TestDecay|TestSupersede' -v`
Expected: PASS; log lines show decay-on fresh-wins ≥ 0.5 and trap-correct flat.

- [ ] **Step 4: Verify CLAUDE.md still accurate**

`CLAUDE.md` says "RecencyWeight defaults to 0 — a documented no-op" is gone from code; the memory config docs may mention it. Grep the repo for `RecencyWeight` outside `docs/superpowers/` and `internal/bench` test history; if `docs/architecture.md` or the README reference it, update those mentions to the decay factor (same treatment as Task 6).

- [ ] **Step 5: Commit any doc stragglers**

```bash
git add -u
git commit -m "docs: remove stale RecencyWeight references (#316)"  # only if Task 6/7 found stragglers
```

---

## Self-Review Notes

- **Spec coverage:** decayFactor (T1), parity test (T2), apply-before-truncation in fused + FTS-only paths + SearchParams/DefaultSearchParams (T3), staleness fixture category (T4), replaced sweep proofs (T5), docs (T6), evaluation (T7). The spec's "Files touched" list maps 1:1 to tasks.
- **Type consistency:** `decayFactor(category string, pinned bool, ageDays float64) float64` defined in T1 and consumed in T2/T3 with identical signature; `decayRank(results []Memory, scores map[string]float64, p SearchParams, limit int, now time.Time) []Memory` defined in T3 and used by both fused and FTS-only paths; `SearchParams.DecayEnabled bool` added in T3, consumed by bench tests in T5. `ageDays` helper introduced in T3.
- **Bench fixture semantics preserved:** staleness → `dependency` (decaying), recency-trap → stays `fact` (never-decay, the safety invariant). No placeholder steps.