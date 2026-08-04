# Non-Destructive Upsert (Area 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop `Store.Upsert` from destructively overwriting memory content on a detected near-duplicate — every save becomes its own row, with a `'duplicate'` `memory_links` edge back to the matched row carrying the match score.

**Architecture:** `Upsert` keeps its existing two-stage FTS-recall + `mergeScore`-precision candidate search (broadened caps), but on a match it now inserts the new content as a normal new row (reusing the no-match insert path) instead of overwriting the existing row's `content`. It inlines a duplicate-link insert directly against `s.db` (not via `CreateLink`, which would deadlock re-entering `s.mu`). The existing row still gets its importance/access_count/`resolved_at` bump — only the destructive content overwrite is removed. `Upsert`'s return signature grows from `(id, merged bool, err)` to `(id, duplicateOf string, score float64, err)`, keeping arity readable through the two positions that already carried duplicate-detection meaning. `memory_links.relation`'s CHECK constraint needs a `'duplicate'` value added via a `migrateV3` table-rebuild (SQLite can't `ALTER` a CHECK).

**Tech Stack:** Go 1.26, SQLite (modernc.org/sqlite, pure Go), MCP server (modelcontextprotocol/go-sdk).

---

## Scope note on the return signature

The design doc calls for the tool result to surface "existing ID + score." A repo-wide `grep -rn "\.Upsert(" --include=*.go .` found **21 test call sites** beyond the ones store_test.go's dup-merge tests already exercise (4 more in `store_test.go`, 17 in `mcpserver_test.go`). Of those 21, only **2** name the second return value in a way that depends on its type (`mcpserver_test.go:303`, plus the 8 already-scoped `store_test.go` dup tests) — the rest destructure it with `_` and only need one extra blank `_` added for the new 4th return value. This plan updates every one of them; none are silently left broken.

---

## Task 1: Schema — allow `'duplicate'` as a `memory_links.relation` (fresh-DB path)

**Files:**
- Modify: `internal/memory/schema.go:186-199`

- [ ] **Step 1: Update the CHECK constraint in `initSQL`**

In `internal/memory/schema.go`, change:

```sql
CREATE TABLE IF NOT EXISTS memory_links (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes')),
```

to:

```sql
CREATE TABLE IF NOT EXISTS memory_links (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes', 'duplicate')),
```

(Rest of the table — `strength`, `source`, indexes — unchanged.)

- [ ] **Step 2: Commit is deferred to the end of Task 3** (schema + migration + migration test land together so `go test` never sees a half-migrated schema).

---

## Task 2: Migration — `migrateV3` rebuilds `memory_links` for existing DBs

**Files:**
- Modify: `internal/memory/migrate.go:14` (bump `schemaVersion`), `:21-24` (register), append new function

- [ ] **Step 1: Bump `schemaVersion` and register `migrateV3`**

In `internal/memory/migrate.go`, change:

```go
const schemaVersion = 2
```
to:
```go
const schemaVersion = 3
```

and change:
```go
var migrations = []func(*sql.Tx) error{
	migrateV1,
	migrateV2,
}
```
to:
```go
var migrations = []func(*sql.Tx) error{
	migrateV1,
	migrateV2,
	migrateV3,
}
```

- [ ] **Step 2: Write `migrateV3`**

Append to `internal/memory/migrate.go`, after `migrateV2`:

```go
// migrateV3 adds 'duplicate' to memory_links.relation's CHECK — Upsert no
// longer destructively overwrites content on a near-duplicate match; it links
// the new row to the existing one instead, and the CHECK must accept that
// relation value. SQLite cannot ALTER a CHECK constraint, so this is a table
// rebuild like migrateV1's, but simpler: memory_links carries no FTS index or
// rowid dependency, so it's a straight copy under the new DDL. A database that
// never had memory_links at all (pre-dates the link graph entirely) needs no
// rebuild — initSQL's CREATE TABLE IF NOT EXISTS will have already created it
// fresh with this CHECK, same as migrateV1's pattern for tables that don't
// exist yet.
func migrateV3(tx *sql.Tx) error {
	stale, err := tableDDLLacks(tx, "memory_links", "'duplicate'")
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}
	stmts := []string{
		`CREATE TABLE memory_links_v3_new (
    source_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    target_id      TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    relation       TEXT NOT NULL DEFAULT 'related'
                   CHECK (relation IN ('related', 'supersedes', 'contradicts', 'elaborates', 'causes', 'duplicate')),
    strength       REAL NOT NULL DEFAULT 0.5,
    source         TEXT NOT NULL DEFAULT 'auto'
                   CHECK (source IN ('auto', 'llm', 'manual')),
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    invalidated_at TEXT,
    PRIMARY KEY (source_id, target_id, relation)
)`,
		`INSERT INTO memory_links_v3_new (source_id, target_id, relation, strength, source, created_at, invalidated_at)
SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
FROM memory_links`,
		`DROP TABLE memory_links`,
		`ALTER TABLE memory_links_v3_new RENAME TO memory_links`,
		`CREATE INDEX IF NOT EXISTS idx_links_source ON memory_links(source_id) WHERE invalidated_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_links_target ON memory_links(target_id) WHERE invalidated_at IS NULL`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("%q: %w", s[:min(40, len(s))], err)
		}
	}
	return nil
}
```

- [ ] **Step 3: `go build ./...` to confirm it compiles** (no test yet — the test comes in Task 3, and the migration test needs a new fixture before it can pass).

Run: `go build ./...`
Expected: clean build, no output.

---

## Task 3: Migration test — a v2 DB with an existing `memory_links` row upgrades cleanly

**Files:**
- Modify: `internal/memory/migrate_test.go`

Note: `legacySQL` (used by `TestMigrateLegacyDB`, `TestMigrateAddsResolvedAt`, `TestMigrateIdempotent`) predates `memory_links` entirely — those tests already prove the "table doesn't exist yet" path via `migrateV1`'s existing tables, and migrateV3's `tableDDLLacks` early-return covers the same "doesn't exist → no rebuild" case. What's *not* covered is a DB that already has `memory_links` (i.e. any DB created at schemaVersion 2) with real rows in it. This task adds that fixture.

- [ ] **Step 1: Write the failing test**

Append to `internal/memory/migrate_test.go`:

```go
// v2WithLinksSQL is a schemaVersion-2 database: memories, memory_links (old
// CHECK — no 'duplicate'), and the surrounding tables current initSQL creates.
// Distinct from legacySQL, which predates memory_links entirely.
const v2WithLinksSQL = `
CREATE TABLE projects (
    id          TEXT PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE memories (
    id            TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category      TEXT NOT NULL DEFAULT 'fact',
    content       TEXT NOT NULL,
    importance    REAL NOT NULL DEFAULT 0.5,
    access_count  INTEGER NOT NULL DEFAULT 0,
    last_accessed TEXT,
    source        TEXT NOT NULL DEFAULT 'reflection',
    tags          TEXT DEFAULT '[]',
    pinned        INTEGER NOT NULL DEFAULT 0,
    resolved_at   TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    content=memories,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TABLE memory_links (
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
`

// newV2WithLinksDB writes a schemaVersion-2 database with two memories and a
// 'related' link between them, stamped at user_version=2, and returns its path.
func newV2WithLinksDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(v2WithLinksSQL); err != nil {
		t.Fatalf("create v2 schema: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/v2-p1', 'p1')`,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES
			('m1', 'p1', 'fact', 'existing memory one', 'mcp'),
			('m2', 'p1', 'fact', 'existing memory two', 'mcp')`,
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source)
			VALUES ('m1', 'm2', 'related', 0.6, 'auto')`,
		`PRAGMA user_version = 2`,
	}
	for _, s := range seed {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed v2 db: %v", err)
		}
	}
	return dbPath
}

