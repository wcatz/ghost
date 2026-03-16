package tool

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func testStoreForTool(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	// Register a project for the test path.
	ctx := context.Background()
	pid := projectIDFromPath("/tmp/test-project")
	if err := s.EnsureProject(ctx, pid, "/tmp/test-project", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestMemorySave_HappyPath(t *testing.T) {
	store := testStoreForTool(t)
	exec := makeMemorySaveExec(store)
	ctx := context.Background()

	tests := []struct {
		name       string
		input      memorySaveInput
		wantMerged bool
		wantError  bool
	}{
		{
			name: "basic save",
			input: memorySaveInput{
				Content:    "Go uses goroutines for concurrency",
				Category:   "fact",
				Importance: 0.7,
				Tags:       []string{"go", "concurrency"},
			},
		},
		{
			name: "default importance when zero",
			input: memorySaveInput{
				Content:  "SQLite supports FTS5",
				Category: "architecture",
			},
		},
		{
			name: "importance capped at 1.0",
			input: memorySaveInput{
				Content:    "Over-importance test",
				Category:   "pattern",
				Importance: 5.0,
			},
		},
		{
			name: "nil tags become empty slice",
			input: memorySaveInput{
				Content:    "Tags nil test",
				Category:   "decision",
				Importance: 0.5,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			result := exec(ctx, "/tmp/test-project", raw)
			if result.IsError {
				t.Fatalf("unexpected error: %s", result.Content)
			}
			if !strings.Contains(result.Content, "memory saved") && !strings.Contains(result.Content, "memory strengthened") {
				t.Errorf("unexpected result: %s", result.Content)
			}
		})
	}
}

func TestMemorySave_MergeExisting(t *testing.T) {
	store := testStoreForTool(t)
	exec := makeMemorySaveExec(store)
	ctx := context.Background()

	// Save an initial memory.
	input1, _ := json.Marshal(memorySaveInput{
		Content:    "SQLite supports full-text search via FTS5",
		Category:   "fact",
		Importance: 0.6,
		Tags:       []string{"sqlite"},
	})
	r1 := exec(ctx, "/tmp/test-project", input1)
	if r1.IsError {
		t.Fatalf("first save: %s", r1.Content)
	}
	if !strings.Contains(r1.Content, "memory saved") {
		t.Fatalf("expected 'memory saved', got: %s", r1.Content)
	}

	// Save a similar memory — should merge.
	input2, _ := json.Marshal(memorySaveInput{
		Content:    "SQLite FTS5 provides full-text search capabilities",
		Category:   "fact",
		Importance: 0.5,
		Tags:       []string{"sqlite", "fts"},
	})
	r2 := exec(ctx, "/tmp/test-project", input2)
	if r2.IsError {
		t.Fatalf("merge save: %s", r2.Content)
	}
	if !strings.Contains(r2.Content, "memory strengthened") {
		t.Errorf("expected merge, got: %s", r2.Content)
	}
}

func TestMemorySave_InvalidJSON(t *testing.T) {
	store := testStoreForTool(t)
	exec := makeMemorySaveExec(store)
	ctx := context.Background()

	result := exec(ctx, "/tmp/test-project", json.RawMessage(`{invalid json`))
	if !result.IsError {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(result.Content, "invalid input") {
		t.Errorf("expected 'invalid input' error, got: %s", result.Content)
	}
}

func TestProjectIDFromPath(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"absolute path", "/home/user/project"},
		{"another path", "/tmp/test"},
		{"empty path", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := projectIDFromPath(tc.path)
			if id == "" {
				t.Error("expected non-empty ID")
			}
			// Same path should produce same ID.
			if id2 := projectIDFromPath(tc.path); id != id2 {
				t.Errorf("ID not stable: %s vs %s", id, id2)
			}
		})
	}

	// Different paths should produce different IDs.
	id1 := projectIDFromPath("/path/a")
	id2 := projectIDFromPath("/path/b")
	if id1 == id2 {
		t.Errorf("different paths produced same ID: %s", id1)
	}
}
