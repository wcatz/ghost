# Near-Duplicate Demotion at Session-Start Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop near-duplicate memories from occupying multiple slots in both ranked-memory paths — `Store.GetTopMemories` (backs `ghost_project_context` and the MCP resources) and `loadSessionContext` in `internal/mcpinit/hook.go` (the actual session-start injection text) — by demoting lower-ranked members of a `memory_links`-detected `related` cluster before truncating to the caller's limit.

**Architecture:** Two new shared helpers in `internal/memory/demotion.go` — `DemotionPenalties` (a batched `memory_links` lookup mirroring `SupersedesWithin`) and `StableDemote` (a generic penalty-ascending stable sort mirroring `demoteSuperseded`). `GetTopMemories` over-fetches `limit*2` candidates, calls both helpers through `Store`'s existing `*sql.DB`, then truncates. `loadSessionContext` does the same against its own separate read-only `*sql.DB` connection — no `Store` dependency — over-fetching `LIMIT 50` instead of `LIMIT 25`. A new `linking.demotion_threshold` config key (default 0.90) gates which `related` edges count as "near-duplicate enough to demote," independent of the looser `linking.threshold` (0.70) used for link creation. `Store` gets a `demotionThreshold` field (default baked in, overridable via a new `SetDemotionThreshold` setter called from `bootstrap()` in `main.go`) rather than changing `GetTopMemories`'s signature — this avoids touching the `provider.MemoryStore` interface or any of its four call sites.

**Tech Stack:** Go 1.26, `database/sql` (`modernc.org/sqlite`), `koanf` config layering, standard library `sort.SliceStable` + generics.

---

## File Structure

- **Create** `internal/memory/demotion.go` — `DefaultDemotionThreshold` const, `DemotionPenalties`, `StableDemote`.
- **Create** `internal/memory/demotion_test.go` — unit tests for the two helpers (spec Testing items 1-5).
- **Modify** `internal/config/config.go` — add `DemotionThreshold` field to `LinkingConfig`, add its default to the `defaults` map.
- **Modify** `internal/config/config.example.yaml` — document the new key in the commented `linking:` block.
- **Modify** `internal/config/config_test.go` — extend `TestLinkingDefaults` and `TestLoad_YAMLFileOverride` to cover the new key.
- **Modify** `internal/memory/store.go` — add `demotionThreshold` field + `SetDemotionThreshold` setter to `Store`; rewrite `GetTopMemories` to over-fetch, demote, truncate.
- **Modify** `internal/memory/store_test.go` — add backfill-after-demotion test (spec Testing item 6).
- **Modify** `cmd/ghost/main.go` — call `store.SetDemotionThreshold(cfg.Linking.DemotionThreshold)` in `bootstrap()`.
- **Modify** `internal/mcpinit/hook.go` — add `sessionMemory` struct, change the memories query to select `pinned` with `LIMIT 50`, call the shared helpers, truncate to 25, update `HandleSessionStartHook`'s two format call sites from tuple indexing to field access.
- **Modify** `internal/mcpinit/hook_test.go` — add backfill-after-demotion test against `HandleSessionStartHook`'s actual output (spec Testing item 7).

---

### Task 1: Config key

**Files:**
- Modify: `internal/config/config.go:57-82`
- Modify: `internal/config/config.example.yaml:31-33`
- Modify: `internal/config/config_test.go` (extend `TestLinkingDefaults`, `TestLoad_YAMLFileOverride`)

- [ ] **Step 1: Add the field and default**

In `internal/config/config.go`, change `LinkingConfig`:

```go
// LinkingConfig controls the memory auto-linking worker. Linking requires
// embeddings, so it is only active when embedding is also enabled.
type LinkingConfig struct {
	Enabled           bool    `koanf:"enabled"`
	Threshold         float64 `koanf:"threshold"`
	DemotionThreshold float64 `koanf:"demotion_threshold"`
}
```

And add to the `defaults` map:

```go
var defaults = map[string]interface{}{
	"embedding.enabled":         true,
	"embedding.ollama_url":      "http://localhost:11434",
	"embedding.model":           "nomic-embed-text:v1.5",
	"embedding.dimensions":      768,
	"reflection.backend":        "auto",
	"reflection.auto_resolve":   false,
	"linking.enabled":           true,
	"linking.threshold":         0.70,
	"linking.demotion_threshold": 0.90,
	"obsidian.vault_dir":        "",
	"obsidian.interval":         "30s",
	"obsidian.auto_sync":        false,
}
```

(Run `gofmt -w internal/config/config.go` after editing — the map literal's alignment above is illustrative, not final column widths.)

- [ ] **Step 2: Document the key in the example config**

In `internal/config/config.example.yaml`, change the linking comment block:

```yaml
# --- Memory Linking (requires embedding) ---
# Background worker that links similar memories into a graph. The graph feeds
# the Obsidian mirror's graph view and supersede candidate selection. A
# search-time graph-expansion ranking bonus was evaluated and removed
# (dominated by a deeper vector-k; see docs/benchmarks.md); the link graph is
# retained for the uses above. The worker itself is on by default.
# linking:
#   enabled: true
#   threshold: 0.70            # min cosine similarity to create a link
#   demotion_threshold: 0.90   # min 'related' link strength to demote a
#                              # near-duplicate from GetTopMemories/session-start
#                              # injection; stricter than threshold since
#                              # "related" isn't the same bar as "redundant"
```

- [ ] **Step 3: Extend the default-value test**

In `internal/config/config_test.go`, change `TestLinkingDefaults`:

```go
func TestLinkingDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.Linking.Enabled {
		t.Error("expected linking.enabled=true by default")
	}
	if cfg.Linking.Threshold != 0.70 {
		t.Errorf("expected linking.threshold=0.70, got %f", cfg.Linking.Threshold)
	}
	if cfg.Linking.DemotionThreshold != 0.90 {
		t.Errorf("expected linking.demotion_threshold=0.90, got %f", cfg.Linking.DemotionThreshold)
	}
}
```

- [ ] **Step 4: Extend the YAML-override test**

In `internal/config/config_test.go`, change `TestLoad_YAMLFileOverride`'s fixture and assertions:

```go
	yamlContent := `
api:
  key: "sk-from-yaml"
embedding:
  model: "custom-embed-model"
linking:
  threshold: 0.85
  demotion_threshold: 0.95
reflection:
  backend: "sqlite"
`
```

```go
	if cfg.Linking.Threshold != 0.85 {
		t.Errorf("linking.threshold = %f, want 0.85", cfg.Linking.Threshold)
	}
	if cfg.Linking.DemotionThreshold != 0.95 {
		t.Errorf("linking.demotion_threshold = %f, want 0.95", cfg.Linking.DemotionThreshold)
	}
```

(Add this block immediately after the existing `cfg.Linking.Threshold` check in that test.)

- [ ] **Step 5: Run the config tests**

Run: `go test ./internal/config/... -run 'TestLinkingDefaults|TestLoad_YAMLFileOverride' -v`
Expected: both tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config.example.yaml internal/config/config_test.go
git commit -m "feat(config): add linking.demotion_threshold key"
```

---

### Task 2: Shared demotion helpers

**Files:**
- Create: `internal/memory/demotion.go`
- Create: `internal/memory/demotion_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/demotion_test.go`:

```go
package memory

import (
	"context"
	"testing"
)

func TestDemotionPenaltiesDemotesLowerRanked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha fact")
	b := makeMemory(t, s, "alpha fact restated")

	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[b] != 1 {
		t.Errorf("penalty[b] = %d, want 1", penalty[b])
	}
	if penalty[a] != 0 {
		t.Errorf("penalty[a] = %d, want 0", penalty[a])
	}
}

