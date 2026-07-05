# Graph Memory Links Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a note-level memory-link layer (A-MEM/Zettelkasten style) to Ghost: an edge table, an async auto-linking worker driven by existing embeddings, and a graph-expansion bonus signal in hybrid search.

**Architecture:** Links live in a new `memory_links` table keyed by memory IDs with `ON DELETE CASCADE`. Because reflection's `ReplaceNonManual` deletes and reinserts all non-manual memories (unstable IDs), links follow the exact same self-healing lifecycle as embeddings: a background worker (modeled on `internal/embedding/worker.go`) continuously finds embedded-but-unscanned memories, links them to their top cosine neighbors above a threshold, and records the scan in `link_scans`. Retrieval gains a third, **additive-only** signal: after FTS+vector RRF fusion, the top seeds are expanded 1–2 hops via a recursive CTE and neighbors get a bonus score — when no links exist, results are byte-identical to today. No LLM calls in this PR (relation typing like `contradicts`/`supersedes` is a follow-up; the `relation` column exists from day one so it slots in without schema change).

**Tech Stack:** Go 1.25, modernc.org/sqlite (pure Go, no CGO), SQLite recursive CTEs, existing Ollama embeddings. No new dependencies.

**Branch:** `feat/graph-memory-links` (never commit to main; no AI attribution in commits).

**Scope — will touch:**
- `internal/memory/schema.go` (additive tables only)
- `internal/memory/links.go` (new), `internal/memory/links_test.go` (new)
- `internal/memory/vector.go` (graph bonus in `SearchHybrid`/`SearchHybridAll`), `internal/memory/vector_test.go`
- `internal/linking/worker.go` (new), `internal/linking/worker_test.go` (new)
- `internal/config/config.go` (LinkingConfig), `internal/config/config_test.go`
- `cmd/ghost/main.go` (wire worker in `runMCP`)
- `migrations/001_init.sql` (keep canonical SQL in sync with schema.go)

**Will NOT touch:** `internal/provider/provider.go` (no interface signature changes needed), `internal/mcpserver/` (benefits automatically via `SearchHybrid`), `internal/reflection/`, `internal/ai/`, README.

---

### Task 0: Branch

- [ ] **Step 0.1: Create feature branch**

```bash
cd /home/wayne/git/ghost && git checkout main && git pull && git checkout -b feat/graph-memory-links
```

---

### Task 1: Schema + Link CRUD (CreateLink / GetLinks)

**Files:**
- Modify: `internal/memory/schema.go` (append to `initSQL`, before the closing backtick)
- Modify: `migrations/001_init.sql` (append same SQL)
- Create: `internal/memory/links.go`
- Create: `internal/memory/links_test.go`

- [ ] **Step 1.1: Add tables to `initSQL` in `internal/memory/schema.go`**

Append before the closing backtick (after the `idx_snapshots_project` index):

```sql
CREATE TABLE IF NOT EXISTS memory_links (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes')),
    strength       REAL NOT NULL DEFAULT 0.5,
    source         TEXT NOT NULL DEFAULT 'auto'
                   CHECK (source IN ('auto', 'llm', 'manual')),
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    invalidated_at TEXT,
    PRIMARY KEY (source_id, target_id, relation)
);
CREATE INDEX IF NOT EXISTS idx_links_source ON memory_links(source_id) WHERE invalidated_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_links_target ON memory_links(target_id) WHERE invalidated_at IS NULL;

CREATE TABLE IF NOT EXISTS link_scans (
    memory_id  TEXT PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE,
    scanned_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Append the same SQL to `migrations/001_init.sql` (that file is documentation of the canonical schema — the comment at `schema.go:12` says it must stay in sync).

Note: `initSQL` runs on every `OpenDB`, and these statements are all `IF NOT EXISTS`, so existing databases migrate automatically. No ALTER needed.

- [ ] **Step 1.2: Write failing tests for CreateLink/GetLinks**

Create `internal/memory/links_test.go`:

```go
package memory

import (
	"context"
	"testing"
)

// makeMemory creates a memory and returns its ID.
func makeMemory(t *testing.T, s *Store, content string) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

func TestCreateAndGetLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "memory alpha about SQLite WAL mode")
	b := makeMemory(t, s, "memory beta about SQLite busy timeout")

	if err := s.CreateLink(ctx, a, b, "related", 0.85, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	// Links are visible from BOTH endpoints.
	for _, id := range []string{a, b} {
		links, err := s.GetLinks(ctx, id)
		if err != nil {
			t.Fatalf("GetLinks(%s): %v", id, err)
		}
		if len(links) != 1 {
			t.Fatalf("GetLinks(%s): got %d links, want 1", id, len(links))
		}
		if links[0].Relation != "related" || links[0].Strength != 0.85 {
			t.Errorf("link = %+v, want relation=related strength=0.85", links[0])
		}
	}
}

