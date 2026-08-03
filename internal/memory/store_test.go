package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

const testProject = "test-project"

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestStoreCreate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	m := Memory{
		Category:   "fact",
		Content:    "Go uses goroutines for concurrency",
		Source:     "manual",
		Importance: 0.7,
		Tags:       []string{"go", "concurrency"},
	}

	id, err := s.Create(ctx, testProject, m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty ID")
	}

	// Verify the memory is stored by retrieving all memories.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(all))
	}
	if all[0].ID != id {
		t.Errorf("expected ID %s, got %s", id, all[0].ID)
	}
	if all[0].Content != m.Content {
		t.Errorf("expected content %q, got %q", m.Content, all[0].Content)
	}
	if all[0].Category != m.Category {
		t.Errorf("expected category %q, got %q", m.Category, all[0].Category)
	}
	if all[0].Importance != m.Importance {
		t.Errorf("expected importance %f, got %f", m.Importance, all[0].Importance)
	}
	if all[0].Source != m.Source {
		t.Errorf("expected source %q, got %q", m.Source, all[0].Source)
	}
}

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

func TestStoreUpsertNoMergeOnSharedWord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Two unrelated same-category memories sharing a single common word
	// ("Phase"). Reproduces the 2026-07-19 live bug: the FTS OR-probe alone
	// treated any one-word overlap as a duplicate and silently swallowed
	// the second save.
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
	if id2 == id1 {
		t.Error("second Upsert should have created a distinct memory")
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(all))
	}
}

func TestStoreUpsertNoMergeBelowThreshold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Same topic vocabulary but genuinely different facts: token overlap is
	// real yet below the 0.5 Jaccard gate, so both must survive.
	_, _, _, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite busy timeout must be set on the read-only hook connection",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (first): %v", err)
	}

	_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha",
		"SQLite FTS5 rank ordering is unstable across identical RRF scores in hybrid search",
		"mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if dupOf != "" {
		t.Error("below-threshold overlap must not report a duplicate")
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(all))
	}
}

func TestStoreUpsertNoMergeOnTokenFreeContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Contents whose tokens are all single characters produce EMPTY token
	// sets (tokenizeContent keeps len>1 only) while still FTS-matching each
	// other (sanitizeFTS keeps single-char words). jaccard(∅,∅) = 1.0, so
	// without a guard these unrelated saves would spuriously merge.
	_, _, _, err := s.Upsert(ctx, testProject, "fact", "a b c", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert (first): %v", err)
	}

	_, dupOf, _, err := s.Upsert(ctx, testProject, "fact", "a x y", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert (second): %v", err)
	}
	if dupOf != "" {
		t.Error("token-free contents must never report a duplicate")
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(all))
	}
}

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

// TestStoreUpsertOverlapLegDoesNotOverMerge guards the risk the overlap leg
// introduces: containment is trivially satisfied by a short save whose few
// tokens all appear in a longer, unrelated memory. Distinct facts must survive.
func TestStoreUpsertOverlapLegDoesNotOverMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A long memory that lexically contains every token of the short saves
	// below, but states a different fact from each of them.
	long := "the embedding worker retries the Ollama request twice before giving " +
		"up and leaves the memory unembedded until the next sweep"
	if _, _, _, err := s.Upsert(ctx, testProject, "gotcha", long, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (long): %v", err)
	}

	shorts := []string{
		"the Ollama request twice",
		"the next sweep",
		"the memory unembedded",
	}
	for i, sh := range shorts {
		_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", sh, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (short %d): %v", i, err)
		}
		if dupOf != "" {
			t.Errorf("short contained save %d must not report a duplicate for a longer unrelated memory: %q", i, sh)
		}
	}

	// Comparably-sized, same-topic, genuinely different facts — the shape the
	// overlap leg's gates do NOT exclude — must still survive on score alone.
	distinct := []string{
		"the embedding worker skips memories longer than the model context window",
		"the embedding worker sweeps the unembedded memories every two minutes",
		"the embedding worker writes vectors into the memory embeddings table",
	}
	for i, d := range distinct {
		_, dupOf, _, err := s.Upsert(ctx, testProject, "gotcha", d, "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert (distinct %d): %v", i, err)
		}
		if dupOf != "" {
			t.Errorf("distinct fact %d sharing topic vocabulary must not report a duplicate: %q", i, d)
		}
	}
}

func TestMergeScore(t *testing.T) {
	tests := []struct {
		name      string
		a, b      string
		wantMerge bool
	}{
		{"identical", "cache TTL is 300 seconds", "cache TTL is 300 seconds", true},
		{"length-asymmetric paraphrase", "cache TTL is 300 seconds",
			"the cache TTL is set to 300 seconds in config", true},
		{"empty vs empty", "a b c", "x y z", false},
		{"empty vs populated", "a b c", "the embedding worker sweeps every minute", false},
		{"short containment below min tokens", "pnpm workspaces",
			"we use pnpm workspaces because npm workspaces hoists transitive deps wrongly", false},
		{"unrelated same topic", "SQLite busy timeout must be set on the read-only hook connection",
			"SQLite FTS5 rank ordering is unstable across identical RRF scores in hybrid search", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeScore(tokenizeContent(tt.a), tokenizeContent(tt.b)) > 0
			if got != tt.wantMerge {
				t.Errorf("mergeScore(%q, %q) merge = %v, want %v (jaccard=%.3f overlap=%.3f)",
					tt.a, tt.b, got, tt.wantMerge,
					jaccard(tokenizeContent(tt.a), tokenizeContent(tt.b)),
					overlapCoefficient(tokenizeContent(tt.a), tokenizeContent(tt.b)))
			}
		})
	}
}

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

func TestStoreDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    "Temporary memory to delete",
		Source:     "chat",
		Importance: 0.3,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it's gone.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 memories after delete, got %d", len(all))
	}

	// Deleting a non-existent memory should return an error.
	err = s.Delete(ctx, "nonexistent-id")
	if err == nil {
		t.Error("expected error deleting non-existent memory")
	}
}