// TestMigrateV3AddsDuplicateRelation: a schemaVersion-2 database with an
// existing memory_links row must upgrade to schemaVersion 3, keep the
// existing link intact, and accept a 'duplicate' relation afterward.
func TestMigrateV3AddsDuplicateRelation(t *testing.T) {
	dbPath := newV2WithLinksDB(t)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB on v2-with-links db: %v", err)
	}
	defer db.Close() //nolint:errcheck

	if v := schemaVersionOf(t, db); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The existing 'related' link survived the rebuild.
	var strength float64
	err = db.QueryRow(
		`SELECT strength FROM memory_links WHERE source_id = 'm1' AND target_id = 'm2' AND relation = 'related'`,
	).Scan(&strength)
	if err != nil {
		t.Fatalf("existing link missing after migration: %v", err)
	}
	if strength != 0.6 {
		t.Errorf("existing link strength = %f, want 0.6", strength)
	}

	// The widened CHECK now accepts 'duplicate'.
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('m2', 'm1', 'duplicate', 0.8, 'auto')`,
	); err != nil {
		t.Errorf("insert with relation='duplicate' still fails: %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it passes** (Tasks 1-2 already landed the schema/migration code, so this should pass immediately — but run it to prove the fixture and migration agree)

Run: `go test ./internal/memory/... -run TestMigrateV3AddsDuplicateRelation -v`
Expected: PASS

- [ ] **Step 3: Run the full migration test suite to confirm no regressions**

Run: `go test ./internal/memory/... -run TestMigrate -v`
Expected: all PASS, including the pre-existing `TestMigrateLegacyDB`, `TestMigrateFreshDBStamped`, `TestMigrateFreshDBHasResolvedAt`, `TestMigrateHandMigratedDB`, `TestMigrateIdempotent`, `TestMigrateAddsResolvedAt`.

- [ ] **Step 4: Commit**

```bash
git add internal/memory/schema.go internal/memory/migrate.go internal/memory/migrate_test.go
git commit -m "feat(memory): add 'duplicate' relation to memory_links schema"
```

---

## Task 4: Update the `provider.MemoryStore` interface

**Files:**
- Modify: `internal/provider/provider.go:23`

- [ ] **Step 1: Change the interface signature**

In `internal/provider/provider.go`, change:

```go
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, bool, error)
```

to:

```go
	Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (string, string, float64, error)
```

This will not compile cleanly until Task 5 changes `Store.Upsert` to match — that's expected and fixed in the next task.

---

## Task 5: Rewrite `Store.Upsert` — non-destructive merge

**Files:**
- Modify: `internal/memory/store.go:349-441` (the whole function), `:1439-1469` (`sanitizeFTS`)

- [ ] **Step 1: Broaden `sanitizeFTS`'s truncation cap**

In `internal/memory/store.go`, change:

```go
	// Limit to first 10 words to keep the query reasonable.
	if len(words) > 10 {
		slog.Warn("fts query truncated",
			"original_terms", len(words),
			"limit", 10)
		words = words[:10]
	}
```

to:

```go
	// Limit to first 30 words to keep the query reasonable.
	if len(words) > 30 {
		slog.Warn("fts query truncated",
			"original_terms", len(words),
			"limit", 30)
		words = words[:30]
	}