func TestCreateLinkNormalizesSymmetricPair(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha content one")
	b := makeMemory(t, s, "beta content two")

	// Inserting A→B then B→A for symmetric 'related' must not duplicate.
	if err := s.CreateLink(ctx, a, b, "related", 0.80, "auto"); err != nil {
		t.Fatalf("CreateLink a->b: %v", err)
	}
	if err := s.CreateLink(ctx, b, a, "related", 0.90, "auto"); err != nil {
		t.Fatalf("CreateLink b->a: %v", err)
	}
	links, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1 (symmetric pair must normalize)", len(links))
	}
}

func TestCreateLinkRejectsSelfLink(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "self referential memory")

	if err := s.CreateLink(ctx, a, a, "related", 0.9, "auto"); err == nil {
		t.Fatal("CreateLink(a, a) succeeded, want error")
	}
}

func TestLinksCascadeOnMemoryDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha to be deleted")
	b := makeMemory(t, s, "beta survivor")

	if err := s.CreateLink(ctx, a, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := s.Delete(ctx, a); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	links, err := s.GetLinks(ctx, b)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links after cascade delete, want 0", len(links))
	}
}
```

- [ ] **Step 1.3: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'TestCreate.*Link|TestLinks' -v`
Expected: FAIL — `s.CreateLink undefined`, `s.GetLinks undefined` (compile error).

- [ ] **Step 1.4: Implement CreateLink/GetLinks in new `internal/memory/links.go`**

```go
package memory

import (
	"context"
	"fmt"
)

// Link is an edge between two memories. 'related' links are symmetric and
// stored with (source_id < target_id) normalized ordering; directed relations
// (supersedes, contradicts, elaborates, causes) preserve their direction.
type Link struct {
	SourceID      string
	TargetID      string
	Relation      string
	Strength      float32
	Source        string
	CreatedAt     string
	InvalidatedAt *string
}

// symmetricRelations are stored in normalized (min, max) ID order so A→B and
// B→A collapse to one row.
var symmetricRelations = map[string]bool{"related": true}

// CreateLink inserts an edge between two memories. Idempotent: re-inserting
// an existing (source, target, relation) keeps the higher strength and
// clears any invalidation.
func (s *Store) CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error {
	if sourceID == targetID {
		return fmt.Errorf("create link: self-links not allowed (id %s)", sourceID)
	}
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_links (source_id, target_id, relation, strength, source)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
			strength = MAX(strength, excluded.strength),
			invalidated_at = NULL
	`, sourceID, targetID, relation, strength, source)
	if err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	return nil
}

// GetLinks returns all valid (non-invalidated) links touching a memory,
// from either endpoint, strongest first.
func (s *Store) GetLinks(ctx context.Context, memoryID string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
		FROM memory_links
		WHERE (source_id = ? OR target_id = ?) AND invalidated_at IS NULL
		ORDER BY strength DESC
	`, memoryID, memoryID)
	if err != nil {
		return nil, fmt.Errorf("get links: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// InvalidateLink soft-invalidates a link (Zep-style: never delete, mark
// invalid with a timestamp so history is preserved).
func (s *Store) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) error {
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE memory_links SET invalidated_at = datetime('now')
		WHERE source_id = ? AND target_id = ? AND relation = ?
	`, sourceID, targetID, relation)
	if err != nil {
		return fmt.Errorf("invalidate link: %w", err)
	}
	return nil
}
```

- [ ] **Step 1.5: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestCreate.*Link|TestLinks' -v`
Expected: PASS (4 tests).

- [ ] **Step 1.6: Vet and commit**

```bash
go vet ./... && go test ./internal/memory/
git add internal/memory/schema.go internal/memory/links.go internal/memory/links_test.go migrations/001_init.sql
git commit -m "feat(memory): add memory_links edge table with CRUD and soft invalidation"
```

---

### Task 2: InvalidateLink test + scan-tracking queries

**Files:**
- Modify: `internal/memory/links.go` (append)
- Modify: `internal/memory/links_test.go` (append)

- [ ] **Step 2.1: Write failing tests**

Append to `internal/memory/links_test.go`:

```go
func TestInvalidateLinkHidesFromGetLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha invalidation test")
	b := makeMemory(t, s, "beta invalidation test")

	if err := s.CreateLink(ctx, a, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// Invalidate using reversed order — normalization must still find it.
	if err := s.InvalidateLink(ctx, b, a, "related"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	links, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links after invalidation, want 0", len(links))
	}
	// Re-creating the link revives it.
	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink revive: %v", err)
	}
	links, _ = s.GetLinks(ctx, a)
	if len(links) != 1 {
		t.Fatalf("got %d links after revive, want 1", len(links))
	}
}

func TestUnscannedEmbeddedMemoryIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "embedded and unscanned")
	b := makeMemory(t, s, "embedded and scanned")
	_ = makeMemory(t, s, "not embedded")

	vec := []float32{1, 0, 0}
	if err := s.StoreEmbedding(ctx, a, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.StoreEmbedding(ctx, b, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.MarkLinkScanned(ctx, b); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}

	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != a {
		t.Fatalf("got %v, want [%s] (embedded, unscanned only)", ids, a)
	}
}

func TestGetEmbedding(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "embedding roundtrip")

	want := []float32{0.1, 0.2, 0.3}
	if err := s.StoreEmbedding(ctx, a, want, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	got, err := s.GetEmbedding(ctx, a)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(got) != 3 || got[0] != 0.1 || got[1] != 0.2 || got[2] != 0.3 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
```

- [ ] **Step 2.2: Run tests to verify they fail**

Run: `go test ./internal/memory/ -run 'TestInvalidateLink|TestUnscanned|TestGetEmbedding' -v`
Expected: FAIL — `s.MarkLinkScanned undefined`, `s.UnscannedEmbeddedMemoryIDs undefined`, `s.GetEmbedding undefined`.

- [ ] **Step 2.3: Implement the three methods**

Append to `internal/memory/links.go`:

```go
// MarkLinkScanned records that the linking worker has processed this memory.
// Rows cascade-delete with the memory, so reflection churn resets scans
// automatically and the worker self-heals.
func (s *Store) MarkLinkScanned(ctx context.Context, memoryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO link_scans (memory_id) VALUES (?)
		ON CONFLICT(memory_id) DO UPDATE SET scanned_at = datetime('now')
	`, memoryID)
	if err != nil {
		return fmt.Errorf("mark link scanned: %w", err)
	}
	return nil
}

// UnscannedEmbeddedMemoryIDs returns memories that have an embedding but have
// not yet been processed by the linking worker.
func (s *Store) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("unscanned embedded memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetEmbedding returns the stored embedding vector for a memory.
func (s *Store) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding FROM memory_embeddings WHERE memory_id = ?
	`, memoryID).Scan(&blob)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	return bytesToFloat32s(blob), nil
}
```

- [ ] **Step 2.4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run 'TestInvalidateLink|TestUnscanned|TestGetEmbedding' -v`
Expected: PASS (3 tests).

- [ ] **Step 2.5: Vet and commit**

```bash
go vet ./... && go test ./internal/memory/
git add internal/memory/links.go internal/memory/links_test.go
git commit -m "feat(memory): add link scan tracking and embedding fetch for linker"
```

---

### Task 3: GraphNeighbors — recursive CTE traversal

**Files:**
- Modify: `internal/memory/links.go` (append)
- Modify: `internal/memory/links_test.go` (append)

- [ ] **Step 3.1: Write failing test**

Append to `internal/memory/links_test.go`:

```go
func TestGraphNeighborsTwoHops(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Chain: a — b — c, plus d unlinked.
	a := makeMemory(t, s, "graph node a")
	b := makeMemory(t, s, "graph node b")
	c := makeMemory(t, s, "graph node c")
	_ = makeMemory(t, s, "graph node d unlinked")

	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink a-b: %v", err)
	}
	if err := s.CreateLink(ctx, b, c, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink b-c: %v", err)
	}

	// From seed a with 2 hops: b at depth 1 (strength 0.9), c at depth 2 (0.9*0.8=0.72).
	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{a}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2: %+v", len(neighbors), neighbors)
	}
	if neighbors[0].MemoryID != b || neighbors[0].Depth != 1 {
		t.Errorf("first neighbor = %+v, want id=%s depth=1", neighbors[0], b)
	}
	if neighbors[1].MemoryID != c || neighbors[1].Depth != 2 {
		t.Errorf("second neighbor = %+v, want id=%s depth=2", neighbors[1], c)
	}
	if diff := neighbors[1].Strength - 0.72; diff > 0.001 || diff < -0.001 {
		t.Errorf("c strength = %f, want ~0.72", neighbors[1].Strength)
	}

	// With 1 hop, only b is reachable.
	neighbors, err = s.GraphNeighbors(ctx, testProject, []string{a}, 1, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors 1hop: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].MemoryID != b {
		t.Fatalf("1-hop got %+v, want only %s", neighbors, b)
	}
}