func TestStoreReplaceNonManual(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("empty set guard", func(t *testing.T) {
		err := s.ReplaceNonManual(ctx, testProject, []Memory{}, "")
		if err == nil {
			t.Fatal("expected error for empty set")
		}
		if !strings.Contains(err.Error(), "empty set") {
			t.Errorf("error should mention empty set, got: %v", err)
		}
	})

	t.Run("manual memories survive replace", func(t *testing.T) {
		// Create one manual and one reflection memory.
		manualID, err := s.Create(ctx, testProject, Memory{
			Category:   "preference",
			Content:    "I prefer tabs over spaces",
			Source:     "manual",
			Importance: 0.8,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create manual: %v", err)
		}
		_, err = s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    "Old reflection fact that should be replaced",
			Source:     "reflection",
			Importance: 0.5,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create reflection: %v", err)
		}

		// Replace non-manual with a new set.
		replacement := []Memory{
			{Category: "fact", Content: "New consolidated fact", Importance: 0.6, Tags: []string{}},
		}
		if err := s.ReplaceNonManual(ctx, testProject, replacement, ""); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}

		// Should have manual + 1 new reflection = 2.
		if len(all) != 2 {
			t.Fatalf("expected 2 memories, got %d", len(all))
		}

		foundManual := false
		foundReplacement := false
		for _, m := range all {
			if m.ID == manualID && m.Source == "manual" {
				foundManual = true
			}
			if m.Content == "New consolidated fact" && m.Source == "reflection" {
				foundReplacement = true
			}
		}
		if !foundManual {
			t.Error("manual memory should survive replace")
		}
		if !foundReplacement {
			t.Error("replacement memory should be present")
		}
	})

	t.Run("preserves memories saved concurrently with consolidation", func(t *testing.T) {
		s := testStore(t)

		// Simulate ghost reflect's flow: capture "now" before the consolidation
		// input is fetched, then a ghost_memory_save lands on the live MCP
		// server while the (slow) consolidation round trip is in flight.
		since, err := s.CurrentTimestamp(ctx)
		if err != nil {
			t.Fatalf("CurrentTimestamp: %v", err)
		}

		concurrentID, err := s.Create(ctx, testProject, Memory{
			Category:   "gotcha",
			Content:    "saved while reflection was running",
			Source:     "mcp",
			Importance: 0.7,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create concurrent memory: %v", err)
		}

		replacement := []Memory{
			{Category: "fact", Content: "consolidator output, stale snapshot", Importance: 0.6, Tags: []string{}},
		}
		if err := s.ReplaceNonManual(ctx, testProject, replacement, since); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}

		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}

		var foundConcurrent, foundReplacement bool
		for _, m := range all {
			if m.Content == "saved while reflection was running" && m.Source == "mcp" {
				foundConcurrent = true
			}
			if m.Content == "consolidator output, stale snapshot" {
				foundReplacement = true
			}
		}
		if !foundConcurrent {
			t.Errorf("memory %s saved during consolidation was dropped by ReplaceNonManual", concurrentID)
		}
		if !foundReplacement {
			t.Error("consolidator output should still be present")
		}
	})
}

func TestStoreSearchFTS(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Insert several memories with distinct content.
	contents := []string{
		"Kubernetes pods run containers",
		"Go channels enable concurrency",
		"SQLite is an embedded database engine",
	}
	for _, c := range contents {
		_, err := s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    c,
			Source:     "reflection",
			Importance: 0.5,
			Tags:       []string{},
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", c, err)
		}
	}

	// Search for "Kubernetes".
	results, err := s.SearchFTS(ctx, testProject, "Kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'Kubernetes'")
	}
	if !strings.Contains(results[0].Content, "Kubernetes") {
		t.Errorf("expected Kubernetes result, got %q", results[0].Content)
	}

	// Search for "concurrency" — should find the Go channels memory.
	results, err = s.SearchFTS(ctx, testProject, "concurrency", 10)
	if err != nil {
		t.Fatalf("SearchFTS concurrency: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least 1 result for 'concurrency'")
	}
	if !strings.Contains(results[0].Content, "concurrency") {
		t.Errorf("expected concurrency result, got %q", results[0].Content)
	}

	// Search for something that doesn't exist.
	results, err = s.SearchFTS(ctx, testProject, "blockchain", 10)
	if err != nil {
		t.Fatalf("SearchFTS no results: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'blockchain', got %d", len(results))
	}
}

