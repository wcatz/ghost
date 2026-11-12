package memory

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestDecayFactor_MatchesSQLSemantics(t *testing.T) {
	cases := []struct {
		category string
		content  string
		pinned   bool
		ageDays  float64
		want     float64
	}{
		{"decision", "", false, 0, 1.0},                    // brand new → no decay
		{"decision", "", false, 30, 0.5},                   // tau=30: 1/(1+30/30)
		{"gotcha", "", false, 30, 0.5},                     // same tier as decision
		{"dependency", "", false, 90, 0.25},                // tau=30: 1/(1+90/30), above 0.15 floor
		{"pattern", "", false, 45, 0.5},                    // tau=45: 1/(1+45/45)
		{"architecture", "", false, 100, 45.0 / 145.0},     // tau=45: 1/(1+100/45), above 0.3 floor
		{"preference", "", false, 1000, 1.0},               // never decays
		{"convention", "", false, 1000, 1.0},               // never decays
		{"fact", "timeless claim", false, 1000, 1.0},       // undated fact never decays
		{"fact", "upgraded on 2026-01-15", false, 45, 0.5}, // dated fact: tau=45
		{"fact", "status 2026-01-15", false, 1000, 0.3},    // dated fact: floor 0.3
		{"decision", "", true, 1000, 1.0},                  // pinned → exempt
		{"gotcha", "", true, 30, 1.0},                      // pinned beats decay
		{"fact", "dated 2026-01-15", true, 1000, 1.0},      // pinned dated fact → exempt
		{"decision", "", false, -5, 1.2},                   // negative age: SQL doesn't clamp, factor > 1
	}
	const eps = 1e-9
	for _, c := range cases {
		got := DecayFactor(c.category, c.content, RetentionProject, c.pinned, c.ageDays)
		if math.Abs(got-c.want) > eps {
			t.Errorf("decayFactor(%q, %q, pinned=%v, age=%.1f) = %v, want %v",
				c.category, c.content, c.pinned, c.ageDays, got, c.want)
		}
	}
}

func TestVectorRoundtrip(t *testing.T) {
	inf := float32(math.Inf(1))
	for name, in := range map[string][]float32{
		"empty": nil,
		"many":  {1, -1, 0, 0.5, -0.5, 100, -100, 1e-10, 1e10, -42, -0.001},
		"inf":   {inf, -inf},
		"nan":   {float32(math.NaN())},
	} {
		b := float32sToBytes(in)
		if len(b) != len(in)*4 {
			t.Fatalf("%s: %d bytes, want %d", name, len(b), len(in)*4)
		}
		got := bytesToFloat32s(b)
		if len(got) != len(in) {
			t.Fatalf("%s: got %v, want %v", name, got, in)
		}
		for i := range in {
			if got[i] != in[i] && !(math.IsNaN(float64(got[i])) && math.IsNaN(float64(in[i]))) {
				t.Errorf("%s[%d]: got %v, want %v", name, i, got[i], in[i])
			}
		}
	}
}