func TestGraphNeighborsExcludesSeedsAndCycles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Triangle: a — b — c — a. Seeds {a, b}: only c should be returned.
	a := makeMemory(t, s, "cycle node a")
	b := makeMemory(t, s, "cycle node b")
	c := makeMemory(t, s, "cycle node c")

	for _, pair := range [][2]string{{a, b}, {b, c}, {c, a}} {
		if err := s.CreateLink(ctx, pair[0], pair[1], "related", 0.9, "auto"); err != nil {
			t.Fatalf("CreateLink: %v", err)
		}
	}
	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{a, b}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].MemoryID != c {
		t.Fatalf("got %+v, want only %s (seeds excluded, no cycle blowup)", neighbors, c)
	}
}

func TestGraphNeighborsEmptySeeds(t *testing.T) {
	s := testStore(t)
	neighbors, err := s.GraphNeighbors(context.Background(), testProject, nil, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors(nil): %v", err)
	}
	if len(neighbors) != 0 {
		t.Fatalf("got %d neighbors for empty seeds, want 0", len(neighbors))
	}
}
```

- [ ] **Step 3.2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestGraphNeighbors -v`
Expected: FAIL — `s.GraphNeighbors undefined`.

- [ ] **Step 3.3: Implement GraphNeighbors**

Append to `internal/memory/links.go`:

```go
// GraphNeighbor is a memory reached by traversing links from seed memories.
type GraphNeighbor struct {
	MemoryID string
	Depth    int
	Strength float32 // product of link strengths along the strongest path
}

// GraphNeighbors walks the link graph outward from seed memory IDs up to
// maxHops, returning reachable memories (excluding the seeds themselves)
// ordered by path strength. Traversal is bidirectional and only follows
// valid (non-invalidated) links. Results are scoped to projectID plus
// '_global'; pass projectID "" to search all projects.
func (s *Store) GraphNeighbors(ctx context.Context, projectID string, seedIDs []string, maxHops, limit int) ([]GraphNeighbor, error) {
	if len(seedIDs) == 0 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	placeholders := make([]string, len(seedIDs))
	args := make([]interface{}, 0, len(seedIDs)+4)
	for i, id := range seedIDs {
		placeholders[i] = "(?)"
		args = append(args, id)
	}
	args = append(args, maxHops, projectID, projectID, limit)

	query := fmt.Sprintf(`
		WITH RECURSIVE seeds(id) AS (VALUES %s),
		walk(id, depth, strength) AS (
			SELECT id, 0, 1.0 FROM seeds
			UNION
			SELECT CASE WHEN l.source_id = w.id THEN l.target_id ELSE l.source_id END,
			       w.depth + 1,
			       w.strength * l.strength
			FROM memory_links l
			JOIN walk w ON w.id IN (l.source_id, l.target_id)
			WHERE w.depth < ? AND l.invalidated_at IS NULL
		)
		SELECT w.id, MIN(w.depth) AS depth, MAX(w.strength) AS strength
		FROM walk w
		JOIN memories m ON m.id = w.id
		WHERE w.depth > 0
		  AND w.id NOT IN (SELECT id FROM seeds)
		  AND (? = '' OR m.project_id = ? OR m.project_id = '_global')
		GROUP BY w.id
		ORDER BY strength DESC
		LIMIT ?
	`, strings.Join(placeholders, ","))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("graph neighbors: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var neighbors []GraphNeighbor
	for rows.Next() {
		var n GraphNeighbor
		if err := rows.Scan(&n.MemoryID, &n.Depth, &n.Strength); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, rows.Err()
}
```

Add `"strings"` to the imports of `links.go`.

Recursion is bounded: `w.depth < maxHops` caps expansion (callers pass ≤2), and `UNION` (not `UNION ALL`) dedups identical `(id, depth, strength)` rows, so triangle cycles terminate.

- [ ] **Step 3.4: Run tests to verify they pass**

Run: `go test ./internal/memory/ -run TestGraphNeighbors -v`
Expected: PASS (3 tests).

- [ ] **Step 3.5: Vet and commit**

```bash
go vet ./... && go test ./internal/memory/
git add internal/memory/links.go internal/memory/links_test.go
git commit -m "feat(memory): add GraphNeighbors recursive-CTE link traversal"
```

---