func TestDemotionPenaltiesIgnoresBelowThreshold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "topic A discussion")
	b := makeMemory(t, s, "topic A follow-up")

	// Related, but not redundant: clears linking.threshold (0.70) without
	// clearing linking.demotion_threshold (0.90).
	if err := s.CreateLink(ctx, a, b, "related", 0.75, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 0 || penalty[b] != 0 {
		t.Errorf("expected no penalties below threshold, got a=%d b=%d", penalty[a], penalty[b])
	}
}

func TestDemotionPenaltiesNeverPenalizesPinnedOverUnpinned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// a ranks higher than b (index 0 vs 1) but b is pinned and a is not.
	a := makeMemory(t, s, "unpinned near-duplicate, ranked higher")
	b := makeMemory(t, s, "pinned near-duplicate, ranked lower")

	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	pinned := map[string]bool{b: true}
	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, pinned, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 1 {
		t.Errorf("penalty[a] = %d, want 1 (unpinned should absorb the penalty)", penalty[a])
	}
	if penalty[b] != 0 {
		t.Errorf("penalty[b] = %d, want 0 (pinned must survive)", penalty[b])
	}
}

func TestDemotionPenaltiesCollapsesMutualCluster(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "cluster fact v1")
	b := makeMemory(t, s, "cluster fact v2")
	c := makeMemory(t, s, "cluster fact v3")

	// All-pairwise above threshold: a is rank 0, b rank 1, c rank 2.
	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink a-b: %v", err)
	}
	if err := s.CreateLink(ctx, a, c, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink a-c: %v", err)
	}
	if err := s.CreateLink(ctx, b, c, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink b-c: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b, c}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 0 {
		t.Errorf("penalty[a] = %d, want 0 (top-ranked survivor)", penalty[a])
	}
	if penalty[b] != 1 {
		t.Errorf("penalty[b] = %d, want 1", penalty[b])
	}
	if penalty[c] != 2 {
		t.Errorf("penalty[c] = %d, want 2 (loses to both a and b)", penalty[c])
	}

	items := []string{a, b, c}
	ordered := StableDemote(items, func(id string) string { return id }, penalty)
	if ordered[0] != a {
		t.Errorf("expected a first after StableDemote, got %v", ordered)
	}
}

