package memory

import (
	"context"
	"database/sql"
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
	id, err := s.RecordDecision(ctx, testProject,
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
	if err := s.UpdateTask(ctx, id2, "active", 1, "Updated description"); err != nil {
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
	memID, _, err := s.Upsert(ctx, "test", "fact", "MCP-created memory about deployment", "mcp", 0.7, []string{})
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
	if err := s.ReplaceNonManual(ctx, "_global", replaceMems); err != nil {
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

func TestEnsureProject_AutoMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Simulate MCP creating a project with name-as-ID.
	if err := s.EnsureProject(ctx, "myproject", "myproject", "myproject"); err != nil {
		t.Fatalf("EnsureProject MCP: %v", err)
	}

	// Save a memory under the MCP project.
	_, _, err := s.Upsert(ctx, "myproject", "fact", "deployed on k8s cluster alpha-7", "mcp", 0.8, []string{})
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