### Task 4: Graph bonus signal in hybrid search

**Files:**
- Modify: `internal/memory/vector.go:133-212` (`SearchHybrid`) and `internal/memory/vector.go:286-350` (`SearchHybridAll`)
- Modify: `internal/memory/vector_test.go` (append)

**Design:** the graph signal is ADDITIVE-ONLY. FTS/vector RRF weights (0.3/0.7) are untouched. After fusing, the top 3 preliminary IDs seed a 2-hop `GraphNeighbors` expansion; each neighbor adds `0.15/(k+rank+1)` weighted by path strength. When no links exist (today's databases), scores are identical to current behavior. Graph errors are non-fatal, matching the existing FTS/vector error handling in this function.

- [ ] **Step 4.1: Write failing test**

Append to `internal/memory/vector_test.go`:

```go
func TestSearchHybridGraphBonus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// a matches the query vector exactly; c is textually and vectorially
	// unrelated but linked to a; d is an unlinked distractor like c.
	a, _ := s.Create(ctx, testProject, Memory{Category: "fact", Content: "kubernetes ingress routing", Source: "manual", Importance: 0.7})
	c, _ := s.Create(ctx, testProject, Memory{Category: "fact", Content: "zzz unrelated cooking recipe", Source: "manual", Importance: 0.7})
	d, _ := s.Create(ctx, testProject, Memory{Category: "fact", Content: "zzz unrelated gardening tips", Source: "manual", Importance: 0.7})

	queryVec := []float32{1, 0, 0}
	if err := s.StoreEmbedding(ctx, a, []float32{1, 0, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding a: %v", err)
	}
	if err := s.StoreEmbedding(ctx, c, []float32{0, 1, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding c: %v", err)
	}
	if err := s.StoreEmbedding(ctx, d, []float32{0, 1, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding d: %v", err)
	}

	rank := func(memories []Memory, id string) int {
		for i, m := range memories {
			if m.ID == id {
				return i
			}
		}
		return -1
	}

	// Without links: c and d are symmetric — tie ranking.
	before, err := s.SearchHybrid(ctx, testProject, "kubernetes ingress", queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid before: %v", err)
	}
	if rank(before, a) != 0 {
		t.Fatalf("a should rank first, got order %v", before)
	}

	// Link a—c. Now c must outrank the symmetric distractor d.
	if err := s.CreateLink(ctx, a, c, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	after, err := s.SearchHybrid(ctx, testProject, "kubernetes ingress", queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid after: %v", err)
	}
	if rank(after, a) != 0 {
		t.Fatalf("a should still rank first, got order %v", after)
	}
	rc, rd := rank(after, c), rank(after, d)
	if rc == -1 || rd == -1 || rc >= rd {
		t.Fatalf("linked c (rank %d) should outrank unlinked d (rank %d): %v", rc, rd, after)
	}
}
```

- [ ] **Step 4.2: Run test to verify it fails**

Run: `go test ./internal/memory/ -run TestSearchHybridGraphBonus -v`
Expected: FAIL — c does not outrank d (no graph signal yet). If it passes spuriously due to tie-break ordering, tighten by asserting a score difference via rank stability across both orders — but map-iteration tie-break should make this deterministic enough to observe the failure; if flaky, swap c/d insertion order and require c strictly before d in both runs.

- [ ] **Step 4.3: Implement the graph bonus**

In `internal/memory/vector.go`, add a helper (place it above `SearchHybrid`):

```go
// applyGraphBonus adds an additive RRF-style bonus to scores for memories
// reachable via links from the top-ranked seed IDs. Additive-only: when no
// links exist, scores are unchanged. Errors are non-fatal (graph signal is
// best-effort, like the FTS and vector legs).
func (s *Store) applyGraphBonus(ctx context.Context, projectID string, scores map[string]float64, idSet map[string]bool, ranked []string, limit int) {
	const k = 60
	const graphWeight = 0.15
	const maxSeeds = 3
	const maxHops = 2

	seeds := ranked
	if len(seeds) > maxSeeds {
		seeds = seeds[:maxSeeds]
	}
	if len(seeds) == 0 {
		return
	}

	neighbors, err := s.graphNeighborsLocked(ctx, projectID, seeds, maxHops, limit)
	if err != nil {
		s.logger.Debug("graph bonus: traversal failed", "error", err)
		return
	}
	for rank, n := range neighbors {
		scores[n.MemoryID] += graphWeight * float64(n.Strength) / float64(k+rank+1)
		idSet[n.MemoryID] = true
	}
}
```

**Locking note:** `SearchHybrid` does not hold `s.mu` itself (its callees take RLock individually), so `applyGraphBonus` can call the public `GraphNeighbors` directly — rename the helper call accordingly. Check this against the actual code when implementing: `SearchFTS` and `SearchVector` each take `s.mu.RLock()` internally and release it, so calling `s.GraphNeighbors(...)` (which also takes RLock) from `SearchHybrid` is safe and there is no `graphNeighborsLocked` needed. Use:

```go
	neighbors, err := s.GraphNeighbors(ctx, projectID, seeds, maxHops, limit)
```

Then in `SearchHybrid` (vector.go), after the two RRF loops that fill `scores`/`idSet` and BEFORE building `ranked`, insert:

```go
	// Preliminary ranking to pick graph seeds.
	prelim := make([]string, 0, len(idSet))
	for id := range idSet {
		prelim = append(prelim, id)
	}
	sort.Slice(prelim, func(i, j int) bool { return scores[prelim[i]] > scores[prelim[j]] })

	// Third signal: link-graph expansion from top seeds (additive-only).
	s.applyGraphBonus(ctx, projectID, scores, idSet, prelim, limit)
```

Make the identical change in `SearchHybridAll`, passing `""` as projectID:

```go
	s.applyGraphBonus(ctx, "", scores, idSet, prelim, limit)
```

- [ ] **Step 4.4: Run tests — new test passes, existing hybrid tests still pass**

Run: `go test ./internal/memory/ -v`
Expected: ALL PASS, including pre-existing `SearchHybrid` tests (behavior unchanged when no links exist).

- [ ] **Step 4.5: Vet and commit**

```bash
go vet ./... && go test ./internal/memory/
git add internal/memory/vector.go internal/memory/vector_test.go
git commit -m "feat(memory): add link-graph expansion as third hybrid search signal"
```

---

### Task 5: Linking worker

**Files:**
- Create: `internal/linking/worker.go`
- Create: `internal/linking/worker_test.go`

**Design:** mirrors `internal/embedding/worker.go` — narrow store interface, periodic sweep. No channel plumbing: the worker sweeps `ListProjects` on a ticker (embeddings appear asynchronously anyway, so save-time triggering buys little). For each unscanned embedded memory: fetch its vector, find top cosine neighbors via the existing `SearchVector`, create `related` links for neighbors at/above threshold, mark scanned. After reflection wipes memories, cascades clear `link_scans` and the next sweep relinks — self-healing.

- [ ] **Step 5.1: Write failing test**

Create `internal/linking/worker_test.go`:

```go
package linking

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

const testProject = "test-project"

func testStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

var _ = sql.ErrNoRows // keep database/sql import if unused after edits

func addEmbedded(t *testing.T, s *memory.Store, content string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, memory.Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	return id
}

func TestSweepOnceLinksSimilarMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// a and b nearly parallel (cosine ~0.995); c orthogonal.
	a := addEmbedded(t, s, "SQLite WAL journal mode", []float32{1, 0, 0.1})
	b := addEmbedded(t, s, "SQLite busy timeout pragma", []float32{1, 0.1, 0})
	c := addEmbedded(t, s, "totally different topic", []float32{0, 1, 0})

	w := NewWorker(s, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), 0, 0.70)
	w.SweepOnce(ctx)

	linksA, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks(a): %v", err)
	}
	if len(linksA) != 1 {
		t.Fatalf("a: got %d links, want 1 (a-b only): %+v", len(linksA), linksA)
	}
	other := linksA[0].SourceID
	if other == a {
		other = linksA[0].TargetID
	}
	if other != b {
		t.Errorf("a linked to %s, want %s", other, b)
	}

	linksC, err := s.GetLinks(ctx, c)
	if err != nil {
		t.Fatalf("GetLinks(c): %v", err)
	}
	if len(linksC) != 0 {
		t.Fatalf("c: got %d links, want 0 (below threshold): %+v", len(linksC), linksC)
	}

	// All embedded memories are now scanned — second sweep is a no-op.
	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("got %d unscanned after sweep, want 0", len(ids))
	}
}
```

- [ ] **Step 5.2: Run test to verify it fails**

Run: `go test ./internal/linking/ -v`
Expected: FAIL — package doesn't exist / `NewWorker` undefined.

- [ ] **Step 5.3: Implement the worker**

Create `internal/linking/worker.go`:

```go
// Package linking discovers relationships between memories by comparing
// their embeddings, persisting them as memory_links edges. It follows the
// same self-healing lifecycle as embeddings: reflection may wipe and
// recreate memories at any time, and the periodic sweep relinks them.
package linking

import (
	"context"
	"log/slog"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// linkStore is the subset of memory.Store needed by the worker.
type linkStore interface {
	ListProjects(ctx context.Context) ([]memory.Project, error)
	UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
	MarkLinkScanned(ctx context.Context, memoryID string) error
}

// Worker periodically links embedded memories to their nearest neighbors.
type Worker struct {
	store     linkStore
	logger    *slog.Logger
	interval  time.Duration
	threshold float32
}

// NewWorker creates a background linking worker. Memories whose cosine
// similarity is at or above threshold get a 'related' link.
func NewWorker(store linkStore, logger *slog.Logger, interval time.Duration, threshold float32) *Worker {
	return &Worker{store: store, logger: logger, interval: interval, threshold: threshold}
}

// Run sweeps all projects on a ticker. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.SweepOnce(ctx)
		}
	}
}

// SweepOnce links unscanned embedded memories across all projects.
func (w *Worker) SweepOnce(ctx context.Context) {
	projects, err := w.store.ListProjects(ctx)
	if err != nil {
		w.logger.Error("linking: list projects", "error", err)
		return
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		w.processProject(ctx, p.ID)
	}
}

const (
	batchSize     = 50
	maxCandidates = 6
)

func (w *Worker) processProject(ctx context.Context, projectID string) {
	ids, err := w.store.UnscannedEmbeddedMemoryIDs(ctx, projectID, batchSize)
	if err != nil {
		w.logger.Error("linking: list unscanned", "error", err, "project_id", projectID)
		return
	}
	if len(ids) == 0 {
		return
	}

	linked := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		vec, err := w.store.GetEmbedding(ctx, id)
		if err != nil {
			w.logger.Debug("linking: get embedding", "error", err, "memory_id", id)
			continue
		}
		// +1 because the memory itself is its own nearest neighbor.
		candidates, err := w.store.SearchVector(ctx, projectID, vec, maxCandidates+1)
		if err != nil {
			w.logger.Debug("linking: search", "error", err, "memory_id", id)
			continue
		}
		for _, cand := range candidates {
			if cand.MemoryID == id || cand.Score < w.threshold {
				continue
			}
			if err := w.store.CreateLink(ctx, id, cand.MemoryID, "related", cand.Score, "auto"); err != nil {
				w.logger.Debug("linking: create link", "error", err, "memory_id", id)
				continue
			}
			linked++
		}
		if err := w.store.MarkLinkScanned(ctx, id); err != nil {
			w.logger.Error("linking: mark scanned", "error", err, "memory_id", id)
		}
	}
	if linked > 0 {
		w.logger.Info("linking: batch complete", "project_id", projectID, "scanned", len(ids), "links", linked)
	}
}
```

Note for the implementer: `NewWorker(s, logger, 0, 0.70)` in the test passes interval 0 — that is fine because the test calls `SweepOnce` directly and never `Run`. Do NOT call `Run` with a zero interval (`time.NewTicker` panics); the main.go wiring in Task 6 passes a real interval.

If the test's `database/sql` import trick looks odd, just remove that import and the `var _ = sql.ErrNoRows` line — they exist only in case an intermediate edit needs them. Clean version omits both.

- [ ] **Step 5.4: Run test to verify it passes**

Run: `go test ./internal/linking/ -v`
Expected: PASS.

- [ ] **Step 5.5: Vet and commit**

```bash
go vet ./... && go test ./...
git add internal/linking/
git commit -m "feat(linking): add background worker that auto-links similar memories"
```

---

### Task 6: Config + wiring in runMCP

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go` (append)
- Modify: `cmd/ghost/main.go:77-84` (`runMCP`)

- [ ] **Step 6.1: Write failing config test**

Append to `internal/config/config_test.go` (inside or alongside the existing defaults test — follow the existing test's pattern of loading defaults, see `config_test.go:62-66`):

```go
func TestLinkingDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Linking.Enabled {
		t.Error("expected linking.enabled=true by default")
	}
	if cfg.Linking.Threshold != 0.70 {
		t.Errorf("expected linking.threshold=0.70, got %f", cfg.Linking.Threshold)
	}
}
```

(Adapt the `Load()` call to match how the existing defaults test constructs config — copy its setup exactly.)

- [ ] **Step 6.2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLinkingDefaults -v`
Expected: FAIL — `cfg.Linking undefined`.