func TestStoreSearchFTSSpecialCharacters(t *testing.T) {
	// Verify that FTS5 operators in content don't break queries.
	s := testStore(t)
	ctx := context.Background()

	_, err := s.Create(ctx, testProject, Memory{
		Category:   "gotcha",
		Content:    "NEAR AND OR NOT are FTS5 operators that need quoting",
		Source:     "manual",
		Importance: 0.5,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// sanitizeFTS is called internally by Upsert. Test via SearchFTS with operator-laden input.
	// This should not error — special chars should be handled.
	results, err := s.SearchFTS(ctx, testProject, "operators", 10)
	if err != nil {
		t.Fatalf("SearchFTS with special chars: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'operators' query")
	}
}

func TestSanitizeFTS(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // expected output
	}{
		{
			name:  "plain words",
			input: "hello world",
			want:  `"hello" OR "world"`,
		},
		{
			name:  "keeps single-char words",
			input: "a Go is great",
			want:  `"a" OR "Go" OR "is" OR "great"`,
		},
		{
			name:  "strips punctuation from edges",
			input: "(hello) [world]!",
			want:  `"hello" OR "world"`,
		},
		{
			name:  "strips FTS operators",
			input: "NEAR AND OR NOT test",
			want:  `"NEAR" OR "AND" OR "OR" OR "NOT" OR "test"`,
		},
		{
			name:  "empty input",
			input: "",
			want:  `""`,
		},
		{
			name:  "only punctuation",
			input: "* + - !",
			want:  `""`,
		},
		{
			name:  "escapes mid-token quote instead of leaving an unbalanced literal",
			input: `fo"o`,
			want:  `"fo""o"`,
		},
		{
			name:  "escapes mid-token quotes while preserving exact identifiers",
			input: `evil"NEAR"foo 192.168.9.150 sealed-secrets`,
			want:  `"evil""NEAR""foo" OR "192.168.9.150" OR "sealed-secrets"`,
		},
		{
			name:  "limits to 10 words",
			input: "one two three four five six seven eight nine ten eleven twelve",
			want:  `"one" OR "two" OR "three" OR "four" OR "five" OR "six" OR "seven" OR "eight" OR "nine" OR "ten"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFTS(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeFTS(%q)\n  got:  %s\n  want: %s", tc.input, got, tc.want)
			}
		})
	}
}

// TestSanitizeFTSNWiderCap verifies Upsert's duplicate-recall probe (which
// calls sanitizeFTSN(content, 30)) gets a wider word cap than plain search
// (sanitizeFTS, capped at 10) — see sanitizeFTSN's doc comment for why the
// caps must differ.
func TestSanitizeFTSNWiderCap(t *testing.T) {
	input := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twentyone twentytwo twentythree twentyfour twentyfive twentysix twentyseven twentyeight twentynine thirty thirtyone thirtytwo"
	want := `"one" OR "two" OR "three" OR "four" OR "five" OR "six" OR "seven" OR "eight" OR "nine" OR "ten" OR "eleven" OR "twelve" OR "thirteen" OR "fourteen" OR "fifteen" OR "sixteen" OR "seventeen" OR "eighteen" OR "nineteen" OR "twenty" OR "twentyone" OR "twentytwo" OR "twentythree" OR "twentyfour" OR "twentyfive" OR "twentysix" OR "twentyseven" OR "twentyeight" OR "twentynine" OR "thirty"`
	got := sanitizeFTSN(input, 30)
	if got != want {
		t.Errorf("sanitizeFTSN(%q, 30)\n  got:  %s\n  want: %s", input, got, want)
	}
}

func TestStoreTouch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "Touchable memory for access tracking",
		Source:     "reflection",
		Importance: 0.5,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify initial access_count is 0.
	before, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if before[0].AccessCount != 0 {
		t.Fatalf("expected initial access_count 0, got %d", before[0].AccessCount)
	}

	// Touch it.
	if err := s.Touch(ctx, []string{id}); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	after, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if after[0].AccessCount != 1 {
		t.Errorf("expected access_count 1 after Touch, got %d", after[0].AccessCount)
	}
	if after[0].LastAccessed == nil {
		t.Error("expected last_accessed to be set after Touch")
	} else {
		// Verify last_accessed is roughly now.
		parsed, parseErr := time.Parse(time.RFC3339, *after[0].LastAccessed)
		if parseErr != nil {
			t.Errorf("failed to parse last_accessed %q: %v", *after[0].LastAccessed, parseErr)
		} else if time.Since(parsed) > 5*time.Second {
			t.Errorf("last_accessed seems too old: %v", parsed)
		}
	}

	// Touch again — should increment to 2.
	if err := s.Touch(ctx, []string{id}); err != nil {
		t.Fatalf("Touch (second): %v", err)
	}
	after2, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if after2[0].AccessCount != 2 {
		t.Errorf("expected access_count 2 after second Touch, got %d", after2[0].AccessCount)
	}

	// Touch with empty slice — should be a no-op.
	if err := s.Touch(ctx, []string{}); err != nil {
		t.Fatalf("Touch (empty): %v", err)
	}
}

func TestStoreTogglePin(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category:   "decision",
		Content:    "Pin-toggle test memory",
		Source:     "manual",
		Importance: 0.5,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initially not pinned.
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if all[0].Pinned {
		t.Error("expected initially not pinned")
	}

	// Pin it.
	if err := s.TogglePin(ctx, id, true); err != nil {
		t.Fatalf("TogglePin (pin): %v", err)
	}
	all, err = s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if !all[0].Pinned {
		t.Error("expected pinned after TogglePin(true)")
	}

	// Unpin it.
	if err := s.TogglePin(ctx, id, false); err != nil {
		t.Fatalf("TogglePin (unpin): %v", err)
	}
	all, err = s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if all[0].Pinned {
		t.Error("expected not pinned after TogglePin(false)")
	}
}

func TestStoreGetTopMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create memories with varying importance and categories.
	mems := []Memory{
		{Category: "fact", Content: "Low importance fact", Source: "reflection", Importance: 0.2, Tags: []string{}},
		{Category: "preference", Content: "High importance preference", Source: "manual", Importance: 0.9, Tags: []string{}},
		{Category: "pattern", Content: "Medium pattern", Source: "reflection", Importance: 0.5, Tags: []string{}},
	}
	for _, m := range mems {
		if _, err := s.Create(ctx, testProject, m); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// Get top 2 — should be ordered by composite score (importance * decay * pin boost).
	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(top))
	}
	// Highest importance preference (no decay) should be first.
	if top[0].Content != "High importance preference" {
		t.Errorf("expected highest importance first, got %q", top[0].Content)
	}
}

func TestStoreGetTopMemoriesIncludesGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a global memory.
	if err := s.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "fact", Content: "Global fact visible everywhere",
		Source: "manual", Importance: 0.8, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create global: %v", err)
	}

	// Create a project-scoped memory.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Project-scoped fact",
		Source: "manual", Importance: 0.7, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create project: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(top) != 2 {
		t.Fatalf("expected 2 memories (global + project), got %d", len(top))
	}
}

func TestStoreGetTopMemoriesPinnedBoost(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create two memories — lower importance but pinned should rank higher.
	id1, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Pinned low importance",
		Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Unpinned higher importance",
		Source: "manual", Importance: 0.6, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Pin the first one — 0.5 * 1.5 = 0.75 > 0.6.
	if err := s.TogglePin(ctx, id1, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}

	top, err := s.GetTopMemories(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if top[0].Content != "Pinned low importance" {
		t.Errorf("expected pinned memory first, got %q", top[0].Content)
	}
}

func TestStoreGetByCategory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create memories in different categories.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "Watch out for nil maps",
		Source: "manual", Importance: 0.7, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Go uses goroutines",
		Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Query only gotchas.
	results, err := s.GetByCategory(ctx, testProject, "gotcha", 10)
	if err != nil {
		t.Fatalf("GetByCategory: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 gotcha, got %d", len(results))
	}
	if results[0].Category != "gotcha" {
		t.Errorf("expected category 'gotcha', got %q", results[0].Category)
	}

	// No results for unused category.
	results, err = s.GetByCategory(ctx, testProject, "dependency", 10)
	if err != nil {
		t.Fatalf("GetByCategory empty: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for 'dependency', got %d", len(results))
	}
}

func TestStoreGetByCategoryIncludesGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "convention", Content: "Global convention",
		Source: "manual", Importance: 0.8, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create global: %v", err)
	}

	results, err := s.GetByCategory(ctx, testProject, "convention", 10)
	if err != nil {
		t.Fatalf("GetByCategory: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 global convention, got %d", len(results))
	}
}

func TestStoreSearchFTSAll(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a second project.
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Create memories in different projects with a shared keyword.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Kubernetes uses etcd for storage",
		Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, "other-project", Memory{
		Category: "fact", Content: "Kubernetes clusters need networking",
		Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// SearchFTSAll should find both.
	results, err := s.SearchFTSAll(ctx, "Kubernetes", 10)
	if err != nil {
		t.Fatalf("SearchFTSAll: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 cross-project results, got %d", len(results))
	}
}

func TestStoreCountMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	count, err := s.CountMemories(ctx, testProject)
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 memories, got %d", count)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "memory number",
			Source: "manual", Importance: 0.5, Tags: []string{},
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	count, err = s.CountMemories(ctx, testProject)
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 memories, got %d", count)
	}
}

func TestStoreListProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// testStore already created one project.
	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].ID != testProject {
		t.Errorf("expected project ID %q, got %q", testProject, projects[0].ID)
	}
	if projects[0].Name != "test" {
		t.Errorf("expected project name 'test', got %q", projects[0].Name)
	}

	// Add another project.
	if err := s.EnsureProject(ctx, "second", "/tmp/second", "alpha"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	projects, err = s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	// Ordered by name ASC — "alpha" before "test".
	if projects[0].Name != "alpha" {
		t.Errorf("expected 'alpha' first (sorted), got %q", projects[0].Name)
	}
}

func TestStoreResolveProjectByName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// testStore created project with name "test".
	id, err := s.ResolveProjectByName(ctx, "test")
	if err != nil {
		t.Fatalf("ResolveProjectByName: %v", err)
	}
	if id != testProject {
		t.Errorf("expected %q, got %q", testProject, id)
	}

	// Non-existent name returns empty string, no error.
	id, err = s.ResolveProjectByName(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ResolveProjectByName nonexistent: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty string for nonexistent, got %q", id)
	}
}

func TestStoreIncrementInteraction(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// EnsureProject creates ghost_state row with interaction_count=0.
	count, err := s.IncrementInteraction(ctx, testProject)
	if err != nil {
		t.Fatalf("IncrementInteraction: %v", err)
	}
	if count != 1 {
		t.Errorf("expected count 1, got %d", count)
	}

	count, err = s.IncrementInteraction(ctx, testProject)
	if err != nil {
		t.Fatalf("IncrementInteraction (2nd): %v", err)
	}
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
}

func TestStoreLearnedContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Initially empty.
	lc, err := s.GetLearnedContext(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLearnedContext: %v", err)
	}
	if lc != "" {
		t.Errorf("expected empty learned context, got %q", lc)
	}

	// Update it.
	if err := s.UpdateLearnedContext(ctx, testProject, "Go project with SQLite", "consolidated summary"); err != nil {
		t.Fatalf("UpdateLearnedContext: %v", err)
	}

	lc, err = s.GetLearnedContext(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLearnedContext after update: %v", err)
	}
	if lc != "Go project with SQLite" {
		t.Errorf("expected 'Go project with SQLite', got %q", lc)
	}

	// Non-existent project returns empty, no error.
	lc, err = s.GetLearnedContext(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetLearnedContext nonexistent: %v", err)
	}
	if lc != "" {
		t.Errorf("expected empty for nonexistent, got %q", lc)
	}
}

func TestStoreConversations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a conversation.
	convID, err := s.CreateConversation(ctx, testProject, "chat")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if convID == "" {
		t.Fatal("CreateConversation returned empty ID")
	}

	// Append messages.
	if err := s.AppendMessage(ctx, convID, "user", "Hello ghost"); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "assistant", "Hello! How can I help?"); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "user", "What is Go?"); err != nil {
		t.Fatalf("AppendMessage user 2: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "assistant", "Go is a programming language."); err != nil {
		t.Fatalf("AppendMessage assistant 2: %v", err)
	}

	// GetConversationMessages.
	msgs, err := s.GetConversationMessages(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversationMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Hello ghost" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content != "Go is a programming language." {
		t.Errorf("unexpected last message: %+v", msgs[3])
	}

	// GetLatestConversation.
	latestID, err := s.GetLatestConversation(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLatestConversation: %v", err)
	}
	if latestID != convID {
		t.Errorf("expected latest conv %q, got %q", convID, latestID)
	}

	// GetLatestConversation for non-existent project.
	_, err = s.GetLatestConversation(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent project conversation")
	}
}

func TestStoreGetRecentExchanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, testProject, "chat")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Insert 3 pairs of user/assistant messages with distinct timestamps.
	// SQLite datetime('now') has only second precision, so we insert with
	// explicit timestamps to guarantee ordering.
	msgs := []struct {
		role, content, ts string
	}{
		{"user", "first question", "2026-01-01 00:00:01"},
		{"assistant", "first answer", "2026-01-01 00:00:02"},
		{"user", "second question", "2026-01-01 00:00:03"},
		{"assistant", "second answer", "2026-01-01 00:00:04"},
		{"user", "third question", "2026-01-01 00:00:05"},
		{"assistant", "third answer", "2026-01-01 00:00:06"},
	}
	for _, m := range msgs {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
			convID, m.role, m.content, m.ts)
		if err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	// Get last 2 exchanges.
	pairs, err := s.GetRecentExchanges(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetRecentExchanges: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}

	// Should be the last 2, in chronological order.
	if pairs[0][0] != "second question" || pairs[0][1] != "second answer" {
		t.Errorf("expected second exchange first, got %v", pairs[0])
	}
	if pairs[1][0] != "third question" || pairs[1][1] != "third answer" {
		t.Errorf("expected third exchange second, got %v", pairs[1])
	}

	// Get 0 exchanges.
	pairs, err = s.GetRecentExchanges(ctx, testProject, 0)
	if err != nil {
		t.Fatalf("GetRecentExchanges(0): %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs, got %d", len(pairs))
	}
}

func TestStoreRecordUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	usage := TokenUsage{
		InputTokens:   1000,
		OutputTokens:  500,
		CacheCreation: 200,
		CacheRead:     100,
		CostUSD:       0.05,
	}

	if err := s.RecordUsage(ctx, testProject, "claude-opus-4-6", usage); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	// Verify by reading directly from DB.
	var inputTokens, outputTokens int
	var costUSD float64
	err := s.db.QueryRowContext(ctx, `
		SELECT input_tokens, output_tokens, cost_usd FROM token_usage
		WHERE project_id = ? LIMIT 1
	`, testProject).Scan(&inputTokens, &outputTokens, &costUSD)
	if err != nil {
		t.Fatalf("query token_usage: %v", err)
	}
	if inputTokens != 1000 {
		t.Errorf("expected 1000 input tokens, got %d", inputTokens)
	}
	if outputTokens != 500 {
		t.Errorf("expected 500 output tokens, got %d", outputTokens)
	}
	if costUSD != 0.05 {
		t.Errorf("expected cost 0.05, got %f", costUSD)
	}
}

func TestStoreSetOnSave(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	called := make(chan string, 1)
	s.SetOnSave(func(projectID string) {
		called <- projectID
	})

	// Create should trigger callback.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "Callback test",
		Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case pid := <-called:
		if pid != testProject {
			t.Errorf("expected project %q, got %q", testProject, pid)
		}
	case <-time.After(time.Second):
		t.Error("onSave callback not called after Create")
	}
}

func TestStoreClose(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After close, operations should fail.
	_, err = s.ListProjects(context.Background())
	if err == nil {
		t.Error("expected error after Close")
	}
}

func TestStoreDecisions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Record a decision.
	id, memID, err := s.RecordDecision(ctx, testProject,
		"Use SQLite for storage",
		"SQLite provides embedded persistence with FTS5",
		"Simple, no external dependencies",
		[]string{"PostgreSQL", "BoltDB"},
		[]string{"storage", "database"},
	)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Fatal("RecordDecision returned empty ID")
	}
	if memID == "" || memID == id {
		t.Fatalf("RecordDecision returned invalid memory ID: %q (decision ID: %q)", memID, id)
	}

	// List decisions.
	decisions, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}

	d := decisions[0]
	if d.Title != "Use SQLite for storage" {
		t.Errorf("expected title 'Use SQLite for storage', got %q", d.Title)
	}
	if d.Decision != "SQLite provides embedded persistence with FTS5" {
		t.Errorf("unexpected decision text: %q", d.Decision)
	}
	if d.Rationale != "Simple, no external dependencies" {
		t.Errorf("unexpected rationale: %q", d.Rationale)
	}
	if len(d.Alternatives) != 2 || d.Alternatives[0] != "PostgreSQL" {
		t.Errorf("unexpected alternatives: %v", d.Alternatives)
	}
	if len(d.Tags) != 2 || d.Tags[0] != "storage" {
		t.Errorf("unexpected tags: %v", d.Tags)
	}
	if d.Status != "active" {
		t.Errorf("expected status 'active', got %q", d.Status)
	}

	// Filter by status.
	active, err := s.ListDecisions(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 active decision, got %d", len(active))
	}

	superseded, err := s.ListDecisions(ctx, testProject, "superseded", 10)
	if err != nil {
		t.Fatalf("ListDecisions superseded: %v", err)
	}
	if len(superseded) != 0 {
		t.Errorf("expected 0 superseded decisions, got %d", len(superseded))
	}

	// RecordDecision also creates a memory — verify.
	results, err := s.SearchFTS(ctx, testProject, "SQLite storage", 10)
	if err != nil {
		t.Fatalf("SearchFTS decision memory: %v", err)
	}
	foundDecisionMemory := false
	for _, r := range results {
		if r.Category == "decision" && strings.Contains(r.Content, "Use SQLite for storage") {
			foundDecisionMemory = true
		}
	}
	if !foundDecisionMemory {
		t.Error("RecordDecision should also create a decision-category memory")
	}
}

// TestSupersedeDecision covers the eval finding that a reversed decision kept
// reporting status "active" and could head ghost_decisions_list ahead of the
// decision that replaced it. The status/superseded_by columns existed but had
// no writer, and ListDecisions ordered purely by created_at DESC.
func TestSupersedeDecision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	oldID, _, err := s.RecordDecision(ctx, testProject,
		"Use Redis for the job queue", "Redis lists as the queue backend",
		"Already deployed for caching", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (old): %v", err)
	}
	newID, _, err := s.RecordDecision(ctx, testProject,
		"Reverse: use Postgres for the job queue", "SKIP LOCKED on Postgres",
		"Redis lost jobs on failover", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (new): %v", err)
	}

	if err := s.SupersedeDecision(ctx, testProject, oldID, newID); err != nil {
		t.Fatalf("SupersedeDecision: %v", err)
	}

	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(all))
	}
	// The live decision must come first, and the reversed one must report its
	// real status and point at its replacement.
	if all[0].ID != newID {
		t.Errorf("live decision should rank first, got %s", all[0].ID)
	}
	if all[1].ID != oldID {
		t.Fatalf("superseded decision should rank last, got %s", all[1].ID)
	}
	if all[1].Status != "superseded" {
		t.Errorf("reversed decision status = %q, want superseded", all[1].Status)
	}
	if all[1].SupersededBy != newID {
		t.Errorf("superseded_by = %q, want %s", all[1].SupersededBy, newID)
	}
	if all[0].Status != "active" {
		t.Errorf("replacement status = %q, want active", all[0].Status)
	}

	// Status filters now actually partition.
	active, err := s.ListDecisions(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(active) != 1 || active[0].ID != newID {
		t.Errorf("expected only the replacement to be active, got %v", active)
	}

	// Re-running repoints rather than failing.
	if err := s.SupersedeDecision(ctx, testProject, oldID, newID); err != nil {
		t.Errorf("re-running SupersedeDecision should be safe: %v", err)
	}
}

// TestListDecisionsLimitPicksNewestNotLiveOnly pins the ordering change to
// presentation only: the limit still selects the newest N decisions, so
// demoting superseded ones cannot silently push them out of the window. A
// caller that never sees the reversed decision cannot tell a decision was
// reversed at all.
func TestListDecisionsLimitPicksNewestNotLiveOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Oldest recorded first, so the last one recorded is the newest.
	var ids []string
	for i := range 4 {
		id, _, err := s.RecordDecision(ctx, testProject,
			fmt.Sprintf("Decision %d", i), fmt.Sprintf("do thing %d", i), "because", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision %d: %v", i, err)
		}
		ids = append(ids, id)
		// RecordDecision stamps datetime('now'), so all four land in the same
		// second and created_at DESC would be arbitrary. Space them out.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE decisions SET created_at = datetime('now', ?) WHERE id = ?`,
			fmt.Sprintf("-%d days", 10-i), id); err != nil {
			t.Fatalf("backdate %d: %v", i, err)
		}
	}
	// Supersede the two newest, which are the ones inside a limit-2 window.
	for _, i := range []int{2, 3} {
		if err := s.SupersedeDecision(ctx, testProject, ids[i], ids[0]); err != nil {
			t.Fatalf("SupersedeDecision %d: %v", i, err)
		}
	}

	got, err := s.ListDecisions(ctx, testProject, "", 2)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 decisions, got %d", len(got))
	}
	// Window membership is still "the newest 2" — both superseded — not "the
	// newest 2 active ones".
	inWindow := map[string]bool{got[0].ID: true, got[1].ID: true}
	if !inWindow[ids[2]] || !inWindow[ids[3]] {
		t.Errorf("limit must select the newest decisions regardless of status; got %s, %s (want %s, %s)",
			got[0].ID, got[1].ID, ids[2], ids[3])
	}
}