func TestDemotionPenaltiesQueryErrorPropagates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := DemotionPenalties(ctx, s.db, []string{"a", "b"}, map[string]bool{}, 0.90)
	if err == nil {
		t.Fatal("expected error from DemotionPenalties on closed db, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/... -run TestDemotionPenalties -v`
Expected: FAIL with `undefined: DemotionPenalties` (and `undefined: StableDemote`).

- [ ] **Step 3: Implement the helpers**

Create `internal/memory/demotion.go`:

```go
package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// DefaultDemotionThreshold is the near-duplicate demotion cutoff used until
// config overrides it (see Store.SetDemotionThreshold and
// internal/mcpinit/hook.go's own fallback for the config-less hook path).
const DefaultDemotionThreshold = 0.90

// DemotionPenalties returns a penalty count per candidate ID, mirroring
// SupersedesWithin's batched-lookup shape (internal/memory/links.go) but over
// 'related' edges at or above threshold instead of 'supersedes' edges.
//
// ids order encodes rank (index 0 = highest-ranked). For every 'related' pair
// found, the lower-ranked ID's penalty is incremented — unless that ID is
// pinned and the other isn't, in which case the unpinned one is penalized
// instead regardless of rank, since pinning is an explicit user signal to
// keep a memory visible. No locking: callers that need Store's s.mu.RLock
// (i.e. GetTopMemories) take it themselves around the call, same as every
// other Store method taking a raw *sql.DB.
func DemotionPenalties(ctx context.Context, db *sql.DB, ids []string, pinned map[string]bool, threshold float64) (map[string]int, error) {
	if len(ids) < 2 {
		return nil, nil
	}

	rank := make(map[string]int, len(ids))
	ph := make([]string, len(ids))
	idArgs := make([]interface{}, len(ids))
	for i, id := range ids {
		rank[id] = i
		ph[i] = "?"
		idArgs[i] = id
	}
	list := strings.Join(ph, ",")

	args := make([]interface{}, 0, 1+len(ids)*2)
	args = append(args, threshold)
	args = append(args, idArgs...)
	args = append(args, idArgs...)

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT source_id, target_id FROM memory_links
		WHERE relation = 'related' AND invalidated_at IS NULL
		  AND strength >= ? AND source_id IN (%s) AND target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	penalty := make(map[string]int, len(ids))
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, fmt.Errorf("demotion penalties: %w", err)
		}
		loser, winner := a, b
		if rank[b] > rank[a] {
			loser, winner = b, a
		}
		if pinned[loser] && !pinned[winner] {
			loser = winner
		}
		penalty[loser]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("demotion penalties: %w", err)
	}
	return penalty, nil
}

// StableDemote reorders items ascending by penalty (ties keep existing
// relative order), mirroring demoteSuperseded's sort.SliceStable pattern
// (internal/memory/vector.go) generically over any item shape that can name
// its own ID via the id accessor.
func StableDemote[T any](items []T, id func(T) string, penalty map[string]int) []T {
	sort.SliceStable(items, func(i, j int) bool {
		return penalty[id(items[i])] < penalty[id(items[j])]
	})
	return items
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/... -run TestDemotionPenalties -v`
Expected: all 5 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/memory/demotion.go internal/memory/demotion_test.go
git commit -m "feat(memory): add DemotionPenalties/StableDemote near-duplicate helpers"
```

---

### Task 3: Wire `Store.GetTopMemories`

**Files:**
- Modify: `internal/memory/store.go:44-62` (Store struct, NewStore)
- Modify: `internal/memory/store.go:429-459` (GetTopMemories)
- Modify: `internal/memory/store_test.go`
- Modify: `cmd/ghost/main.go:916-955` (bootstrap)

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/store_test.go`:

```go
func TestGetTopMemoriesBackfillsAfterDemotion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact", Source: "manual", Importance: 0.9})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact restated", Source: "manual", Importance: 0.85})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	c, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "distinct fact", Source: "manual", Importance: 0.8})
	if err != nil {
		t.Fatalf("create c: %v", err)
	}

	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(top), top)
	}
	got := map[string]bool{top[0].ID: true, top[1].ID: true}
	if !got[a] {
		t.Errorf("expected higher-importance duplicate %q (a) to survive; got %+v", a, top)
	}
	if got[b] {
		t.Errorf("expected lower-ranked duplicate %q (b) to be demoted; got %+v", b, top)
	}
	if !got[c] {
		t.Errorf("expected backfill to include distinct memory %q (c); got %+v", c, top)
	}
}

func TestSetDemotionThresholdOverridesDefault(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact", Source: "manual", Importance: 0.9})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "alpha fact restated", Source: "manual", Importance: 0.85})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	c, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "distinct fact", Source: "manual", Importance: 0.8})
	if err != nil {
		t.Fatalf("create c: %v", err)
	}

	// Link strength (0.80) clears linking.threshold but sits below the
	// default demotion threshold (0.90) — lowering the override to 0.75
	// must make it demote.
	if err := s.CreateLink(ctx, a, b, "related", 0.80, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	s.SetDemotionThreshold(0.75)

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	got := map[string]bool{top[0].ID: true, top[1].ID: true}
	if got[b] {
		t.Errorf("lowered threshold should have demoted b; got %+v", top)
	}
	if !got[c] {
		t.Errorf("expected backfill to include c; got %+v", top)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/... -run 'TestGetTopMemoriesBackfillsAfterDemotion|TestSetDemotionThresholdOverridesDefault' -v`
Expected: FAIL — `TestGetTopMemoriesBackfillsAfterDemotion` returns 3 results instead of 2 (no over-fetch/demotion yet), and `TestSetDemotionThresholdOverridesDefault` fails with `s.SetDemotionThreshold undefined`.

- [ ] **Step 3: Add the field, default, and setter**

In `internal/memory/store.go`, change the `Store` struct and `NewStore`:

```go
// Store manages the SQLite memory database.
type Store struct {
	db     *sql.DB
	mu     sync.RWMutex
	logger *slog.Logger
	onSave func(projectID string) // optional callback after memory create/upsert

	// demotionThreshold gates near-duplicate demotion in GetTopMemories (see
	// DemotionPenalties). Defaults to DefaultDemotionThreshold; callers that
	// have loaded config override it via SetDemotionThreshold.
	demotionThreshold float64
}

// SetOnSave registers a callback invoked after each successful memory save.
// The callback must be non-blocking (e.g., a non-blocking channel send).
func (s *Store) SetOnSave(fn func(projectID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSave = fn
}

// SetDemotionThreshold overrides the near-duplicate demotion cutoff
// GetTopMemories uses. Call after NewStore once config is loaded; until
// called, GetTopMemories uses DefaultDemotionThreshold.
func (s *Store) SetDemotionThreshold(threshold float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demotionThreshold = threshold
}

// NewStore creates a new memory store from an open database.
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger, demotionThreshold: DefaultDemotionThreshold}
}
```

- [ ] **Step 4: Rewrite `GetTopMemories`**

In `internal/memory/store.go`, replace the existing `GetTopMemories` (lines 429-459 before this task's edits):

```go
// GetTopMemories returns the top N memories ranked by composite score with
// category-aware time decay and pinned boost, with near-duplicate demotion:
// candidates linked by a strong enough 'related' edge (see DemotionPenalties)
// are sunk below their higher-ranked match before truncation, so two
// near-identical memories don't both occupy a slot.
func (s *Store) GetTopMemories(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE (project_id = ? OR project_id = '_global')
		  AND resolved_at IS NULL
		ORDER BY (
			importance
			* CASE
				WHEN category IN ('preference', 'convention', 'fact') THEN 1.0
				WHEN category IN ('pattern', 'architecture') THEN
					MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))
				ELSE
					MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))
			END
			* CASE WHEN pinned = 1 THEN 1.5 ELSE 1.0 END
		) DESC
		LIMIT ?
	`, projectID, limit*2)
	if err != nil {
		return nil, fmt.Errorf("get top memories: %w", err)
	}
	results, err := scanMemories(rows)
	_ = rows.Close()
	if err != nil {
		return nil, err
	}

	if len(results) > limit {
		ids := make([]string, len(results))
		pinned := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			pinned[m.ID] = m.Pinned
		}
		penalty, err := DemotionPenalties(ctx, s.db, ids, pinned, s.demotionThreshold)
		if err != nil {
			s.logger.Debug("get top memories: demotion lookup failed", "error", err)
		} else {
			results = StableDemote(results, func(m Memory) string { return m.ID }, penalty)
		}
		if len(results) > limit {
			results = results[:limit]
		}
	}
	return results, nil
}
```

- [ ] **Step 5: Wire config into `bootstrap()`**

In `cmd/ghost/main.go`, change the `bootstrap` function:

```go
	store := memory.NewStore(db, logger)
	store.SetDemotionThreshold(cfg.Linking.DemotionThreshold)

	if err := store.SeedGlobalMemories(context.Background()); err != nil {
		logger.Warn("seed global memories", "error", err)
	}