```

- [ ] **Step 2: Replace the entire `Upsert` function**

Replace `internal/memory/store.go:349-441` (the current `Upsert`, from its doc comment through the closing brace) with:

```go
// Upsert checks for an existing similar memory (same category, FTS overlap).
// If found, it strengthens the existing memory's importance/access_count and
// links the new content to it as a 'duplicate' — it never overwrites the
// existing row's content, so a genuinely different follow-up save under a
// manual-sourced target (or any target) is never silently discarded. If no
// candidate scores above threshold, it creates a new, unlinked row.
func (s *Store) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (id string, duplicateOf string, score float64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var existingID string
	var existingImportance float32

	// Two-stage duplicate detection. Stage 1 (recall): the FTS OR-probe over
	// the first 30 words retrieves merge candidates cheaply. Stage 2
	// (precision): mergeScore over the full token sets confirms a true
	// duplicate — the OR-probe alone treats a single shared word as a match,
	// which silently swallowed unrelated saves.
	ftsQuery := sanitizeFTS(content)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.importance, m.content
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE m.project_id = ?
		  AND m.category = ?
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT 15
	`, projectID, category, ftsQuery)
	if err == nil {
		// Token-free content (punctuation/single-char words only) can still
		// FTS-match — sanitizeFTS keeps single-char words that tokenizeContent
		// drops — and jaccard(∅,∅) scores 1.0. Never merge on empty tokens.
		newTokens := tokenizeContent(content)
		var bestSim float64
		for len(newTokens) > 0 && rows.Next() {
			var candID, candContent string
			var candImportance float32
			if scanErr := rows.Scan(&candID, &candImportance, &candContent); scanErr != nil {
				continue
			}
			sim := mergeScore(newTokens, tokenizeContent(candContent))
			if sim > 0 && sim > bestSim {
				bestSim = sim
				existingID = candID
				existingImportance = candImportance
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			existingID = "" // treat a broken candidate scan as no match
		}
		_ = rows.Close()
		score = bestSim
	}

	tagsJSON, _ := json.Marshal(tags)

	if existingID != "" {
		// Found a match — strengthen the existing row, then insert the new
		// content as its own row and link it back as a 'duplicate'. Never
		// overwrite existingContent: that silently discarded content for
		// source='manual' targets while still reporting a merge.
		newImportance := existingImportance + (importance * 0.2)
		if newImportance > 1.0 {
			newImportance = 1.0
		}
		if _, err = s.db.ExecContext(ctx, `
			UPDATE memories
			SET importance = ?, access_count = access_count + 1, updated_at = datetime('now'),
			    resolved_at = NULL
			WHERE id = ? AND project_id = ?
		`, newImportance, existingID, projectID); err != nil {
			return "", "", 0, fmt.Errorf("strengthen memory: %w", err)
		}

		if err = s.db.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags)
			VALUES (?, ?, ?, ?, ?, ?)
			RETURNING id
		`, projectID, category, content, source, importance, string(tagsJSON)).Scan(&id); err != nil {
			return "", "", 0, fmt.Errorf("create memory: %w", err)
		}

		// CreateLink takes s.mu itself — calling it here would deadlock, since
		// Upsert already holds the lock for its entire body. Inline the same
		// upsert-link SQL directly instead. Direction is fixed (source_id=new,
		// target_id=existing), not normalized like symmetric relations.
		if _, err = s.db.ExecContext(ctx, `
			INSERT INTO memory_links (source_id, target_id, relation, strength, source)
			VALUES (?, ?, 'duplicate', ?, 'auto')
			ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
				strength = MAX(strength, excluded.strength),
				invalidated_at = NULL
		`, id, existingID, score); err != nil {
			return "", "", 0, fmt.Errorf("link duplicate: %w", err)
		}

		if s.onSave != nil {
			s.onSave(projectID)
		}
		return id, existingID, score, nil
	}

	// No match — create new.
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, category, content, source, importance, string(tagsJSON)).Scan(&id)
	if err != nil {
		return "", "", 0, fmt.Errorf("create memory: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, "", 0, nil
}
```

- [ ] **Step 3: Try to build** (expected to fail — call sites aren't updated yet)

Run: `go build ./...`
Expected: FAIL — compile errors in `internal/mcpserver`, `internal/claudeimport`, and every `_test.go` file that calls `Upsert`. This confirms the signature actually changed; Tasks 6-8 fix every call site.

---

## Task 6: Fix the 3 non-test call sites

**Files:**
- Modify: `internal/mcpserver/mcpserver.go:555-572` (`ghost_memory_save`), `:894-903` (`ghost_save_global`)
- Modify: `internal/claudeimport/import.go:244`

- [ ] **Step 1: `ghost_memory_save`**

In `internal/mcpserver/mcpserver.go`, change:

```go
		id, merged, err := s.store.Upsert(ctx, args.ProjectID, args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		s.notifyProjectResource(ctx, args.ProjectID, "context")

		// Notify embedding worker of new/updated memory.
		if s.projectCh != nil {
			select {
			case s.projectCh <- args.ProjectID:
			default: // non-blocking
			}
		}

		action := "saved"
		if merged {
			action = "merged with existing memory"
		}
		msg := fmt.Sprintf("Memory %s (id: %s)", action, id)
```

to:

```go
		id, duplicateOf, score, err := s.store.Upsert(ctx, args.ProjectID, args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		s.notifyProjectResource(ctx, args.ProjectID, "context")

		// Notify embedding worker of new/updated memory.
		if s.projectCh != nil {
			select {
			case s.projectCh <- args.ProjectID:
			default: // non-blocking
			}
		}

		msg := fmt.Sprintf("Memory saved (id: %s)", id)
		if duplicateOf != "" {
			msg = fmt.Sprintf("Memory saved (id: %s), linked as a likely duplicate of %s (score %.2f)", id, duplicateOf, score)
		}
```

- [ ] **Step 2: `ghost_save_global`**

In `internal/mcpserver/mcpserver.go`, change:

```go
		id, merged, err := s.store.Upsert(ctx, "_global", args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		s.notifyResourceUpdated(ctx, "ghost://memories/global")
		action := "saved"
		if merged {
			action = "merged with existing"
		}
		msg := fmt.Sprintf("Global memory %s (id: %s)", action, id)
```

to:

```go
		id, duplicateOf, score, err := s.store.Upsert(ctx, "_global", args.Category, args.Content, "mcp", importance, args.Tags)
		if err != nil {
			return nil, nil, fmt.Errorf("save failed: %w", err)
		}
		s.notifyResourceUpdated(ctx, "ghost://memories/global")
		msg := fmt.Sprintf("Global memory saved (id: %s)", id)
		if duplicateOf != "" {
			msg = fmt.Sprintf("Global memory saved (id: %s), linked as a likely duplicate of %s (score %.2f)", id, duplicateOf, score)
		}
```

- [ ] **Step 3: `claudeimport`**

In `internal/claudeimport/import.go`, change:

```go
		_, _, err = store.Upsert(ctx, projectID, category, content, source, importance, importTags)
```

to:

```go
		_, _, _, err = store.Upsert(ctx, projectID, category, content, source, importance, importTags)
```

- [ ] **Step 4: `go build ./...`**

Run: `go build ./...`
Expected: still FAILS in `_test.go` files (Tasks 7-8 not done yet) but the three non-test packages above now compile — check `go build ./internal/mcpserver/... ./internal/claudeimport/... ./internal/memory/... ./internal/provider/...` (excluding test files) succeeds:

Run: `go vet ./internal/mcpserver ./internal/claudeimport ./internal/memory ./internal/provider`
Expected: only test-file errors reported (or none, if `go vet` on non-test build succeeds first) — proceed to Task 7 regardless.

---

## Task 7: Fix `store_test.go` — mechanical arity fixes (no behavior change)

**Files:**
- Modify: `internal/memory/store_test.go:167`, `:201`, `:229`, `:236`, `:272`, `:308`, `:379`, `:1822`, `:2268`

These 9 call sites destructure the second return value with `_` and don't depend on its type — they only need a second blank added for the new 4th return value.

- [ ] **Step 1: Fix all 9 blank-destructure sites**

Each of these lines:

```go
	_, _, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite busy timeout must be set on the read-only hook connection",
		"mcp", 0.7, nil)
```
(line 167), and the same `_, _, err :=` / `_, _, err =` shape at lines 201, 229, 272, 308, 379 in `store_test.go`, and at line 1822 (`memID, _, err :=`) and 2268 (`_, _, err :=`) — each becomes a 4-value destructure with an extra blank in the second position:

```go
	_, _, _, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite busy timeout must be set on the read-only hook connection",
		"mcp", 0.7, nil)
```

Line 236 (`idB, _, err := s.Upsert(...)`) becomes:

```go
	idB, _, _, err := s.Upsert(ctx, testProject, "fact",
		"Ollama embedding worker sweeps unembedded memories every two minutes",
		"mcp", 0.7, nil)
```

Line 1822 (`memID, _, err := s.Upsert(...)`) becomes:

```go
	memID, _, _, err := s.Upsert(ctx, "test", "fact", "MCP-created memory about deployment", "mcp", 0.7, []string{})
```

Line 2268 (`_, _, err := s.Upsert(...)`) becomes:

```go
	_, _, _, err := s.Upsert(ctx, "myproject", "fact", "deployed on k8s cluster alpha-7", "mcp", 0.8, []string{})
```

- [ ] **Step 2: Rewrite `TestStoreUpsert` (lines 79-119)**

Replace the whole test:

```go
func TestStoreUpsert(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// First insert via Upsert — should create new.
	id1, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "SQLite supports full-text search via FTS5", "reflection", 0.6, []string{"sqlite"})
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}
	if dupOf != "" {
		t.Error("first Upsert should not report a duplicate")
	}
	if id1 == "" {
		t.Fatal("first Upsert returned empty ID")
	}

	// Second Upsert with overlapping content — should link as a duplicate of id1.
	id2, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "SQLite FTS5 provides full-text search capabilities", "reflection", 0.5, []string{"sqlite", "fts"})
	if err != nil {
		t.Fatalf("Upsert (merge): %v", err)
	}
	if dupOf != id1 {
		t.Errorf("second Upsert should report duplicateOf %s, got %q", id1, dupOf)
	}
	if id2 == id1 {
		t.Error("second Upsert should have created its own row, not reused id1")
	}

	// Both rows exist, with their original content untouched.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories after duplicate-link, got %d", len(all))
	}
	byID := map[string]Memory{}
	for _, m := range all {
		byID[m.ID] = m
	}
	if byID[id1].Content != "SQLite supports full-text search via FTS5" {
		t.Errorf("existing row content was overwritten: %q", byID[id1].Content)
	}
	if byID[id2].Content != "SQLite FTS5 provides full-text search capabilities" {
		t.Errorf("new row content wrong: %q", byID[id2].Content)
	}

	// Existing row's importance was strengthened: 0.6 + (0.5 * 0.2) = 0.7
	expected := float32(0.6 + 0.5*0.2)
	if diff := byID[id1].Importance - expected; diff > 0.01 || diff < -0.01 {
		t.Errorf("expected existing-row importance ~%f, got %f", expected, byID[id1].Importance)
	}

	// A 'duplicate' link connects the two rows in the right direction.
	links, err := s.GetLinks(ctx, id1)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "duplicate" && l.SourceID == id2 && l.TargetID == id1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 'duplicate' link source_id=%s target_id=%s, links: %+v", id2, id1, links)
	}
}
```

- [ ] **Step 3: Update the 4 `merged`-checking "no merge" tests to check `dupOf`**

`TestStoreUpsertNoMergeOnSharedWord` (lines 121-159): change the two Upsert calls from
```go
	id1, merged, err := s.Upsert(ctx, testProject, "decision", ...)
	...
	if merged {
		t.Error("first Upsert should not merge")
	}
	...
	id2, merged, err := s.Upsert(ctx, testProject, "decision", ...)
	...
	if merged {
		t.Error("unrelated content sharing one word must not merge")
	}
