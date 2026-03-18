package memory

import (
	"context"
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
	t.Cleanup(func() { db.Close() })

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
	id1, merged, err := s.Upsert(ctx, testProject, "fact", "SQLite supports full-text search via FTS5", "reflection", 0.6, []string{"sqlite"})
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}
	if merged {
		t.Error("first Upsert should not merge")
	}
	if id1 == "" {
		t.Fatal("first Upsert returned empty ID")
	}

	// Second Upsert with overlapping content — should merge.
	id2, merged, err := s.Upsert(ctx, testProject, "fact", "SQLite FTS5 provides full-text search capabilities", "reflection", 0.5, []string{"sqlite", "fts"})
	if err != nil {
		t.Fatalf("Upsert (merge): %v", err)
	}
	if !merged {
		t.Error("second Upsert should have merged")
	}
	if id2 != id1 {
		t.Errorf("merged ID should match original: got %s, want %s", id2, id1)
	}

	// Verify importance was strengthened: 0.6 + (0.5 * 0.2) = 0.7
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory after merge, got %d", len(all))
	}
	expected := float32(0.6 + 0.5*0.2)
	if diff := all[0].Importance - expected; diff > 0.01 || diff < -0.01 {
		t.Errorf("expected importance ~%f, got %f", expected, all[0].Importance)
	}
}

func TestStoreUpsertImportanceCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create with high importance.
	_, _, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the system", "reflection", 0.95, nil)
	if err != nil {
		t.Fatalf("Upsert (new): %v", err)
	}

	// Upsert again — should be capped at 1.0.
	_, merged, err := s.Upsert(ctx, testProject, "fact", "Critical architecture pattern for the entire system design", "reflection", 0.9, nil)
	if err != nil {
		t.Fatalf("Upsert (cap): %v", err)
	}
	if !merged {
		t.Error("expected merge")
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(all))
	}
	if all[0].Importance > 1.0 {
		t.Errorf("importance should be capped at 1.0, got %f", all[0].Importance)
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
		err := s.ReplaceNonManual(ctx, testProject, []Memory{})
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
		if err := s.ReplaceNonManual(ctx, testProject, replacement); err != nil {
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
