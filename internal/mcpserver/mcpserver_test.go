package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// testStore creates an in-memory Store suitable for testing.
func testStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestResolveProjectID_ByName(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := &Server{store: store, logger: logger}

	ctx := context.Background()
	got := srv.resolveProjectID(ctx, "test-project")
	if got != "abc123" {
		t.Errorf("resolveProjectID(name) = %q, want %q", got, "abc123")
	}
}

func TestResolveProjectID_ByID(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := &Server{store: store, logger: logger}

	ctx := context.Background()
	got := srv.resolveProjectID(ctx, "abc123")
	if got != "abc123" {
		t.Errorf("resolveProjectID(id) = %q, want %q", got, "abc123")
	}
}

func TestResolveProjectID_Unknown(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := &Server{store: store, logger: logger}

	ctx := context.Background()
	got := srv.resolveProjectID(ctx, "nonexistent")
	// Should fall through and return the input as-is.
	if got != "nonexistent" {
		t.Errorf("resolveProjectID(unknown) = %q, want %q", got, "nonexistent")
	}
}

func TestResolveProjectID_NameTakesPrecedence(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create a second project where the name matches the first project's ID.
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "def456", "/tmp/second", "abc123"); err != nil {
		t.Fatalf("EnsureProject second: %v", err)
	}

	srv := &Server{store: store, logger: logger}

	// When "abc123" is passed, name lookup should match the second project's name.
	got := srv.resolveProjectID(ctx, "abc123")
	// Name "abc123" maps to project ID "def456".
	if got != "def456" {
		t.Errorf("resolveProjectID should prefer name lookup, got %q, want %q", got, "def456")
	}
}

func TestFormatMemories(t *testing.T) {
	tests := []struct {
		name     string
		memories []memory.Memory
		wantIn   []string
	}{
		{
			name:     "empty",
			memories: nil,
			wantIn:   []string{},
		},
		{
			name: "single memory",
			memories: []memory.Memory{
				{Category: "fact", Importance: 0.7, Content: "test content"},
			},
			wantIn: []string{"[fact]", "0.7", "test content"},
		},
		{
			name: "pinned memory",
			memories: []memory.Memory{
				{Category: "decision", Importance: 0.9, Content: "important decision", Pinned: true},
			},
			wantIn: []string{"[pinned]", "decision", "important decision"},
		},
		{
			name: "memory with tags",
			memories: []memory.Memory{
				{Category: "pattern", Importance: 0.5, Content: "tagged memory", Tags: []string{"go", "test"}},
			},
			wantIn: []string{"tags:", "go", "test", "tagged memory"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := formatMemories(tc.memories)
			for _, want := range tc.wantIn {
				if !strings.Contains(result, want) {
					t.Errorf("formatMemories: expected %q in output, got: %s", want, result)
				}
			}
		})
	}
}

func TestNew_CreatesServer(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := New(store, logger)
	if srv == nil {
		t.Fatal("New returned nil")
	}
	if srv.store == nil {
		t.Error("server store is nil")
	}
	if srv.mcp == nil {
		t.Error("server mcp is nil")
	}
}

func TestSetEmbedder(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger)

	ch := make(chan string, 1)
	mockEmbed := &mockEmbedder{}
	srv.SetEmbedder(mockEmbed, ch)

	if srv.embedder == nil {
		t.Error("embedder not set")
	}
	if srv.projectCh == nil {
		t.Error("projectCh not set")
	}
}

type mockEmbedder struct{}

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// --- Resource tests ---

func TestBuildProjectContext_WithMemories(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger)

	ctx := context.Background()
	// Seed a memory for the test project (ID "abc123").
	if _, _, err := store.Upsert(ctx, "abc123", "convention", "use nerdctl on node-2 for builds", "manual", 1.0, []string{"nerdctl"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	text := srv.buildProjectContext(ctx, "abc123")

	if !strings.Contains(text, "## Memories") {
		t.Errorf("expected '## Memories' header, got: %s", text)
	}
	if !strings.Contains(text, "nerdctl") {
		t.Errorf("expected memory content in output, got: %s", text)
	}
}

func TestBuildProjectContext_Empty(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger)

	ctx := context.Background()
	text := srv.buildProjectContext(ctx, "abc123")

	if text != "No memories found for this project." {
		t.Errorf("expected empty placeholder, got: %s", text)
	}
}

func TestBuildProjectContext_IncludesGlobal(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger)

	ctx := context.Background()
	// Seed a global memory — must ensure _global project exists first.
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	if _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl not docker", "manual", 1.0, []string{}); err != nil {
		t.Fatalf("Upsert global: %v", err)
	}

	// buildProjectContext for abc123 should pull in _global memories too.
	text := srv.buildProjectContext(ctx, "abc123")

	if !strings.Contains(text, "nerdctl") {
		t.Errorf("expected global memory in project context, got: %s", text)
	}
}

func TestBuildProjectContext_IncludesLearnedContext(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger)

	ctx := context.Background()
	if err := store.UpdateLearnedContext(ctx, "abc123", "This is the learned summary.", ""); err != nil {
		t.Fatalf("UpdateLearnedContext: %v", err)
	}
	// Need at least one memory for the memories block to appear — but learned context
	// should appear even with no memories since it's added separately.
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "seed memory", "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	text := srv.buildProjectContext(ctx, "abc123")

	if !strings.Contains(text, "## Learned Context") {
		t.Errorf("expected '## Learned Context' section, got: %s", text)
	}
	if !strings.Contains(text, "learned summary") {
		t.Errorf("expected learned context text, got: %s", text)
	}
}

func TestNew_RegistersResources(t *testing.T) {
	// Verify that New() doesn't panic when registering resources (panics on
	// invalid URI templates or duplicate registrations).
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Should not panic.
	srv := New(store, logger)
	if srv == nil {
		t.Fatal("New returned nil")
	}
}