```
to:
```go
	id1, dupOf, _, err := s.Upsert(ctx, testProject, "decision",
		"Benchmark strategy decided: Phase one is LongMemEval retrieval eval with ablations",
		"mcp", 0.8, nil)
	if err != nil {
		t.Fatalf("Upsert (first): %v", err)
	}
	if dupOf != "" {
		t.Error("first Upsert should not report a duplicate")
	}

	id2, dupOf, _, err := s.Upsert(ctx, testProject, "decision",
		"MCP Phase two design approved: memory update tool, stop hook, promote to global",
		"mcp", 0.8, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if dupOf != "" {
		t.Error("unrelated content sharing one word must not report a duplicate")
	}
```
(keep the trailing `id2 == id1` check and the `len(all) != 2` `GetAll` check as-is).

`TestStoreUpsertNoMergeBelowThreshold` (lines 161-191): change
```go
	_, merged, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite FTS5 rank ordering is unstable across identical RRF scores in hybrid search",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if merged {
		t.Error("below-threshold overlap must not merge")
	}
```
to:
```go
	_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite FTS5 rank ordering is unstable across identical RRF scores in hybrid search",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if dupOf != "" {
		t.Error("below-threshold overlap must not report a duplicate")
	}
```

`TestStoreUpsertNoMergeOnTokenFreeContent` (lines 193-221): change
```go
	_, merged, err := s.Upsert(ctx, testProject, "fact", "a x y", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if merged {
		t.Error("token-free contents must never merge")
	}
```
to:
```go
	_, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "a x y", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if dupOf != "" {
		t.Error("token-free contents must never report a duplicate")
	}
```

`TestStoreUpsertOverlapLegDoesNotOverMerge` (lines 297-343): change both `merged`-checking loops:
```go
	for i, sh := range shorts {
		_, merged, err := s.Upsert(ctx, testProject, "gotcha", sh, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (short %d): %v", i, err)
		}
		if merged {
			t.Errorf("short contained save %d must not merge into a longer unrelated memory: %q", i, sh)
		}
	}
	...
	for i, d := range distinct {
		_, merged, err := s.Upsert(ctx, testProject, "gotcha", d, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (distinct %d): %v", i, err)
		}
		if merged {
			t.Errorf("distinct fact %d sharing topic vocabulary must not merge: %q", i, d)
		}
	}
```
to:
```go
	for i, sh := range shorts {
		_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", sh, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (short %d): %v", i, err)
		}
		if dupOf != "" {
			t.Errorf("short contained save %d must not report a duplicate for a longer unrelated memory: %q", i, sh)
		}
	}
	...
	for i, d := range distinct {
		_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", d, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (distinct %d): %v", i, err)
		}
		if dupOf != "" {
			t.Errorf("distinct fact %d sharing topic vocabulary must not report a duplicate: %q", i, d)
		}
	}