- [ ] **Step 6.3: Add LinkingConfig**

In `internal/config/config.go`:

Add to the main Config struct (next to `Embedding  EmbeddingConfig`):

```go
	Linking    LinkingConfig    `koanf:"linking"`
```

Add the struct (next to `EmbeddingConfig` around line 76):

```go
// LinkingConfig controls the memory auto-linking worker. Linking requires
// embeddings, so it is only active when embedding is also enabled.
type LinkingConfig struct {
	Enabled   bool    `koanf:"enabled"`
	Threshold float64 `koanf:"threshold"`
}
```

Add defaults to the defaults map (near `"embedding.ollama_url"` around line 101):

```go
	"linking.enabled":   true,
	"linking.threshold": 0.70,
```

- [ ] **Step 6.4: Run config tests**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 6.5: Wire the worker in `runMCP` (cmd/ghost/main.go)**

Inside the existing `if cfg.Embedding.Enabled {` block (after `logger.Info("mcp: embedding enabled", ...)` at main.go:83), add:

```go
		if cfg.Linking.Enabled {
			linkWorker := linking.NewWorker(store, logger, 2*time.Minute, float32(cfg.Linking.Threshold))
			go linkWorker.Run(ctx)
			logger.Info("mcp: memory linking enabled", "threshold", cfg.Linking.Threshold)
		}
```