```

(This replaces the two lines `store := memory.NewStore(db, logger)` followed immediately by the `SeedGlobalMemories` block — insert the new `SetDemotionThreshold` call between them.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/memory/... -run 'TestGetTopMemories|TestSetDemotionThreshold' -v`
Expected: all PASS, including the pre-existing `TestGetTopMemoriesExcludesResolved`.

Run: `go build ./...`
Expected: builds cleanly (confirms `main.go`'s edit compiles).

- [ ] **Step 7: Commit**

```bash
git add internal/memory/store.go internal/memory/store_test.go cmd/ghost/main.go
git commit -m "feat(memory): demote near-duplicates in GetTopMemories"
```

---

### Task 4: Wire `loadSessionContext` (the actual session-start path)

**Files:**
- Modify: `internal/mcpinit/hook.go:1-16` (imports)
- Modify: `internal/mcpinit/hook.go:95-150` (`HandleSessionStartHook`'s memories loop)
- Modify: `internal/mcpinit/hook.go:212-257` (`loadSessionContext`'s memories query)
- Modify: `internal/mcpinit/hook_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/mcpinit/hook_test.go`:

```go
// TestSessionInjectionBackfillsAfterDemotion: a near-duplicate pair linked
// above the demotion threshold must not both occupy injection slots — the
// backfill (LIMIT 50 over-fetch) must surface a distinct memory instead.
func TestSessionInjectionBackfillsAfterDemotion(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// Two near-duplicates (a, b) plus a distinct memory (c). Importance
	// ordering: a > b > c, so without demotion the top-2 would be a, b.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('aaaa0001', 'p1', 'fact', 'ALPHAMARKER original', 'manual', 0.9)`,
	); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('bbbb0001', 'p1', 'fact', 'ALPHAMARKER restated', 'manual', 0.85)`,
	); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('cccc0001', 'p1', 'fact', 'DISTINCTMARKER unrelated', 'manual', 0.8)`,
	); err != nil {
		t.Fatalf("insert c: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('aaaa0001', 'bbbb0001', 'related', 0.95, 'auto')`,
	); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	HandleSessionStartHook(strings.NewReader(string(input)), &out)
	result := out.String()

	if !strings.Contains(result, "ALPHAMARKER original") {
		t.Errorf("higher-ranked duplicate must survive; got:\n%s", result)
	}
	if strings.Contains(result, "ALPHAMARKER restated") {
		t.Errorf("lower-ranked duplicate must be demoted out; got:\n%s", result)
	}
	if !strings.Contains(result, "DISTINCTMARKER") {
		t.Errorf("backfill must surface the distinct memory; got:\n%s", result)
	}
}
```

This test doesn't set a project-scoped injection limit below 3, so it relies on `loadSessionContext`'s real `LIMIT 25`/truncate-to-25 default — all 3 memories fit within 25, so to actually exercise demotion+backfill within this small fixture the test asserts on ordering/inclusion rather than a tight count (unlike the `Store` test, which can pass a small `limit` directly). This is why the assertions check "b absent, a and c present" instead of a result count.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcpinit/... -run TestSessionInjectionBackfillsAfterDemotion -v`
Expected: FAIL — `result` contains `ALPHAMARKER restated` (no demotion applied yet).

- [ ] **Step 3: Add imports and the `sessionMemory` type**

In `internal/mcpinit/hook.go`, change the import block:

```go
import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)
```

Add the struct just above `loadSessionContext`:

```go
// sessionMemory is loadSessionContext's own memory shape — a local struct
// rather than memory.Memory because this function deliberately queries its
// own lightweight *sql.DB connection instead of depending on Store.
type sessionMemory struct {
	ID, Category, Content string
	Pinned                bool
}
```

- [ ] **Step 4: Update `loadSessionContext`'s signature and memories query**

Change the function signature and the memories-query block:

```go
func loadSessionContext(cwd string) (projectID, project string, memories []sessionMemory, learned string, tasks [][4]string, decisions [][2]string, interactionCount int) {
	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		return // no store yet — never create a phantom empty DB
	}
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck

	// Find matching project: try full path prefix first, then cwd basename name match
	projectID, project = lookupProject(db, cwd)
	if projectID == "" {
		return
	}

	// Get learned context summary
	_ = db.QueryRow(
		`SELECT learned_context FROM ghost_state WHERE project_id = ?`, projectID,
	).Scan(&learned)

	// Get top memories: pinned first, then by importance. Over-fetches
	// (LIMIT 50 instead of the eventual 25) so near-duplicate demotion below
	// can drop matches without under-returning.
	rows, err := db.Query(`
		SELECT id, category, content, pinned FROM memories
		WHERE project_id = ? AND resolved_at IS NULL
		ORDER BY pinned DESC, importance DESC, updated_at DESC
		LIMIT 50
	`, projectID)
	if err != nil {
		return
	}
	defer rows.Close() //nolint:errcheck

	for rows.Next() {
		var id, cat, content string
		var pinnedInt int
		if err := rows.Scan(&id, &cat, &content, &pinnedInt); err != nil {
			continue
		}
		content = truncateUTF8(content, 300)
		memories = append(memories, sessionMemory{ID: id, Category: cat, Content: content, Pinned: pinnedInt == 1})
	}

	if len(memories) > 25 {
		demotionThreshold := memory.DefaultDemotionThreshold
		if cfg, cfgErr := config.Load(); cfgErr == nil {
			demotionThreshold = cfg.Linking.DemotionThreshold
		}
		ids := make([]string, len(memories))
		pinned := make(map[string]bool, len(memories))
		for i, m := range memories {
			ids[i] = m.ID
			pinned[m.ID] = m.Pinned
		}
		penalty, penaltyErr := memory.DemotionPenalties(context.Background(), db, ids, pinned, demotionThreshold)
		if penaltyErr != nil {
			fmt.Fprintln(os.Stderr, "ghost: session injection demotion lookup failed:", penaltyErr)
		} else {
			memories = memory.StableDemote(memories, func(m sessionMemory) string { return m.ID }, penalty)
		}
		if len(memories) > 25 {
			memories = memories[:25]
		}
	}

	// Get open tasks
	taskRows, err := db.Query(`
		SELECT id, status, priority, title, COALESCE(description, '')
		FROM tasks
		WHERE project_id = ? AND status IN ('pending', 'active', 'blocked')
		ORDER BY priority ASC, created_at DESC
		LIMIT 10
	`, projectID)
	if err == nil {
		defer taskRows.Close() //nolint:errcheck
		for taskRows.Next() {
			var id, status, title, desc string
			var priority int
			if err := taskRows.Scan(&id, &status, &priority, &title, &desc); err != nil {
				continue
			}
			label := fmt.Sprintf("P%d %s", priority, title)
			tasks = append(tasks, [4]string{shortID(id), status, label, truncateUTF8(desc, 200)})
		}
	}

	// Get active decisions
	decRows, err := db.Query(`
		SELECT title, decision FROM decisions
		WHERE project_id = ? AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 5
	`, projectID)
	if err == nil {
		defer decRows.Close() //nolint:errcheck
		for decRows.Next() {
			var title, decision string
			if err := decRows.Scan(&title, &decision); err != nil {
				continue
			}
			decisions = append(decisions, [2]string{title, truncateUTF8(decision, 200)})
		}
	}

	// Get interaction count
	_ = db.QueryRow(
		`SELECT interaction_count FROM ghost_state WHERE project_id = ?`, projectID,
	).Scan(&interactionCount)

	return
}
```

- [ ] **Step 5: Update `HandleSessionStartHook`'s format loop**

In `internal/mcpinit/hook.go`, change the memories-formatting block:

```go
	if len(memories) > 0 {
		fmt.Fprintf(&sb, "**Memories (%d shown):**\n", len(memories))
		for _, m := range memories {
			fmt.Fprintf(&sb, "- [%s] `%s` %s\n", m.Category, shortID(m.ID), quoteData(m.Content))
		}
	}
```

(This replaces the `m[1]`/`m[0]`/`m[2]` indexing version at the existing lines 145-150.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/mcpinit/... -v`
Expected: all tests PASS, including the new `TestSessionInjectionBackfillsAfterDemotion` and the pre-existing `TestInjectionExcludesResolved`.

- [ ] **Step 7: Commit**

```bash
git add internal/mcpinit/hook.go internal/mcpinit/hook_test.go
git commit -m "feat(hook): demote near-duplicates in session-start injection"
```

---

### Task 5: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Vet the whole module**

Run: `go vet ./...`
Expected: no output (clean).

- [ ] **Step 2: Run the full test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 3: Build the binary**

Run: `make build`
Expected: builds cleanly, matching the eval suite's own prerequisite (`docs/superpowers/eval/README.md` step 1).

- [ ] **Step 4: Manual sanity check (optional but recommended)**

```bash
./ghost mcp status
```

Confirm the printed config reflects `linking.demotion_threshold: 0.9` (or the value from `~/.config/ghost/config.yaml` if one exists) if `mcp status` prints the full `LinkingConfig` — if it doesn't already print linking config, skip this step; it is not a functional requirement of this plan.

---

## Self-Review

**Spec coverage:**
- "Shared implementation, two call sites" (`DemotionPenalties`, `StableDemote`, `sessionMemory`) → Task 2 + Task 4 Step 3.
- "Signal: reuse `memory_links`" (query shape, `related` relation, `strength >=` threshold) → Task 2 Step 3.
- "Threshold: stricter than link-creation" (`linking.demotion_threshold`, default 0.90, independent key) → Task 1.
- "Demotion algorithm" (batched query, penalty map, pinned override, `StableDemote`, truncate) → Task 2 Step 3, Task 3 Step 4, Task 4 Step 4.
- "Backfill via over-fetch" (`limit*2` for `GetTopMemories`, `LIMIT 50`→25 for `loadSessionContext`) → Task 3 Step 4, Task 4 Step 4.
- "Error handling" (fail-open, `s.logger.Debug` vs. `fmt.Fprintln(os.Stderr, ...)`) → Task 3 Step 4, Task 4 Step 4.
- Testing items 1-5 (`DemotionPenalties`/`StableDemote` unit tests) → Task 2 Step 1.
- Testing item 6 (`GetTopMemories` backfill) → Task 3 Step 1.
- Testing item 7 (`loadSessionContext`/`HandleSessionStartHook` backfill) → Task 4 Step 1.
- Out-of-scope items (search-time demotion, cross-project demotion, unifying the two functions, changing `linking.threshold`/worker behavior) → deliberately untouched; no task modifies `internal/memory/vector.go`'s search path or `internal/linking/worker.go`.

**Placeholder scan:** No "TBD"/"add appropriate error handling"/"similar to Task N" phrases — every step above contains complete, copy-pasteable code or an exact command with its expected result.

**Type/signature consistency across tasks:**
- `DemotionPenalties(ctx context.Context, db *sql.DB, ids []string, pinned map[string]bool, threshold float64) (map[string]int, error)` — identical signature used in Task 2 (definition + tests), Task 3 Step 4 (`GetTopMemories`), and Task 4 Step 4 (`loadSessionContext`).
- `StableDemote[T any](items []T, id func(T) string, penalty map[string]int) []T` — identical signature used in Task 2, Task 3 Step 4 (`Memory` instantiation), Task 4 Step 4 (`sessionMemory` instantiation).
- `sessionMemory{ID, Category, Content string; Pinned bool}` — defined once in Task 4 Step 3, consumed identically in Task 4 Steps 4 and 5.
- `Store.demotionThreshold` / `SetDemotionThreshold` — defined in Task 3 Step 3, read in Task 3 Step 4, set from `main.go` in Task 3 Step 5.
- `LinkingConfig.DemotionThreshold` (koanf tag `demotion_threshold`) — defined in Task 1 Step 1, consumed in Task 3 Step 5 (`cfg.Linking.DemotionThreshold`) and Task 4 Step 4 (`cfg.Linking.DemotionThreshold`).

No gaps found.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-01-near-duplicate-demotion.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