```

- [ ] **Step 4: Rename and rewrite `TestStoreUpsertMergesBestCandidate` → `TestStoreUpsertDuplicateLinksBestCandidate` (lines 223-255)**

```go
func TestStoreUpsertDuplicateLinksBestCandidate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two existing memories; the new content shares one word with A but is a
	// near-duplicate of B. It must link to B, not the first FTS hit.
	_, _, _, err := s.Upsert(ctx, testProject, "fact",
		"Deployment pipeline uses GitHub Actions runners on the cluster",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (A): %v", err)
	}

	idB, _, _, err := s.Upsert(ctx, testProject, "fact",
		"Ollama embedding worker sweeps unembedded memories every two minutes",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (B): %v", err)
	}

	idNew, dupOf, _, err := s.Upsert(ctx, testProject, "fact",
		"Ollama embedding worker sweeps the unembedded memories every two minutes for cluster projects",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (near-dup of B): %v", err)
	}
	if dupOf != idB {
		t.Fatalf("should link to B (%s), linked to %q", idB, dupOf)
	}
	if idNew == idB {
		t.Errorf("near-duplicate should get its own row distinct from B (%s)", idB)
	}
}
```

- [ ] **Step 5: Rename and rewrite `TestStoreUpsertMergesLengthAsymmetricParaphrases` → `TestStoreUpsertDuplicateLinksLengthAsymmetricParaphrases` (lines 257-295)**

```go
// TestStoreUpsertDuplicateLinksLengthAsymmetricParaphrases covers the failure
// the eval surfaced: the same fact restated at a different length. Each
// restatement must get its own row and link back to a previously-seen row —
// not accumulate as unrelated memories, and not silently overwrite one another.
func TestStoreUpsertDuplicateLinksLengthAsymmetricParaphrases(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	base := "the session cache TTL is 300 seconds"
	restatements := []string{
		"session cache TTL is set to 300 seconds by default",
		"default session cache TTL: 300 seconds, configured in the server config",
		"the session cache TTL value is 300 seconds and it is not currently tunable",
	}

	baseID, _, _, err := s.Upsert(ctx, testProject, "fact", base, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (base): %v", err)
	}
	seenIDs := map[string]bool{baseID: true}
	for i, r := range restatements {
		id, dupOf, _, err := s.Upsert(ctx, testProject, "fact", r, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (restatement %d): %v", i, err)
		}
		if dupOf == "" || !seenIDs[dupOf] {
			t.Errorf("restatement %d must link to a previously-seen row, got dupOf=%q: %q", i, dupOf, r)
		}
		seenIDs[id] = true
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 rows (base + 3 restatements), got %d", len(all))
		for _, m := range all {
			t.Logf("  %s", m.Content)
		}
	}
}
```

- [ ] **Step 6: Rewrite `TestStoreUpsertImportanceCap` (lines 374-403)**

```go
func TestStoreUpsertImportanceCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create with high importance.
	firstID, _, _, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the system", "reflection", 0.95, nil)
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}

	// Upsert a near-duplicate — existing row's importance should be capped at 1.0.
	_, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the entire system design", "reflection", 0.9, nil)
	if err != nil {
		t.Fatalf("Upsert (cap): %v", err)
	}
	if dupOf != firstID {
		t.Fatalf("expected duplicate link to %s, got %q", firstID, dupOf)
	}

	var importance float32
	if err := s.db.QueryRowContext(ctx, `SELECT importance FROM memories WHERE id = ?`, firstID).Scan(&importance); err != nil {
		t.Fatalf("query importance: %v", err)
	}
	if importance > 1.0 {
		t.Errorf("importance should be capped at 1.0, got %f", importance)
	}
}
```

- [ ] **Step 7: Fix the remaining named-`merged` site at line ~2502 (`TestMergeProject`'s neighbor / resolved_at-on-strengthen test)**

Change:
```go
	uid, _, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{uid}); err != nil {
		t.Fatalf("SetResolved upsert row: %v", err)
	}
	gotID, merged, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	if !merged || gotID != uid {
		t.Fatalf("upsert dup did not strengthen existing row: merged=%v id=%s want %s", merged, gotID, uid)
	}
	assertActive(t, s, testProject, uid)