func TestVectorCosine(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 2, 3}, []float32{1, 2, 3}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0, 0}, []float32{-1, 0, 0}, -1},
		{"zero a", []float32{0, 0, 0}, []float32{1, 2, 3}, 0},
		{"zero b", []float32{1, 2, 3}, []float32{0, 0, 0}, 0},
		{"both zero", []float32{0, 0}, []float32{0, 0}, 0},
		{"mismatched", []float32{1, 2}, []float32{1, 2, 3}, 0},
		{"empty", []float32{}, []float32{}, 0},
		{"nil", nil, nil, 0},
		{"45 degrees", []float32{1, 0}, []float32{1, 1}, 1 / math.Sqrt2},
	} {
		if got := float64(cosineSimilarity(c.a, c.b)); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// --- helpers for DB-backed tests ---

func setupTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := NewStore(db, nil)
	ctx := context.Background()

	if err := store.EnsureProject(ctx, "test-proj", "/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, ctx
}

// TestGetByIDsBatchesPastSQLiteVariableLimit is the #608 nit. GetByIDs built a
// single IN list with one placeholder per id, so a request wide enough to pass
// SQLite's SQLITE_MAX_VARIABLE_NUMBER (32766 on the version this tree links,
// and 999 on older builds) came back as an error rather than as rows. The
// callers pass a candidate WINDOW and a pool, so the width is the caller's
// fetch limit rather than anything Ghost chose — and a hydration that errors
// is a retrieval that returns nothing, which reads as "no memories" rather
// than as a failure.
//
// The ids asked for here mostly do not exist, and that is the point: the bug is
// the statement being refused, not a row being missed. The second half seeds
// real rows across a chunk boundary so a fix that batches but drops the tail
// cannot pass.
func TestGetByIDsBatchesPastSQLiteVariableLimit(t *testing.T) {
	store, ctx := setupTestStore(t)

	t.Run("more ids than SQLite has variables", func(t *testing.T) {
		const n = 33000
		ids := make([]string, n)
		for i := range ids {
			ids[i] = "no-such-memory-" + strconv.Itoa(i)
		}
		got, err := store.GetByIDs(ctx, ids)
		if err != nil {
			t.Fatalf("GetByIDs over %d ids: %v", n, err)
		}
		if len(got) != 0 {
			t.Errorf("got %d rows for %d ids that name nothing, want none", len(got), n)
		}
	})

	t.Run("every row comes back across chunk boundaries", func(t *testing.T) {
		// More than the chunk size and more than the old 999 default, so the
		// rows land in at least three statements.
		const n = 1200
		//
		// The rows are inserted directly, in one transaction: what is under test
		// is how GetByIDs chunks its IN list, not how a memory gets written, and
		// twelve hundred committed Create calls cost about a dozen seconds under
		// the race detector for nothing this subtest asserts. The ids are the
		// ones the store minted, so they are real rows across the chunk
		// boundaries, which is what a fix that drops the tail would fail on.
		ids := make([]string, n)
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin seed: %v", err)
		}
		defer tx.Rollback() //nolint:errcheck
		for i := range ids {
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO memories (project_id, category, content, source, importance, tags)
				VALUES ('test-proj', 'fact', ?, 'manual', 0.8, '["test"]') RETURNING id`,
				fmt.Sprintf("memory number %d", i)).Scan(&ids[i]); err != nil {
				t.Fatalf("seed memory %d: %v", i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit seed: %v", err)
		}
		got, err := store.GetByIDs(ctx, ids)
		if err != nil {
			t.Fatalf("GetByIDs over %d ids: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("got %d rows, want all %d", len(got), n)
		}
		seen := make(map[string]bool, len(got))
		for _, m := range got {
			seen[m.ID] = true
		}
		for _, id := range ids {
			if !seen[id] {
				t.Errorf("row %s was dropped, so a chunk boundary lost rows", id)
			}
		}
	})
}

func createTestMemory(t *testing.T, store *Store, ctx context.Context, content string) string {
	t.Helper()
	id, err := store.Create(ctx, "test-proj", Memory{
		Category:   "fact",
		Content:    content,
		Source:     "manual",
		Importance: 0.8,
		Tags:       []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create memory: %v", err)
	}
	return id
}

// --- StoreEmbedding + SearchVector roundtrip ---

func TestVectorStoreAndSearch(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "Go is a compiled language")
	id2 := createTestMemory(t, store, ctx, "Python is an interpreted language")
	id3 := createTestMemory(t, store, ctx, "Rust has zero-cost abstractions")

	// Store embeddings — make id1's vector similar to the query.
	vec1 := []float32{0.9, 0.1, 0.0}
	vec2 := []float32{0.1, 0.9, 0.0}
	vec3 := []float32{0.0, 0.1, 0.9}

	for _, tc := range []struct {
		id  string
		vec []float32
	}{
		{id1, vec1},
		{id2, vec2},
		{id3, vec3},
	} {
		if err := store.StoreEmbedding(ctx, tc.id, tc.vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%s): %v", tc.id, err)
		}
	}

	// Query vector close to vec1.
	query := []float32{0.95, 0.05, 0.0}
	results, err := store.SearchVector(ctx, "test-proj", query, 3)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// First result should be id1 (most similar to query).
	if results[0].MemoryID != id1 {
		t.Errorf("expected first result to be %s, got %s", id1, results[0].MemoryID)
	}

	// Scores should be in descending order.
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("results not sorted: score[%d]=%v > score[%d]=%v",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

// --- StoreEmbedding upsert behavior ---

// --- DeleteEmbedding ---

func TestVectorDeleteEmbedding(t *testing.T) {
	store, ctx := setupTestStore(t)

	id := createTestMemory(t, store, ctx, "to be deleted")
	vec := []float32{1.0, 0.0, 0.0}
	if err := store.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	if err := store.DeleteEmbedding(ctx, id); err != nil {
		t.Fatalf("DeleteEmbedding: %v", err)
	}

	// After deletion, search should return nothing.
	results, err := store.SearchVector(ctx, "test-proj", vec, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results after delete, got %d", len(results))
	}
}

// --- UnembeddedMemoryIDs ---

func TestVectorUnembeddedMemoryIDs(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "embedded memory")
	id2 := createTestMemory(t, store, ctx, "unembedded memory")
	_ = id2

	// Embed only id1.
	if err := store.StoreEmbedding(ctx, id1, []float32{1.0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	ids, err := store.UnembeddedMemoryIDs(ctx, "test-proj", "", 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 unembedded ID, got %d", len(ids))
	}
	if ids[0] != id2 {
		t.Errorf("expected unembedded ID %s, got %s", id2, ids[0])
	}
}

// --- GetMemoryContent ---

func TestVectorGetMemoryContent(t *testing.T) {
	store, ctx := setupTestStore(t)

	content := "the content of this memory"
	id := createTestMemory(t, store, ctx, content)

	got, err := store.GetMemoryContent(ctx, id)
	if err != nil {
		t.Fatalf("GetMemoryContent: %v", err)
	}
	if got != content {
		t.Fatalf("expected %q, got %q", content, got)
	}
}

// --- GetByIDs ---

func TestVectorGetByIDs_Valid(t *testing.T) {
	store, ctx := setupTestStore(t)

	id1 := createTestMemory(t, store, ctx, "memory one")
	id2 := createTestMemory(t, store, ctx, "memory two")

	memories, err := store.GetByIDs(ctx, []string{id1, id2})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}

	// Verify both IDs are present (order not guaranteed).
	found := map[string]bool{}
	for _, m := range memories {
		found[m.ID] = true
	}
	if !found[id1] || !found[id2] {
		t.Errorf("expected IDs %s and %s, got %v", id1, id2, found)
	}
}

// --- SearchHybrid ---

func TestVectorSearchHybrid_FTSOnly(t *testing.T) {
	store, ctx := setupTestStore(t)

	createTestMemory(t, store, ctx, "Go programming language concurrency")
	createTestMemory(t, store, ctx, "Python data science libraries")

	// queryVec=nil should fall back to FTS-only.
	results, err := store.SearchHybrid(ctx, "test-proj", "Go concurrency", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid FTS-only: %v", err)
	}

	// Should find at least the Go memory via FTS.
	if len(results) == 0 {
		t.Fatal("expected at least 1 FTS result, got 0")
	}

	// First result should be the Go-related memory.
	foundGo := false
	for _, m := range results {
		if m.Content == "Go programming language concurrency" {
			foundGo = true
			break
		}
	}
	if !foundGo {
		t.Error("expected to find Go concurrency memory in FTS results")
	}
}

// makeScoredMemory is makeMemory with an explicit importance, so a test can
// pin the pre-demote ranking instead of inheriting makeMemory's fixed 0.7.
func makeScoredMemory(t *testing.T, s *Store, content string, importance float32) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: importance,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

// TestProductionSearchDemotesSuperseded is the regression guard for the eval
// finding that a superseded memory could outrank its replacement in
// ghost_memory_search. The demote mechanism shipped behind
// SearchParams.SupersedeDemote but no production caller ever set it, so it was
// dead code in the live server. These are the entry points the MCP tools
// actually use — SearchHybrid (ghost_memory_search) and SearchHybridAll
// (ghost_search_all) — exercised with no query vector, the FTS-only fallback
// that runs whenever Ollama is unavailable.
func TestProductionSearchDemotesSuperseded(t *testing.T) {
	if !DefaultSearchParams().SupersedeDemote {
		t.Fatal("production default must consume supersedes links")
	}

	s := testStore(t)
	ctx := context.Background()

	// Importance is what forces the baseline ordering: without it the two
	// near-identical contents tie and the winner is decided by FTS rank on a
	// two-token difference, which is not stable enough to build an assertion
	// on. The stale memory is deliberately the higher-scoring one so the only
	// thing that can move it below its replacement is the demote pass.
	stale := makeScoredMemory(t, s, "the payments service deploys to the eu-west-1 region", 0.9)
	fresh := makeScoredMemory(t, s, "the payments service deploys to the us-east-1 region", 0.5)

	before, err := s.SearchHybrid(ctx, testProject, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid (before): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected both versions retrieved, got %d", len(before))
	}
	// The demote is only provable if the stale one wins on base score.
	if before[0].ID != stale {
		t.Fatalf("baseline must favor the stale memory, got %s first (stale=%s)", before[0].ID, stale)
	}

	if err := s.CreateLink(ctx, fresh, stale, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	after, err := s.SearchHybrid(ctx, testProject, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid (after): %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("demote must not drop results: got %d, want 2", len(after))
	}
	if after[0].ID != fresh || after[1].ID != stale {
		t.Errorf("superseded memory must rank below its replacement; got %s then %s (fresh=%s stale=%s)",
			after[0].ID, after[1].ID, fresh, stale)
	}

	allProjects, err := s.SearchHybridAll(ctx, "payments service deploys region", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	if len(allProjects) != 2 {
		t.Fatalf("cross-project demote must not drop results: got %d, want 2", len(allProjects))
	}
	if allProjects[0].ID != fresh {
		t.Errorf("ghost_search_all must demote superseded too; got %s first", allProjects[0].ID)
	}
}

// TestSearchVectorDropsNonPositiveSimilarity: RRF weights by rank alone, so a
// non-positive cosine candidate must be dropped before fusion rather than claim
// the full vector weight.
func TestSearchVectorDropsNonPositiveSimilarity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pos := makeMemory(t, s, "positive similarity memory")
	neg := makeMemory(t, s, "negative similarity memory")

	q := []float32{1, 0, 0}
	if err := s.StoreEmbedding(ctx, pos, q, "test"); err != nil {
		t.Fatalf("StoreEmbedding pos: %v", err)
	}
	if err := s.StoreEmbedding(ctx, neg, []float32{-1, 0, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding neg: %v", err)
	}

	got, err := s.SearchVector(ctx, testProject, q, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	var sawPos bool
	for _, sc := range got {
		if sc.MemoryID == neg {
			t.Errorf("negative-similarity candidate %s must be dropped, got %+v", neg, got)
		}
		if sc.MemoryID == pos {
			sawPos = true
		}
	}
	if !sawPos {
		t.Errorf("positive-similarity candidate %s missing from %+v", pos, got)
	}
}

// TestVectorSearchDimensionMismatchIsSurfaced pins the surfacing fix: when the
// embedding model changes, stored vectors no longer match the query dimension
// and every row is skipped — previously indistinguishable from an empty index.
func TestVectorSearchDimensionMismatchIsSurfaced(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var buf bytes.Buffer
	store := NewStore(db, slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "test-proj", "/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id := createTestMemory(t, store, ctx, "some memory")
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, "stale-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	results, err := store.SearchVector(ctx, "test-proj", []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for mismatched dimensions, got %d", len(results))
	}

	out := buf.String()
	if !strings.Contains(out, "dimension") || !strings.Contains(out, "stale-model") {
		t.Fatalf("dimension mismatch was not surfaced: %q", out)
	}
}