Add `"github.com/wcatz/ghost/internal/linking"` to main.go imports.

- [ ] **Step 6.6: Full verification**

```bash
go vet ./... && go build ./... && go test ./...
```
Expected: all packages build, all tests pass.

- [ ] **Step 6.7: Commit**

```bash
git add internal/config/ cmd/ghost/main.go
git commit -m "feat(mcp): wire memory linking worker with config defaults"
```

---

### Task 7: CLAUDE.md pattern note + PR

**Files:**
- Modify: `CLAUDE.md` (Key Patterns section, one line)

- [ ] **Step 7.1: Document the pattern**

Add one line to the Key Patterns section of `CLAUDE.md`:

```
- Memory links: `memory_links` edge table, auto-linked by cosine similarity (internal/linking worker); links cascade-delete with memories and self-heal after reflection; hybrid search adds an additive graph-expansion bonus from top seeds
```

- [ ] **Step 7.2: Final verification and PR**

```bash
go vet ./... && go test ./...
git add CLAUDE.md
git commit -m "docs: document memory link layer in CLAUDE.md"
git push -u origin feat/graph-memory-links
gh pr create --title "feat: graph memory links — auto-linking worker + graph-aware hybrid search" --body "$(cat <<'EOF'
## Summary
- New `memory_links` edge table (+ `link_scans`) — note-level links between memories, soft-invalidation, cascade-delete with memories
- New `internal/linking` background worker: links embedded memories to top cosine neighbors (threshold 0.70, config `linking.enabled`/`linking.threshold`), self-heals after reflection churn — same lifecycle as embeddings
- `SearchHybrid`/`SearchHybridAll` gain a third, additive-only signal: 2-hop recursive-CTE graph expansion from top-3 seeds (weight 0.15). Zero behavior change when no links exist
- `GraphNeighbors` recursive CTE traversal — pure SQLite, no new dependencies, no CGO

## Design rationale
Research (A-MEM arXiv:2502.12110, Mem0 arXiv:2504.19413, HippoRAG, Zep) shows note-level linking beats entity-triple extraction at this scale, and graph signals should fuse with — never replace — lexical/vector retrieval.

## Test plan
- [x] Unit tests: link CRUD, normalization, cascade, invalidation/revive, scan tracking, CTE traversal (2-hop, cycles, seed exclusion), hybrid graph bonus, worker sweep
- [x] `go vet ./...` + full `go test ./...`
EOF
)"
```

---

## Self-review notes

- **Spec coverage:** (a) links + linking at save lifecycle → Tasks 1–3, 5; (b) graph-aware retrieval fusion → Task 4; (c) soft-invalidation primitives → `InvalidateLink` + `invalidated_at` filtering (Tasks 1–3). LLM relation typing and reflection-driven link curation are explicitly deferred (relation column + CHECK already accommodate them).
- **ID instability:** handled by cascade + `link_scans` cascade + periodic sweep (self-healing, mirrors embeddings).
- **Regression guard:** graph signal is additive-only; existing RRF weights untouched; all existing tests must stay green (verified at each task).
- **Type consistency check:** `CreateLink(ctx, sourceID, targetID, relation, strength float32, source)` used identically in Tasks 1, 3, 4, 5; `GraphNeighbors(ctx, projectID, seedIDs, maxHops, limit)` identical in Tasks 3 and 4; `ScoredMemory.Score` is `float32` (matches vector.go:128).