func TestSupersedeDecisionRejectsBadInput(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, err := s.RecordDecision(ctx, testProject, "T", "D", "R", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	if err := s.SupersedeDecision(ctx, testProject, id, id); err == nil {
		t.Error("a decision must not be able to supersede itself")
	}
	if err := s.SupersedeDecision(ctx, testProject, id, "no-such-decision"); err == nil {
		t.Error("superseding by an unknown decision must fail")
	}
	if err := s.SupersedeDecision(ctx, testProject, "no-such-decision", id); err == nil {
		t.Error("superseding an unknown decision must fail")
	}

	// The failed attempts must not have flipped anything.
	all, err := s.ListDecisions(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(all) != 1 || all[0].Status != "active" {
		t.Errorf("failed supersede attempts must leave status untouched, got %v", all)
	}
}

func TestRecordDecisionPersistsMemoryRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, memID, err := s.RecordDecision(ctx, testProject,
		"Use SQLite", "SQLite for persistence", "Simple and embedded",
		[]string{"postgres"}, []string{"db"})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty decision ID")
	}
	if memID == "" || memID == id {
		t.Fatalf("expected distinct non-empty memory ID, got %q (decision ID: %q)", memID, id)
	}

	// The memory row must exist under memID — verify GetByIDs finds it
	// directly, not just via a content search (that's the whole point of
	// returning it).
	mems, err := s.GetByIDs(ctx, []string{memID})
	if err != nil {
		t.Fatalf("GetByIDs(memID): %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("GetByIDs(memID): got %d rows, want 1", len(mems))
	}
	if mems[0].Category != "decision" || !strings.Contains(mems[0].Content, "SQLite") {
		t.Fatalf("memory row at memID has unexpected content: %+v", mems[0])
	}

	// Also verify search finds it (existing coverage, unchanged).
	results, err := s.SearchFTS(ctx, testProject, "SQLite persistence", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	found := false
	for _, m := range results {
		if m.Category == "decision" && strings.Contains(m.Content, "SQLite") {
			found = true
		}
	}
	if !found {
		t.Error("RecordDecision: memory row missing from search after commit")
	}
}

func TestStoreTasks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create tasks.
	id1, err := s.CreateTask(ctx, testProject, "Fix bug", "Null pointer in handler", 1)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if id1 == "" {
		t.Fatal("CreateTask returned empty ID")
	}

	id2, err := s.CreateTask(ctx, testProject, "Add feature", "New memory search", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// List all tasks.
	tasks, err := s.ListTasks(ctx, testProject, "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	// Ordered by priority ASC — priority 1 first.
	if tasks[0].Title != "Fix bug" {
		t.Errorf("expected 'Fix bug' first (priority 1), got %q", tasks[0].Title)
	}

	// Filter by status — both should be "pending" (default).
	pending, err := s.ListTasks(ctx, testProject, "pending", 10)
	if err != nil {
		t.Fatalf("ListTasks pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("expected 2 pending tasks, got %d", len(pending))
	}

	// Complete a task.
	if err := s.CompleteTask(ctx, id1, "Fixed in PR #42"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	done, err := s.ListTasks(ctx, testProject, "done", 10)
	if err != nil {
		t.Fatalf("ListTasks done: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("expected 1 done task, got %d", len(done))
	}
	if done[0].Notes != "Fixed in PR #42" {
		t.Errorf("expected notes 'Fixed in PR #42', got %q", done[0].Notes)
	}
	if done[0].CompletedAt == "" {
		t.Error("expected completed_at to be set")
	}

	// Update a task.
	updStatus, updPriority, updDesc := "active", 1, "Updated description"
	if _, err := s.UpdateTask(ctx, id2, &updStatus, &updPriority, &updDesc); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	active, err := s.ListTasks(ctx, testProject, "active", 10)
	if err != nil {
		t.Fatalf("ListTasks active: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
	}
	if active[0].Description != "Updated description" {
		t.Errorf("expected updated description, got %q", active[0].Description)
	}
}

func TestStoreTaskIDPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	fullID, err := s.CreateTask(ctx, testProject, "Prefix me", "Resolved by short ID", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	prefix := fullID[:8]

	// GetTask resolves an 8-char prefix (what the session hook and task list show).
	got, err := s.GetTask(ctx, prefix)
	if err != nil {
		t.Fatalf("GetTask by prefix: %v", err)
	}
	if got.ID != fullID {
		t.Errorf("GetTask by prefix resolved %q, want %q", got.ID, fullID)
	}

	// UpdateTask and CompleteTask resolve prefixes too.
	prefixStatus, prefixPriority, prefixDesc := "active", 1, "updated via prefix"
	if _, err := s.UpdateTask(ctx, prefix, &prefixStatus, &prefixPriority, &prefixDesc); err != nil {
		t.Fatalf("UpdateTask by prefix: %v", err)
	}
	got, err = s.GetTask(ctx, fullID)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if got.Status != "active" || got.Description != "updated via prefix" {
		t.Errorf("UpdateTask by prefix did not apply: status=%q desc=%q", got.Status, got.Description)
	}
	if err := s.CompleteTask(ctx, prefix, "done via prefix"); err != nil {
		t.Fatalf("CompleteTask by prefix: %v", err)
	}
	got, err = s.GetTask(ctx, fullID)
	if err != nil {
		t.Fatalf("GetTask after complete: %v", err)
	}
	if got.Status != "done" || got.Notes != "done via prefix" {
		t.Errorf("CompleteTask by prefix did not apply: status=%q notes=%q", got.Status, got.Notes)
	}

	// Ambiguous prefix: two tasks sharing the first 8 chars must error, not
	// pick one arbitrarily.
	for _, id := range []string{"AAAABBBB000000000000000000000001", "AAAABBBB000000000000000000000002"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO tasks (id, project_id, title) VALUES (?, ?, ?)`, id, testProject, "twin "+id[len(id)-1:]); err != nil {
			t.Fatalf("insert fixture task: %v", err)
		}
	}
	if _, err := s.GetTask(ctx, "AAAABBBB"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("GetTask ambiguous prefix: want ambiguity error, got %v", err)
	}
	if err := s.CompleteTask(ctx, "AAAABBBB", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("CompleteTask ambiguous prefix: want ambiguity error, got %v", err)
	}
	// A longer, unique prefix disambiguates.
	got, err = s.GetTask(ctx, "AAAABBBB000000000000000000000001")
	if err != nil {
		t.Fatalf("GetTask exact ID among twins: %v", err)
	}
	if got.Title != "twin 1" {
		t.Errorf("expected twin 1, got %q", got.Title)
	}

	// Unknown IDs now fail loudly everywhere — CompleteTask/UpdateTask used to
	// silently no-op on a missing ID.
	if _, err := s.GetTask(ctx, "ZZZZ9999"); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Errorf("GetTask unknown: want not-found error, got %v", err)
	}
	if err := s.CompleteTask(ctx, "ZZZZ9999", ""); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Errorf("CompleteTask unknown: want not-found error, got %v", err)
	}
	unknownStatus, unknownPriority, unknownDesc := "active", 1, ""
	if _, err := s.UpdateTask(ctx, "ZZZZ9999", &unknownStatus, &unknownPriority, &unknownDesc); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Errorf("UpdateTask unknown: want not-found error, got %v", err)
	}

	// Empty ID is rejected, never treated as a match-everything prefix.
	if _, err := s.GetTask(ctx, ""); err == nil {
		t.Error("GetTask empty id: want error, got nil")
	}
}

func TestStoreGetLatestConversationNoRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, err := s.GetLatestConversation(ctx, testProject)
	if err == nil {
		t.Error("expected error for no conversations")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestMergeProject(t *testing.T) {
	s := testStore(t) // creates testProject ("test-project") at "/tmp/test"
	ctx := context.Background()

	// Create an MCP-style duplicate with name-as-ID.
	if err := s.EnsureProject(ctx, "test", "test", "dup-project"); err != nil {
		t.Fatalf("EnsureProject dup: %v", err)
	}

	// Seed data under the duplicate project.
	memID, _, _, err := s.Upsert(ctx, "test", "fact", "MCP-created memory about deployment", "mcp", 0.7, []string{})
	if err != nil {
		t.Fatalf("Upsert under dup: %v", err)
	}
	_, err = s.CreateTask(ctx, "test", "Fix MCP task", "", 2)
	if err != nil {
		t.Fatalf("CreateTask under dup: %v", err)
	}

	// Merge old→new.
	if err := s.MergeProject(ctx, "test", testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	// Memory should now belong to testProject.
	mems, err := s.GetByIDs(ctx, []string{memID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(mems) != 1 || mems[0].ProjectID != testProject {
		t.Errorf("expected memory reassigned to %q, got project_id=%q", testProject, mems[0].ProjectID)
	}

	// Old project should be gone.
	projects, _ := s.ListProjects(ctx)
	for _, p := range projects {
		if p.ID == "test" {
			t.Error("old project should be deleted after merge")
		}
	}

	// Tasks should be reassigned.
	tasks, _ := s.ListTasks(ctx, testProject, "", 30)
	found := false
	for _, task := range tasks {
		if task.Title == "Fix MCP task" {
			found = true
		}
	}
	if !found {
		t.Error("task not merged to new project")
	}
}

func TestMergeProject_SameID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.MergeProject(ctx, testProject, testProject); err != nil {
		t.Fatalf("MergeProject same ID should be no-op: %v", err)
	}
}

func TestSeedGlobalMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Seed should create _global project and insert seed memories.
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	// Verify _global project exists.
	projects, _ := s.ListProjects(ctx)
	found := false
	for _, p := range projects {
		if p.ID == "_global" {
			found = true
		}
	}
	if !found {
		t.Fatal("_global project not created")
	}

	// Verify seed memories are present, pinned, and manual source.
	mems, err := s.GetAll(ctx, "_global", 50)
	if err != nil {
		t.Fatalf("GetAll _global: %v", err)
	}
	if len(mems) == 0 {
		t.Fatal("no seed memories found")
	}

	var coAuthorFound bool
	for _, m := range mems {
		if strings.Contains(m.Content, "Co-Authored-By") {
			coAuthorFound = true
			if m.Source != "manual" {
				t.Errorf("seed memory source = %q, want 'manual'", m.Source)
			}
			if !m.Pinned {
				t.Error("seed memory should be pinned")
			}
			if m.Importance != 1.0 {
				t.Errorf("seed memory importance = %v, want 1.0", m.Importance)
			}
		}
	}
	if !coAuthorFound {
		t.Error("Co-Authored-By seed memory not found")
	}

	// Run again — should be idempotent (no duplicates).
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories (2nd call): %v", err)
	}
	mems2, _ := s.GetAll(ctx, "_global", 50)
	if len(mems2) != len(mems) {
		t.Errorf("idempotency broken: %d memories after 2nd seed (was %d)", len(mems2), len(mems))
	}

	// Verify consolidation cannot remove it: ReplaceNonManual skips manual source.
	replaceMems := []Memory{
		{ProjectID: "_global", Category: "fact", Content: "some new fact", Source: "reflection", Importance: 0.5},
	}
	if err := s.ReplaceNonManual(ctx, "_global", replaceMems, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	memsAfter, _ := s.GetAll(ctx, "_global", 50)
	var seedSurvived bool
	for _, m := range memsAfter {
		if strings.Contains(m.Content, "Co-Authored-By") {
			seedSurvived = true
		}
	}
	if !seedSurvived {
		t.Error("seed memory was deleted by ReplaceNonManual — consolidation protection broken")
	}
}

func TestEnsureProject_EmptyPath_NoUniqueConflict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Simulate MCP calling EnsureProject with empty path for two different projects.
	// Before the fix, passing path="" caused a UNIQUE constraint violation on
	// projects.path when the second project was saved (both would have path="").
	if err := s.EnsureProject(ctx, "a7293a04b38a", "", "dingo"); err != nil {
		t.Fatalf("EnsureProject dingo: %v", err)
	}
	if err := s.EnsureProject(ctx, "b1234567890c", "", "roller"); err != nil {
		t.Fatalf("EnsureProject roller: %v", err)
	}

	// Calling EnsureProject again for the same project must also be idempotent.
	if err := s.EnsureProject(ctx, "a7293a04b38a", "", "dingo"); err != nil {
		t.Fatalf("EnsureProject dingo (repeat): %v", err)
	}

	projects, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	found := 0
	for _, p := range projects {
		if p.ID == "a7293a04b38a" || p.ID == "b1234567890c" {
			// path must equal id for non-filesystem projects.
			if p.Path != p.ID {
				t.Errorf("project %s: expected path == id, got path=%q", p.ID, p.Path)
			}
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 projects, found %d", found)
	}
}

func TestStoreUpdateMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newID := func(t *testing.T) string {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category:   "fact",
			Content:    "Original content about the deploy pipeline",
			Source:     "mcp",
			Importance: 0.7,
			Tags:       []string{"deploy"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}

	getOne := func(t *testing.T, id string) Memory {
		t.Helper()
		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs(%s): %v (n=%d)", id, err, len(mems))
		}
		return mems[0]
	}

	t.Run("content only, others preserved", func(t *testing.T) {
		id := newID(t)
		content := "Corrected content about the deploy pipeline"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if m.Content != content {
			t.Errorf("content = %q, want %q", m.Content, content)
		}
		if m.Category != "fact" || m.Importance != 0.7 || m.Source != "mcp" {
			t.Errorf("unchanged fields clobbered: category=%q importance=%f source=%q",
				m.Category, m.Importance, m.Source)
		}
		if len(m.Tags) != 1 || m.Tags[0] != "deploy" {
			t.Errorf("tags clobbered: %v", m.Tags)
		}
	})

	t.Run("category and importance", func(t *testing.T) {
		id := newID(t)
		cat := "gotcha"
		imp := float32(0.9)
		if err := s.UpdateMemory(ctx, testProject, id, nil, &cat, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if m.Category != "gotcha" || m.Importance != 0.9 {
			t.Errorf("category=%q importance=%f, want gotcha/0.9", m.Category, m.Importance)
		}
		if m.Content != "Original content about the deploy pipeline" {
			t.Errorf("content clobbered: %q", m.Content)
		}
	})

	t.Run("nil tags preserve, empty tags clear", func(t *testing.T) {
		id := newID(t)
		imp := float32(0.5)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory (nil tags): %v", err)
		}
		if m := getOne(t, id); len(m.Tags) != 1 {
			t.Errorf("nil tags should preserve, got %v", m.Tags)
		}
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil, []string{}); err != nil {
			t.Fatalf("UpdateMemory (empty tags): %v", err)
		}
		if m := getOne(t, id); len(m.Tags) != 0 {
			t.Errorf("empty tags should clear, got %v", m.Tags)
		}
	})

	t.Run("content change invalidates embedding and link scan", func(t *testing.T) {
		id := newID(t)
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		if err := s.MarkLinkScanned(ctx, id); err != nil {
			t.Fatalf("MarkLinkScanned: %v", err)
		}
		content := "Entirely rewritten content"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if _, err := s.GetEmbedding(ctx, id); err == nil {
			t.Error("embedding should be deleted after content change")
		}
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM link_scans WHERE memory_id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count link_scans: %v", err)
		}
		if n != 0 {
			t.Error("link_scans row should be deleted after content change")
		}
	})

	t.Run("non-content change keeps embedding", func(t *testing.T) {
		id := newID(t)
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		imp := float32(0.4)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, &imp, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if _, err := s.GetEmbedding(ctx, id); err != nil {
			t.Error("embedding should survive a non-content update")
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		content := "x"
		err := s.UpdateMemory(ctx, testProject, "does-not-exist", &content, nil, nil, nil)
		if err == nil {
			t.Error("expected error for unknown memory id")
		}
	})

	t.Run("pinned survives update", func(t *testing.T) {
		id := newID(t)
		if err := s.TogglePin(ctx, id, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}
		content := "Pinned memory, corrected"
		if err := s.UpdateMemory(ctx, testProject, id, &content, nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		if m := getOne(t, id); !m.Pinned {
			t.Error("pinned flag should survive an update")
		}
	})

	t.Run("tags replaced with new values", func(t *testing.T) {
		id := newID(t)
		if err := s.UpdateMemory(ctx, testProject, id, nil, nil, nil, []string{"alpha", "beta"}); err != nil {
			t.Fatalf("UpdateMemory: %v", err)
		}
		m := getOne(t, id)
		if len(m.Tags) != 2 || m.Tags[0] != "alpha" || m.Tags[1] != "beta" {
			t.Errorf("tags = %v, want [alpha beta]", m.Tags)
		}
	})

	t.Run("wrong project cannot update", func(t *testing.T) {
		id := newID(t)
		if err := s.EnsureProject(ctx, "other-proj", "/tmp/other-proj", "other-proj"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		content := "hijacked"
		if err := s.UpdateMemory(ctx, "other-proj", id, &content, nil, nil, nil); err == nil {
			t.Error("expected error updating another project's memory")
		}
		if m := getOne(t, id); m.Content == "hijacked" {
			t.Error("content must be unchanged after cross-project update attempt")
		}
	})
}

func TestStorePromoteToGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	t.Run("moves memory preserving id, pin, importance, embedding", func(t *testing.T) {
		id, err := s.Create(ctx, testProject, Memory{
			Category:   "preference",
			Content:    "Always use two-space YAML indentation",
			Source:     "mcp",
			Importance: 0.8,
			Tags:       []string{"yaml"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.TogglePin(ctx, id, true); err != nil {
			t.Fatalf("TogglePin: %v", err)
		}
		vec := make([]float32, 8)
		vec[0] = 1
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}

		if err := s.PromoteToGlobal(ctx, testProject, id); err != nil {
			t.Fatalf("PromoteToGlobal: %v", err)
		}

		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
		}
		m := mems[0]
		if m.ProjectID != "_global" {
			t.Errorf("project = %q, want _global", m.ProjectID)
		}
		if !m.Pinned || m.Importance != 0.8 {
			t.Errorf("pin/importance not preserved: pinned=%v importance=%f", m.Pinned, m.Importance)
		}
		if _, err := s.GetEmbedding(ctx, id); err != nil {
			t.Error("embedding should survive promotion")
		}
	})

	t.Run("works without prior _global row", func(t *testing.T) {
		// testStore never seeds _global — the first subtest exercised the
		// inline ensure; this one proves a fresh store promotes fine too.
		s2 := testStore(t)
		id, err := s2.Create(ctx, testProject, Memory{
			Category: "fact", Content: "promotable", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s2.PromoteToGlobal(ctx, testProject, id); err != nil {
			t.Fatalf("PromoteToGlobal on fresh store: %v", err)
		}

		mems, err := s2.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
		}
		if mems[0].ProjectID != "_global" {
			t.Errorf("project = %q, want _global", mems[0].ProjectID)
		}
	})

	t.Run("unknown id errors", func(t *testing.T) {
		if err := s.PromoteToGlobal(ctx, testProject, "does-not-exist"); err == nil {
			t.Error("expected error for unknown memory id")
		}
	})

	t.Run("wrong project cannot promote", func(t *testing.T) {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "stays put", Source: "mcp", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.EnsureProject(ctx, "other-proj2", "/tmp/other-proj2", "other-proj2"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if err := s.PromoteToGlobal(ctx, "other-proj2", id); err == nil {
			t.Error("expected error promoting another project's memory")
		}
		mems, err := s.GetByIDs(ctx, []string{id})
		if err != nil || len(mems) != 1 {
			t.Fatalf("GetByIDs: %v", err)
		}
		if mems[0].ProjectID != testProject {
			t.Errorf("memory moved to %q, must stay in %q", mems[0].ProjectID, testProject)
		}
	})
}

func TestEnsureProject_AutoMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Simulate MCP creating a project with name-as-ID.
	if err := s.EnsureProject(ctx, "myproject", "myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject MCP: %v", err)
	}

	// Save a memory under the MCP project.
	_, _, _, err := s.Upsert(ctx, "myproject", "fact", "deployed on k8s cluster alpha-7", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Simulate orchestrator creating the real project (abs path, hash ID).
	hashID := "abc123def456" // deterministic fake hash
	if err := s.EnsureProject(ctx, hashID, "/home/wayne/git/myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject orchestrator: %v", err)
	}

	// The MCP project should have been auto-merged.
	projects, _ := s.ListProjects(ctx)
	for _, p := range projects {
		if p.ID == "myproject" {
			t.Error("MCP project should have been merged away")
		}
	}

	// Memory should now be under the hash ID.
	mems, _ := s.GetTopMemories(ctx, hashID, 10)
	found := false
	for _, m := range mems {
		if strings.Contains(m.Content, "alpha-7") {
			found = true
		}
	}
	if !found {
		t.Error("expected MCP memory to be reassigned to hash-ID project")
	}
}

func TestResolveCandidatesAndSetResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Eligible: a resolvable gotcha.
	evID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create evidence: %v", err)
	}
	// Exempt by category.
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "convention", Content: "never push to main", Source: "manual", Importance: 0.9,
	}); err != nil {
		t.Fatalf("create convention: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "preference", Content: "user prefers tabs", Source: "manual", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create preference: %v", err)
	}
	// Exempt by pin.
	pinID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "pinned gotcha stays", Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if err := s.TogglePin(ctx, pinID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}

	// Candidates exclude convention, preference, and pinned rows.
	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ID != evID {
		ids := make([]string, len(cands))
		for i, c := range cands {
			ids[i] = c.ID + "/" + c.Category
		}
		t.Fatalf("candidates = %v, want exactly [%s/gotcha]", ids, evID)
	}

	// Marking resolved removes it from the candidate set (idempotent re-run).
	if _, err := s.SetResolved(ctx, []string{evID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	cands, err = s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates after SetResolved: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates after resolve = %d, want 0", len(cands))
	}
}

// TestSetResolvedRechecksEligibilityAtWriteTime guards the TOCTOU window
// between ResolveCandidates (read) and SetResolved (write): a candidate
// pinned or recategorized into an exempt bucket after classification started
// must not be stamped, even though its ID was in the confirmed batch.
func TestSetResolvedRechecksEligibilityAtWriteTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	pinnedLate, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "kill experiment returned NO-GO", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	recategorizedLate, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "fixed in PR #210, removed", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stillEligible, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "shipped and archived", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate state changing between ResolveCandidates and SetResolved: pin
	// one, recategorize another into an exempt bucket.
	if err := s.TogglePin(ctx, pinnedLate, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	category := "convention"
	if err := s.UpdateMemory(ctx, testProject, recategorizedLate, nil, &category, nil, nil); err != nil {
		t.Fatalf("recategorize: %v", err)
	}

	n, err := s.SetResolved(ctx, []string{pinnedLate, recategorizedLate, stillEligible})
	if err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if n != 1 {
		t.Errorf("SetResolved returned %d, want 1 (only stillEligible)", n)
	}

	assertActive(t, s, testProject, pinnedLate)
	assertActive(t, s, testProject, recategorizedLate)

	cands, err := s.ResolveCandidates(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	for _, c := range cands {
		if c.ID == stillEligible {
			t.Errorf("stillEligible %s should be resolved and excluded from candidates", stillEligible)
		}
	}
}

func TestGetTopMemoriesExcludesResolved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	activeID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "active gotcha keep", Source: "manual", Importance: 0.6,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}
	resolvedID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "resolved evidence drop", Source: "manual", Importance: 0.9,
	})
	if err != nil {
		t.Fatalf("create resolved: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{resolvedID}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	// Ranked browse drops the resolved memory even though it has higher importance.
	top, err := s.GetTopMemories(ctx, testProject, 25)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	for _, m := range top {
		if m.ID == resolvedID {
			t.Errorf("resolved memory %s must not appear in GetTopMemories", resolvedID)
		}
	}
	var sawActive bool
	for _, m := range top {
		if m.ID == activeID {
			sawActive = true
		}
	}
	if !sawActive {
		t.Errorf("active memory %s missing from GetTopMemories", activeID)
	}

	// ...but it is still searchable.
	hits, err := s.SearchFTS(ctx, testProject, "resolved evidence", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	var sawInSearch bool
	for _, m := range hits {
		if m.ID == resolvedID {
			sawInSearch = true
		}
	}
	if !sawInSearch {
		t.Errorf("resolved memory %s must remain searchable", resolvedID)
	}
}

func TestUnresolveOnWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// UpdateMemory clears resolved_at.
	id, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "resumed via update", Source: "manual", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	newContent := "resumed via update — now with more detail"
	if err := s.UpdateMemory(ctx, testProject, id, &newContent, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	assertActive(t, s, testProject, id)

	// Upsert of a near-duplicate (strengthen branch) clears resolved_at.
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
}

// assertActive fails if the memory's resolved_at is not NULL.
func assertActive(t *testing.T, s *Store, projectID, id string) {
	t.Helper()
	var resolvedAt sql.NullString
	if err := s.db.QueryRow(
		`SELECT resolved_at FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&resolvedAt); err != nil {
		t.Fatalf("read resolved_at for %s: %v", id, err)
	}
	if resolvedAt.Valid {
		t.Errorf("memory %s should be active (resolved_at NULL), got %q", id, resolvedAt.String)
	}
}

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