```
to:
```go
	uid, _, _, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{uid}); err != nil {
		t.Fatalf("SetResolved upsert row: %v", err)
	}
	_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", "duplicate detection strengthen path here", "manual", 0.5, nil)
	if err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	if dupOf != uid {
		t.Fatalf("upsert dup did not link to existing row: dupOf=%s want %s", dupOf, uid)
	}
	assertActive(t, s, testProject, uid)
```

(This test's docstring/name refers to "strengthen" — no rename needed, the strengthen-and-clear-resolved_at behavior on the *existing* row is unchanged, only the merge/duplicate distinction shifted.)

- [ ] **Step 8: Run the full store test suite**

Run: `go test ./internal/memory/... -v`
Expected: PASS across all tests in the package, including `TestMergeScore` (untouched — it tests the pure function, not `Upsert`).

---

## Task 8: Fix `mcpserver_test.go` — mechanical arity fixes + 1 semantic fix

**Files:**
- Modify: `internal/mcpserver/mcpserver_test.go:186, 227, 250, 303, 335, 360, 363, 366, 393, 432, 435, 458, 548, 551, 907, 994, 1137`

- [ ] **Step 1: Fix the 16 blank-destructure sites**

Each of these currently reads `if _, _, err := store.Upsert(...)` (lines 186, 227, 250, 335, 360, 363, 366, 432, 435, 548, 551, 907, 994, 1137) — add one more blank:

```go
if _, _, _, err := store.Upsert(ctx, "abc123", "convention", "use nerdctl on node-2 for builds", "manual", 1.0, []string{"nerdctl"}); err != nil {
```
(same pattern for every other blank site listed above — same edit, only the arguments differ per call.)

Lines 393 and 458 read `id, _, err := store.Upsert(...)` — these become:
```go
id, _, _, err := store.Upsert(ctx, "abc123", "gotcha", "watch for nil pointers", "mcp", 0.5, []string{})
```
and
```go
id, _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl", "mcp", 0.8, []string{})
```
respectively (args unchanged from the original call).

- [ ] **Step 2: Fix the 1 named-`merged` site (line 303, `TestSaveAndSearch_EndToEnd`)**

Change:
```go
	id, merged, err := store.Upsert(ctx, "abc123", "pattern", "use context.Background() in tests", "mcp", 0.7, []string{"testing"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
	if merged {
		t.Error("first save should not be merged")
	}
```
to:
```go
	id, dupOf, _, err := store.Upsert(ctx, "abc123", "pattern", "use context.Background() in tests", "mcp", 0.7, []string{"testing"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
	if dupOf != "" {
		t.Error("first save should not report a duplicate")
	}
```

- [ ] **Step 3: Run the full mcpserver test suite**

Run: `go test ./internal/mcpserver/... -v`
Expected: PASS across all tests.

---

## Task 9: Full build, vet, and test pass

- [ ] **Step 1: Build everything**

Run: `go build ./...`
Expected: clean build, no output.

- [ ] **Step 2: Vet everything**

Run: `go vet ./...`
Expected: clean, no output.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/memory/store.go internal/memory/store_test.go internal/provider/provider.go \
        internal/mcpserver/mcpserver.go internal/mcpserver/mcpserver_test.go internal/claudeimport/import.go
git commit -m "feat(memory): make Upsert non-destructive on near-duplicate match"
```

---

## Self-Review

**Spec coverage** (against design doc Area 1, `docs/superpowers/specs/2026-08-02-eval-findings-remediation-design.md:19-38`):
- ✅ Never mutate existing content on match, insert as own row — Task 5.
- ✅ `memory_links` edge with match score, distinct 'duplicate' kind — Task 5 (inline insert), Tasks 1-3 (schema/migration).
- ✅ Existing-row importance/access_count bump preserved — Task 5.
- ✅ `ghost_memory_save` (and `ghost_save_global`) surfaces the match (ID + score) — Task 6.
- ✅ `migrateV3`-style rebuild, `'duplicate'` NOT in `symmetricRelations`, direction preserved (source=new, target=existing) — Tasks 1-3, 5 (verified `symmetricRelations` in `links.go` is untouched by this plan).
- ✅ `provider.MemoryStore` interface updated — Task 4.
- ✅ Broadened `sanitizeFTS` cap and FTS candidate `LIMIT` — Task 5, Step 1 (10→30 words) and the `LIMIT 5`→`LIMIT 15` in the rewritten query.
- ✅ Rewrite of the 4 named dup-merge tests with two-row-plus-link assertions — Task 7, Steps 2, 4, 5, 6.
- ✅ `source='manual'` targets no longer silently drop new content — this is now structurally guaranteed (there's no `CASE WHEN source='manual'` branch left in `Upsert` at all — every save always gets its own row).
- Consolidation via `ghost resolve`/`ghost supersede` — out of scope for this plan (design doc defers it there; no code change needed here).

**Placeholder scan:** no TBD/TODO; every step shows complete before/after code; no "similar to Task N" — Task 8 Step 1 groups 14 structurally-identical one-line edits with one example plus an explicit list of line numbers rather than repeating 14 near-identical diffs, since the transformation is the same 1-character insertion at each site and the arguments are already fully visible in the "Files and Code Sections" grep output above — but if executing this in isolation, look up each line's current content before editing (`grep -n "Upsert(ctx" internal/mcpserver/mcpserver_test.go`) rather than assuming the args shown for the first example apply elsewhere.

**Type/signature consistency:** `Upsert` is `(id string, duplicateOf string, score float64, err error)` everywhere it's declared (provider.go), implemented (store.go), and called (all tasks) — verified consistent across Tasks 4-8.
