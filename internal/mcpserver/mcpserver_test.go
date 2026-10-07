package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// testStore creates an in-memory Store suitable for testing.
func testStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)

	ctx := context.Background()
	if err := s.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func TestResolveProject_ByName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id, name, err := store.ResolveProject(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveProject(name): %v", err)
	}
	if id != "abc123" || name != "test-project" {
		t.Errorf("ResolveProject(name) = (%q, %q), want (%q, %q)", id, name, "abc123", "test-project")
	}
}

func TestResolveProject_ByID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id, name, err := store.ResolveProject(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveProject(id): %v", err)
	}
	if id != "abc123" || name != "test-project" {
		t.Errorf("ResolveProject(id) = (%q, %q), want (%q, %q)", id, name, "abc123", "test-project")
	}
}

func TestResolveProject_Unknown(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id, name, err := store.ResolveProject(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ResolveProject(unknown): %v", err)
	}
	// No match should return empty id/name rather than echoing the input.
	if id != "" || name != "" {
		t.Errorf("ResolveProject(unknown) = (%q, %q), want (\"\", \"\")", id, name)
	}
}

func TestResolveProject_IDTakesPrecedenceOverName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create a second project whose name matches the first project's ID.
	if err := store.EnsureProject(ctx, "def456", "/tmp/second", "abc123"); err != nil {
		t.Fatalf("EnsureProject second: %v", err)
	}

	// When "abc123" is passed, ID lookup should match the first project's ID
	// directly rather than falling through to a name match.
	id, name, err := store.ResolveProject(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "test-project" {
		t.Errorf("ResolveProject should prefer ID lookup, got (%q, %q), want (%q, %q)", id, name, "abc123", "test-project")
	}
}

func TestGhostTaskCreate_RejectsUnknownProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_task_create",
		Arguments: map[string]any{"project_id": "nonexistent-project", "title": "should not be created"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_task_create: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result for unknown project, got: %+v", result.Content)
	}

	tasks, err := store.ListTasks(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("task must not be persisted against an empty project id, found %d", len(tasks))
	}
}

func TestGhostDecisionRecord_RejectsUnknownProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_decision_record",
		Arguments: map[string]any{
			"project_id": "nonexistent-project",
			"title":      "should not be recorded",
			"decision":   "some decision",
			"rationale":  "some rationale",
		},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_decision_record: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result for unknown project, got: %+v", result.Content)
	}

	decisions, err := store.ListDecisions(ctx, "", "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Errorf("decision must not be persisted against an empty project id, found %d", len(decisions))
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
				{ID: "ABC123", Category: "fact", Importance: 0.7, Content: "test content"},
			},
			wantIn: []string{"[fact]", "`ABC123`", "0.7", "test content"},
		},
		{
			name: "pinned memory",
			memories: []memory.Memory{
				{ID: "DEF456", Category: "decision", Importance: 0.9, Content: "important decision", Pinned: true},
			},
			wantIn: []string{"`DEF456`", "[pinned]", "decision", "important decision"},
		},
		{
			name: "memory with tags",
			memories: []memory.Memory{
				{ID: "GHI789", Category: "pattern", Importance: 0.5, Content: "tagged memory", Tags: []string{"go", "test"}},
			},
			wantIn: []string{"`GHI789`", "tags:", "go", "test", "tagged memory"},
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

	srv := New(store, logger, "test")
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
	srv := New(store, logger, "test")

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

func (m *mockEmbedder) EmbedQuery(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// --- Resource tests ---

func TestBuildProjectContext_WithMemories(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	// Seed a memory for the test project (ID "abc123").
	if _, _, _, err := store.Upsert(ctx, "abc123", "convention", "use nerdctl on node-2 for builds", "manual", 1.0, []string{"nerdctl"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
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
	srv := New(store, logger, "test")

	ctx := context.Background()
	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if text != "No memories found for this project." {
		t.Errorf("expected empty placeholder, got: %s", text)
	}
}

func TestBuildProjectContext_IncludesGlobal(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	// Seed a global memory — must ensure _global project exists first.
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	if _, _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl not docker", "manual", 1.0, []string{}); err != nil {
		t.Fatalf("Upsert global: %v", err)
	}

	// buildProjectContext for abc123 should pull in _global memories too.
	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if !strings.Contains(text, "nerdctl") {
		t.Errorf("expected global memory in project context, got: %s", text)
	}
}

func TestBuildProjectContext_IncludesLearnedContext(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if err := store.UpdateLearnedContext(ctx, "abc123", "This is the learned summary.", ""); err != nil {
		t.Fatalf("UpdateLearnedContext: %v", err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "seed memory", "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if !strings.Contains(text, "## Learned Context") {
		t.Errorf("expected '## Learned Context' section, got: %s", text)
	}
	if !strings.Contains(text, "learned summary") {
		t.Errorf("expected learned context text, got: %s", text)
	}
}

// TestBuildProjectContext_IncludesDecisionID: ghost_project_context must show
// a decision's own decisions.id — the id ghost_decisions_list/supersedes
// expect — not the unrelated memories.id of its companion memory row.
func TestBuildProjectContext_IncludesDecisionID(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	decisionID, _, _, err := store.RecordDecision(ctx, "abc123", "Use SQLite", "Embedded DB for simplicity", "No CGO dependency", []string{"PostgreSQL", "MySQL"}, []string{"database"})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if !strings.Contains(text, "## Recent Decisions") {
		t.Errorf("expected '## Recent Decisions' section, got: %s", text)
	}
	if !strings.Contains(text, decisionID) {
		t.Errorf("expected decision's own id %q in output, got: %s", decisionID, text)
	}
}

func TestNew_RegistersResources(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()

	// Verify ghost://project/{project_id}/context is registered by reading it.
	// A real resource read would go through the MCP transport; here we exercise
	// the underlying helper directly to confirm the handler logic is wired up.
	text, err := srv.buildProjectContext(ctx, "abc123")
	if err != nil {
		t.Errorf("project context resource handler returned error: %v", err)
	}
	if text == "" {
		t.Error("project context resource returned empty text")
	}

	// Verify ghost://memories/global is registered by reading global memories.
	// _global project may not exist yet — GetTopMemories returns empty, not an error.
	globals, err := store.GetTopMemories(ctx, "_global", 50)
	if err != nil {
		t.Errorf("global resource backing store query failed: %v", err)
	}
	// Empty is valid — just confirms the store call succeeds.
	_ = globals
}

// --- Store-backed tool logic tests ---
// These test the core logic paths that MCP tool handlers exercise,
// using real in-memory SQLite to verify end-to-end behavior.

func TestSaveAndSearch_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Save a memory via store (simulating ghost_memory_save logic).
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

	// Search via FTS (simulating ghost_memory_search without embedder).
	results, err := store.SearchHybrid(ctx, "abc123", "context Background", nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected at least one search result")
	}
}

func TestSaveAndSearch_WithEmbedder(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ch := make(chan string, 1)
	srv.SetEmbedder(&mockEmbedder{}, ch)

	ctx := context.Background()

	// Save memory.
	_, _, _, err := store.Upsert(ctx, "abc123", "fact", "Ghost uses SQLite with FTS5", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Embed + search (mock embedder returns [0.1, 0.2, 0.3]).
	vec, err := srv.embedder.EmbedQuery(ctx, "SQLite FTS5")
	if err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	results, err := store.SearchHybrid(ctx, "abc123", "SQLite", vec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	// FTS should still find it even with dummy vector.
	if len(results) == 0 {
		t.Error("expected at least one search result")
	}
}

func TestListMemories_ByCategoryAndAll(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Save memories in different categories with distinct content to avoid merge.
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "Go compiles to static binaries with no runtime dependencies", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "decision", "Chi was chosen as HTTP router for its stdlib compatibility", "mcp", 0.7, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "Cardano uses Ouroboros Praos consensus protocol for block production", "mcp", 0.6, []string{}); err != nil {
		t.Fatal(err)
	}

	// List by category.
	facts, err := store.GetByCategory(ctx, "abc123", "fact", 30)
	if err != nil {
		t.Fatalf("GetByCategory: %v", err)
	}
	if len(facts) != 2 {
		t.Errorf("expected 2 facts, got %d", len(facts))
	}

	// List all.
	all, err := store.GetAll(ctx, "abc123", 30)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 memories, got %d", len(all))
	}
}

func TestDeleteMemory_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, _, _, err := store.Upsert(ctx, "abc123", "gotcha", "watch for nil pointers", "mcp", 0.5, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Verify it exists.
	all, _ := store.GetAll(ctx, "abc123", 100)
	found := false
	for _, m := range all {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("memory not found after upsert")
	}

	// Delete it.
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify gone.
	all, _ = store.GetAll(ctx, "abc123", 100)
	for _, m := range all {
		if m.ID == id {
			t.Error("memory should be deleted")
		}
	}
}

func TestGhostMemoryPin_EndToEnd(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	id, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "decision", Content: "pin me", Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_pin",
		Arguments: map[string]any{"project_id": "test-project", "memory_id": id, "pinned": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_pin: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}

	mems, err := store.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if !mems[0].Pinned {
		t.Error("expected memory to be pinned")
	}
}

func TestGhostMemoryPin_RejectsWrongProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	if err := store.EnsureProject(ctx, "other", "/tmp/other-pin", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "decision", Content: "do not pin from another project", Source: "mcp", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_pin",
		Arguments: map[string]any{"project_id": "other", "memory_id": id, "pinned": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_memory_pin: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected ownership rejection, got: %+v", result.Content)
	}

	mems, err := store.GetByIDs(ctx, []string{id})
	if err != nil || len(mems) != 1 {
		t.Fatalf("GetByIDs: %v (n=%d)", err, len(mems))
	}
	if mems[0].Pinned {
		t.Error("pin state must be unchanged after rejected cross-project pin")
	}
}

func TestSearchAll_CrossProject(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create second project.
	if err := store.EnsureProject(ctx, "def456", "/tmp/other", "other-project"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "ghost uses SQLite", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "def456", "fact", "roller uses SQLite", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}

	// SearchFTSAll should find both.
	results, err := store.SearchFTSAll(ctx, "SQLite", 10)
	if err != nil {
		t.Fatalf("SearchFTSAll: %v", err)
	}
	if len(results) < 2 {
		t.Errorf("expected at least 2 cross-project results, got %d", len(results))
	}
}

func TestSaveGlobal_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Ensure _global project.
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}

	id, _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert global: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}

	// Verify retrievable.
	mems, err := store.GetTopMemories(ctx, "_global", 50)
	if err != nil {
		t.Fatalf("GetTopMemories: %v", err)
	}
	if len(mems) == 0 {
		t.Error("expected at least one global memory")
	}
}

func TestTaskLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create task.
	id, err := store.CreateTask(ctx, "abc123", "Fix the bug", "Segfault in main.go", 1)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if id == "" {
		t.Error("expected task ID")
	}

	// List tasks.
	tasks, err := store.ListTasks(ctx, "abc123", "", 30)
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Title != "Fix the bug" {
		t.Errorf("title = %q", tasks[0].Title)
	}

	// Complete task.
	if err := store.CompleteTask(ctx, id, "Fixed in commit abc"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	// List done tasks.
	done, err := store.ListTasks(ctx, "abc123", "done", 30)
	if err != nil {
		t.Fatalf("ListTasks done: %v", err)
	}
	if len(done) != 1 {
		t.Errorf("expected 1 done task, got %d", len(done))
	}
}

func TestDecisionRecord_EndToEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, memID, _, err := store.RecordDecision(ctx, "abc123", "Use SQLite", "Embedded DB for simplicity", "No CGO dependency", []string{"PostgreSQL", "MySQL"}, []string{"database"})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Error("expected decision ID")
	}
	if memID == "" || memID == id {
		t.Errorf("expected distinct non-empty memory ID, got %q (decision ID: %q)", memID, id)
	}

	decisions, err := store.ListDecisions(ctx, "abc123", "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].Title != "Use SQLite" {
		t.Errorf("title = %q", decisions[0].Title)
	}
}

func TestHealthOutput(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Seed memories with very distinct content to avoid Upsert merge.
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "Ghost uses SQLite with FTS5 for full-text search capabilities", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "convention", "Kubernetes manifests use helmfile for declarative deployment management", "mcp", 0.6, []string{}); err != nil {
		t.Fatal(err)
	}

	// Count memories.
	count, err := store.CountMemories(ctx, "abc123")
	if err != nil {
		t.Fatalf("CountMemories: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 memories, got %d", count)
	}

	// List projects.
	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}
}

func TestFormatMemories_EdgeCases(t *testing.T) {
	// Multiple memories with different features.
	mems := []memory.Memory{
		{Category: "fact", Importance: 0.5, Content: "plain memory"},
		{Category: "decision", Importance: 1.0, Content: "critical decision", Pinned: true, Tags: []string{"arch"}},
		{Category: "gotcha", Importance: 0.3, Content: "minor gotcha"},
	}
	result := formatMemories(mems)

	if !strings.Contains(result, "[fact]") || !strings.Contains(result, "[decision]") || !strings.Contains(result, "[gotcha]") {
		t.Errorf("expected all categories in output: %s", result)
	}
	if !strings.Contains(result, "[pinned]") {
		t.Error("expected [pinned] marker")
	}
	if !strings.Contains(result, `tags:["arch"]`) {
		t.Errorf("expected tags in output: %s", result)
	}
	// Each memory on its own line.
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
}

// TestFormatMemoriesAtJudgesValidityAtTheRequestedInstant fixes the rule a
// historical listing shares with ghost_memory_search (#899): validity is judged AT
// the requested instant. A row valid at T and closed since is shown as valid at
// T; a row whose window had closed, or not yet opened, at T is withheld; a bound
// exactly at T is inside the window; an open-ended or unreadable-bound row is
// kept with no verdict.
func TestFormatMemoriesAtJudgesValidityAtTheRequestedInstant(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	stamp := func(d time.Duration) *string { return strPtr(at.Add(d).Format(f)) }
	verified := stamp(-time.Hour)

	mk := func(id, content string, from, until, ver *string) memory.Memory {
		return memory.Memory{ID: id, Category: "fact", Importance: 0.7, Content: content, ValidFrom: from, ValidUntil: until, VerifiedAt: ver}
	}
	mems := []memory.Memory{
		mk("mem1", "valid at T, closed since", stamp(-48*time.Hour), stamp(24*time.Hour), verified),
		mk("mem2", "opens after T", stamp(24*time.Hour), stamp(72*time.Hour), verified),
		mk("mem3", "closed before T", stamp(-72*time.Hour), stamp(-time.Hour), verified),
		mk("mem4", "open ended", nil, nil, nil),
		mk("mem5", "ends exactly at T", stamp(-48*time.Hour), stamp(0), verified),
		mk("mem6", "starts exactly at T", stamp(0), stamp(48*time.Hour), verified),
		mk("mem7", "unreadable bound", nil, strPtr("not a date"), nil),
		mk("mem8", "unverified window", stamp(-time.Hour), nil, nil),
	}
	result := formatMemoriesAt(mems, at)

	if !strings.Contains(result, "Validity judged at "+at.Format(time.RFC3339)) {
		t.Errorf("the listing does not say validity was judged at T:\n%s", result)
	}
	for _, id := range []string{"`mem2`", "`mem3`"} {
		if memoryMetaGroup(result, id) != "" {
			t.Errorf("%s is outside its window at T but is listed:\n%s", id, result)
		}
	}
	for _, id := range []string{"`mem1`", "`mem5`", "`mem6`"} {
		got := memoryMetaGroup(result, id)
		if got == "" {
			t.Errorf("%s is inside its window at T but is missing:\n%s", id, result)
		}
		if strings.Contains(got, "expired") || strings.Contains(got, "not yet valid") || strings.Contains(got, "unverified") {
			t.Errorf("%s is valid at T but is labelled %q", id, got)
		}
	}
	if got := memoryMetaGroup(result, "`mem1`"); !strings.Contains(got, "until") {
		t.Errorf("the window of a row valid at T is not shown: %q", got)
	}
	for _, id := range []string{"`mem4`", "`mem7`"} {
		got := memoryMetaGroup(result, id)
		if !strings.Contains(result, id) {
			t.Errorf("%s states no readable window but is missing", id)
		}
		for _, bad := range []string{"valid from", "until", "expired", "not yet valid", "unverified"} {
			if strings.Contains(got, bad) {
				t.Errorf("%s carries a validity claim %q: %q", id, bad, got)
			}
		}
	}
	if got := memoryMetaGroup(result, "`mem8`"); !strings.Contains(got, "unverified") {
		t.Errorf("an unverified window lost its marker: %q", got)
	}
}

// TestWithholdInvalidAtMatchesTheListingFormatter: the handler withholds before
// the limit and the formatter withholds again defensively; both go through
// memory.ValidityAt, so they must not disagree on which rows survive.
func TestWithholdInvalidAtMatchesTheListingFormatter(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	open := strPtr(at.Add(24 * time.Hour).Format(f))
	closed := strPtr(at.Add(-24 * time.Hour).Format(f))
	rows := []memory.AsOfRow{
		{Memory: memory.Memory{ID: "keep", Category: "fact", Content: "kept", ValidUntil: open}},
		{Memory: memory.Memory{ID: "drop", Category: "fact", Content: "dropped", ValidUntil: closed}},
		{Memory: memory.Memory{ID: "future", Category: "fact", Content: "later", ValidFrom: open}},
	}
	kept := withholdInvalidAt(rows, at)
	if len(kept) != 1 || kept[0].ID != "keep" {
		t.Fatalf("withholdInvalidAt kept %v, want only keep", kept)
	}
	mems := make([]memory.Memory, 0, len(rows))
	for _, r := range rows {
		mems = append(mems, r.Memory)
	}
	out := formatMemoriesAt(mems, at)
	if !strings.Contains(out, "kept") || strings.Contains(out, "dropped") || strings.Contains(out, "later") {
		t.Errorf("the formatter disagrees with withholdInvalidAt:\n%s", out)
	}
}

// TestProjectContextAsOfAgreesWithSearchAtTheSameInstant: the two as_of surfaces
// judge the same rows the same way (#899). Rows are saved through the tools with
// windows placed around T, then both ghost_project_context and ghost_memory_search
// are asked at T: each row listed by one is listed by the other, and the rows
// outside their window at T are listed by neither.
func TestProjectContextAsOfAgreesWithSearchAtTheSameInstant(t *testing.T) {
	_, session := newCapSession(t)
	const at = "2035-01-01T00:00:00Z"
	rows := []struct {
		content    string
		from, till string
		inWindow   bool
	}{
		{"zebra valid across T", "2034-06-01T00:00:00Z", "2036-01-01T00:00:00Z", true},
		{"zebra closed before T", "", "2034-06-01T00:00:00Z", false},
		{"zebra opens after T", "2036-01-01T00:00:00Z", "", false},
		{"zebra ends exactly at T", "", at, true},
		{"zebra starts exactly at T", at, "2036-06-01T00:00:00Z", true},
		{"zebra open ended", "", "", true},
	}
	for _, r := range rows {
		args := map[string]any{"project_id": "test-project", "content": r.content, "category": "fact"}
		if r.from != "" {
			args["valid_from"] = r.from
		}
		if r.till != "" {
			args["valid_until"] = r.till
		}
		if res := callTool(t, session, "ghost_memory_save", args); res.IsError {
			t.Fatalf("save %q: %s", r.content, resultText(res))
		}
	}

	listing := resultText(callTool(t, session, "ghost_project_context", map[string]any{
		"project_id": "test-project", "as_of": at,
	}))
	search := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "zebra", "as_of": at,
	}))
	if !strings.Contains(listing, "Validity judged at "+at) {
		t.Errorf("the listing does not say validity was judged at T:\n%s", listing)
	}
	for _, r := range rows {
		inListing := strings.Contains(listing, r.content)
		inSearch := strings.Contains(search, r.content)
		if inListing != inSearch {
			t.Errorf("%q: listing=%v search=%v, the two as_of surfaces disagree\nlisting:\n%s\nsearch:\n%s", r.content, inListing, inSearch, listing, search)
		}
		if inListing != r.inWindow {
			t.Errorf("%q: listed=%v, want %v (inside its window at T)", r.content, inListing, r.inWindow)
		}
	}
}

// memoryMetaGroup returns the parenthesized metadata group on the result line
// whose id marker appears in marker — the text from the last '(' before the «
// data delimiter to the first ')' after it, which is where formatMemoriesInternal
// puts importance, the scope label and the validity window.
func memoryMetaGroup(result, marker string) string {
	for _, line := range strings.Split(result, "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		if idx := strings.LastIndex(line, "("); idx >= 0 {
			if end := strings.Index(line[idx:], ")"); end >= 0 {
				return line[idx : idx+end+1]
			}
		}
		return ""
	}
	return ""
}

// TestFormatMemoriesAsOf_EmptyListingCarriesNoDisclosure is the empty half of
// the same property: projectContextSection writes a heading only for a non-empty
// body, so a disclosure appended to no rows would render a heading over nothing
// — an empty `## Global (applies to all projects)` reads as a claim about the
// project's cross-project rows. An empty listing is therefore empty, not a
// disclosure.
func TestFormatMemoriesAsOf_EmptyListingCarriesNoDisclosure(t *testing.T) {
	at := time.Date(2026, 10, 5, 22, 20, 24, 0, time.UTC)
	if got := formatMemoriesAt(nil, at); got != "" {
		t.Errorf("formatMemoriesAt with no rows = %q, want empty: a disclosure here is a heading over no rows", got)
	}
	// And through the section writer, which is where the heading is decided.
	var sb strings.Builder
	projectContextSection(&sb, globalSectionHeading, formatMemoriesAt(nil, at))
	if strings.Contains(sb.String(), globalSectionHeading) {
		t.Errorf("an empty historical half rendered %q, want no heading:\n%s", globalSectionHeading, sb.String())
	}
}

// TestFormatMemoriesAtLabelsAnUnverifiedWindowAtT: verified_at is a flag rather
// than a predicate, so a window that is inside T but was never verified is
// listed and marked unverified, while one outside T is withheld whether or not it
// was verified.
func TestFormatMemoriesAtLabelsAnUnverifiedWindowAtT(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	insideUnverified := memory.Memory{
		ID: "meminside", Category: "fact", Importance: 0.7, Content: "inside, never verified",
		ValidFrom: strPtr(at.Add(-24 * time.Hour).Format(f)),
	}
	closedUnverified := memory.Memory{
		ID: "memclosed", Category: "fact", Importance: 0.7, Content: "closed before T, never verified",
		ValidFrom:  strPtr(at.Add(-72 * time.Hour).Format(f)),
		ValidUntil: strPtr(at.Add(-24 * time.Hour).Format(f)),
	}
	out := formatMemoriesAt([]memory.Memory{insideUnverified, closedUnverified}, at)
	if got := memoryMetaGroup(out, "`meminside`"); !strings.Contains(got, "unverified") {
		t.Errorf("an unverified window inside T lost its marker: %q", got)
	}
	if strings.Contains(out, "memclosed") {
		t.Errorf("a window closed before T is listed:\n%s", out)
	}
}

// TestFormatMemoriesAsOf_CurrentReadStillUsesWallClock ensures that without asOf,
// formatMemories still uses the wall clock (backward compatibility).
func TestFormatMemoriesAsOf_CurrentReadStillUsesWallClock(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour)

	// Use the stamp layout that the validity parser accepts (SQLite datetime format)
	stampLayout := memory.StoredStampLayout

	// Memory expired now (valid_until = past)
	expiredNow := memory.Memory{
		ID:         "mem1",
		Category:   "fact",
		Importance: 0.7,
		Content:    "expired now",
		ValidFrom:  strPtr(past.Add(-48 * time.Hour).Format(stampLayout)),
		ValidUntil: strPtr(past.Format(stampLayout)),
	}

	result := formatMemories([]memory.Memory{expiredNow})

	// Without asOf, should use wall clock and show "expired"
	if !strings.Contains(result, "expired") {
		t.Errorf("formatMemories without asOf should use wall clock and show 'expired': %s", result)
	}
}

func TestTaskUpdate_EmptyStatusPreservesCurrentStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.CreateTask(ctx, "abc123", "Refactor auth", "needs cleanup", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Default status is "pending". Update priority only (no status change) —
	// UpdateTask preserves status/description internally when passed nil.
	current, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if current.Status != "pending" {
		t.Fatalf("expected initial status=pending, got %q", current.Status)
	}

	priority := 1
	if _, err := store.UpdateTask(ctx, id, nil, &priority, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	after, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if after.Status != "pending" {
		t.Errorf("status should remain pending, got %q", after.Status)
	}
	if after.Priority != 1 {
		t.Errorf("priority should be updated to 1, got %d", after.Priority)
	}
}

func TestGhostTaskUpdate_RejectsInvalidStatus(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	id, err := store.CreateTask(ctx, "abc123", "Fix the bug", "needs triage", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_task_update",
		Arguments: map[string]any{"task_id": id, "status": "pendding"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_task_update: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result for invalid status, got: %+v", result.Content)
	}

	var msg string
	if len(result.Content) > 0 {
		if tc, ok := result.Content[0].(*mcp.TextContent); ok {
			msg = tc.Text
		}
	}
	if strings.Contains(msg, "CHECK constraint") {
		t.Errorf("expected a clear validation error, but got the raw SQLite constraint error: %s", msg)
	}
	wantSubstr := `invalid status "pendding" — must be one of: pending, active, blocked, done`
	if !strings.Contains(msg, wantSubstr) {
		t.Errorf("expected error to contain %q, got: %s", wantSubstr, msg)
	}

	// The task's status must be untouched by the rejected update.
	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "pending" {
		t.Errorf("status should remain pending after rejected update, got %q", task.Status)
	}
}

func TestParseProjectIDFromURI(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		want    string
		wantErr bool
	}{
		{
			name: "plain name",
			uri:  "ghost://project/ghost/context",
			want: "ghost",
		},
		{
			name: "URL-encoded space",
			uri:  "ghost://project/my%20project/context",
			want: "my project",
		},
		{
			name: "decisions resource",
			uri:  "ghost://project/infra/decisions",
			want: "infra",
		},
		{
			name:    "missing project_id",
			uri:     "ghost://project//context",
			wantErr: true,
		},
		{
			name:    "invalid URI",
			uri:     "://bad",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProjectIDFromURI(tc.uri)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateTags(t *testing.T) {
	tests := []struct {
		name       string
		tags       []string
		wantLen    int
		wantLast   string
		wantErr    bool
		wantErrHas string
	}{
		{"nil", nil, 0, "", false, ""},
		{"empty", []string{}, 0, "", false, ""},
		{"under limit", []string{"a", "b", "c"}, 3, "c", false, ""},
		{"at limit", make([]string, 10), 10, "", false, ""},
		{"over limit", make([]string, 15), 10, "", false, ""},
		{"long tag", []string{strings.Repeat("x", 100)}, 1, strings.Repeat("x", 64), false, ""},
		// A SPACE and any length are accepted, and that is the decision the
		// character class is drawn around: a tag is a keyword a reader scans
		// inside a JSON array, not a key or a selector. See validateTags.
		{"a tag holding a space", []string{"ci timeouts"}, 1, "ci timeouts", false, ""},
		{"a non-ascii tag", []string{"日本語"}, 1, "日本語", false, ""},
		// The class. Each is a character that can end the rendered line the label
		// sits on; the boundary cases that are NOT in the class are in the table
		// above, and the two directions matter more than the four refusals.
		{"an opening guillemet", []string{"a«b"}, 0, "", true, "tag 0"},
		{"a closing guillemet", []string{"a»b"}, 0, "", true, "tag 0"},
		{"a newline", []string{"a\nb"}, 0, "", true, "tag 0"},
		{"a carriage return", []string{"a\rb"}, 0, "", true, "tag 0"},
		{"a nul", []string{"a\x00b"}, 0, "", true, "tag 0"},
		{"a tab", []string{"a\tb"}, 0, "", true, "tag 0"},
		{"a backtick", []string{"a`b"}, 0, "", true, "tag 0"},
		// The POSITION, not just the field: a ten-tag list has to name the one to
		// change, or the caller has to diff two lists by eye.
		{"one hostile tag beside ordinary ones", []string{"golden", "x«y"}, 0, "", true, "tag 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateTags(tt.tags)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr {
				// A refusal returns no list, so a caller cannot mistake a partial
				// result for a validated one — and nothing is written.
				if got != nil {
					t.Errorf("a refusal returned a tag list anyway: %v", got)
				}
				if !strings.Contains(err.Error(), tt.wantErrHas) {
					t.Errorf("the refusal does not name the field and position (%q): %v", tt.wantErrHas, err)
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLast != "" && len(got) > 0 && got[len(got)-1] != tt.wantLast {
				t.Errorf("last = %q, want %q", got[len(got)-1], tt.wantLast)
			}
		})
	}
}

func TestDefaultImportance(t *testing.T) {
	f := func(v float32) *float32 { return &v }

	tests := []struct {
		name     string
		p        *float32
		fallback float32
		want     float32
	}{
		{"nil defaults", nil, 0.7, 0.7},
		{"explicit zero", f(0), 0.7, 0},
		{"normal value", f(0.5), 0.7, 0.5},
		{"clamp high", f(2.0), 0.7, 1.0},
		{"clamp negative", f(-1), 0.7, 0},
		{"max value", f(1.0), 0.7, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultImportance(tt.p, tt.fallback)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func testServer(t *testing.T) *Server {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Server{store: store, logger: logger}
}

func TestApplyMemoryUpdate(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "original", Source: "mcp", Importance: 0.7, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("updates content and reports changed fields", func(t *testing.T) {
		msg, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Content: "corrected",
		})
		if err != nil {
			t.Fatalf("applyMemoryUpdate: %v", err)
		}
		if !strings.Contains(msg, "content") {
			t.Errorf("message should name changed field, got %q", msg)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Content != "corrected" {
			t.Errorf("content = %q, want corrected", mems[0].Content)
		}
	})

	t.Run("rejects wrong project", func(t *testing.T) {
		if err := srv.store.EnsureProject(ctx, "other", "/tmp/other", "other"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "other", MemoryID: id, Content: "hijack",
		})
		if err == nil {
			t.Error("expected ownership rejection")
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Content == "hijack" {
			t.Error("content must be unchanged after rejected cross-project update")
		}
	})

	t.Run("rejects unknown memory", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: "nope", Content: "x",
		})
		if err == nil {
			t.Error("expected not-found error")
		}
	})

	t.Run("rejects invalid category", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Category: "bogus",
		})
		if err == nil {
			t.Error("expected invalid-category error")
		}
	})

	t.Run("rejects empty update", func(t *testing.T) {
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id,
		})
		if err == nil {
			t.Error("expected nothing-to-update error")
		}
	})

	t.Run("clamps importance", func(t *testing.T) {
		imp := float32(4.2)
		_, err := srv.applyMemoryUpdate(ctx, nil, updateArgs{
			ProjectID: "test-project", MemoryID: id, Importance: &imp,
		})
		if err != nil {
			t.Fatalf("applyMemoryUpdate: %v", err)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].Importance > 1.0 {
			t.Errorf("importance should clamp to 1.0, got %f", mems[0].Importance)
		}
	})
}

func TestPromoteMemory(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "preference", Content: "tabs never spaces", Source: "mcp", Importance: 0.8, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("rejects wrong project", func(t *testing.T) {
		if err := srv.store.EnsureProject(ctx, "other", "/tmp/other2", "other"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if _, err := srv.promoteMemory(ctx, "other", id); err == nil {
			t.Error("expected ownership rejection")
		}
	})

	t.Run("promotes and then rejects re-promotion", func(t *testing.T) {
		msg, err := srv.promoteMemory(ctx, "test-project", id)
		if err != nil {
			t.Fatalf("promoteMemory: %v", err)
		}
		if !strings.Contains(msg, "global") {
			t.Errorf("message should mention global, got %q", msg)
		}
		mems, _ := srv.store.GetByIDs(ctx, []string{id})
		if mems[0].ProjectID != "_global" {
			t.Errorf("project = %q, want _global", mems[0].ProjectID)
		}
		if _, err := srv.promoteMemory(ctx, "test-project", id); err == nil {
			t.Error("expected already-global rejection")
		}
	})

	t.Run("rejects unknown memory", func(t *testing.T) {
		if _, err := srv.promoteMemory(ctx, "test-project", "nope"); err == nil {
			t.Error("expected not-found error")
		}
	})

	t.Run("rejects missing args", func(t *testing.T) {
		if _, err := srv.promoteMemory(ctx, "", id); err == nil {
			t.Error("expected missing project_id error")
		}
		if _, err := srv.promoteMemory(ctx, "test-project", ""); err == nil {
			t.Error("expected missing memory_id error")
		}
	})
}

// connectedClient spins up a Server and a Client wired together over an
// in-memory transport, and returns the connected ClientSession. Callers must
// close the returned session's connection via t.Cleanup handling in Connect.
// The client's name is deliberately unknown to ai.SourceForClientName, so
// tests that need a known harness identity must use connectedClientNamed:
// ghost_resolve no longer falls back to claude for unknown clients.
func connectedClient(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	return connectedClientNamed(t, srv, "test-client")
}

// connectedClientNamed connects a client that self-reports name, which drives
// ghost_resolve's session-scoped backend selection.
func connectedClientNamed(t *testing.T, srv *Server, name string) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestPrompts_RecallProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "seed memory for recall test", "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	session := connectedClient(t, srv)

	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "recall_project",
		Arguments: map[string]string{"project_id": "test-project"},
	})
	if err != nil {
		t.Fatalf("GetPrompt recall_project: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(res.Messages))
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "seed memory for recall test") {
		t.Errorf("expected recalled memory in prompt text, got: %s", text.Text)
	}

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "recall_project"}); err == nil {
		t.Error("expected error for missing project_id argument")
	}
}

func TestPrompts_RecordDecision(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	res, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "record_decision",
		Arguments: map[string]string{"project_id": "test-project", "topic": "auth strategy"},
	})
	if err != nil {
		t.Fatalf("GetPrompt record_decision: %v", err)
	}
	text, ok := res.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Messages[0].Content)
	}
	if !strings.Contains(text.Text, "auth strategy") || !strings.Contains(text.Text, "ghost_decision_record") {
		t.Errorf("expected topic and tool reference in prompt text, got: %s", text.Text)
	}

	if _, err := session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      "record_decision",
		Arguments: map[string]string{"project_id": "test-project"},
	}); err == nil {
		t.Error("expected error for missing topic argument")
	}
}

// The waits a subscription test makes, bounded far above what an in-memory
// transport needs — the delivery is a channel hand-off, microseconds of work —
// and far below the package timeout. A test that fails because the behaviour is
// broken must not be able to look like a test that failed because the machine
// was busy (#805), and a bound is only ever paid by a test that is genuinely
// broken.
const (
	// subscriptionWait bounds the wait for a subscription to REGISTER.
	subscriptionWait = 30 * time.Second
	// notificationWait bounds the wait for a notification a test's own trigger
	// emitted exactly once, which cannot be retried without changing what the
	// test is about.
	notificationWait = 30 * time.Second
	// subscriptionProbeInterval paces the re-probe while a subscription is
	// still registering.
	subscriptionProbeInterval = 5 * time.Millisecond
	// subscriptionQuiet is how long the channel must stay empty before the
	// probes are treated as all delivered. It is a delivery-latency bound on
	// an in-memory transport, not a timeout on the behaviour under test.
	subscriptionQuiet = 100 * time.Millisecond
)

// awaitResourceSubscriptions blocks until the SERVER has registered a
// subscription for every uri, and leaves nothing of its own probing in updated.
//
// It exists because ClientSession.Subscribe is fire-and-forget under go-sdk's
// SEP-2575 protocol: it opens the background "subscriptions/listen" stream and
// returns as soon as that call is on the wire, while the server handles the
// stream on a goroutine of its own and then parks it for the life of the
// subscription. Nothing sent after Subscribe is therefore ordered behind the
// registration, and a tool call sent immediately after it can be handled
// FIRST — the notification that tool emits then reaches zero subscribers and
// is never delivered at all. That is a lost notification, not a late one, so
// no longer wait repairs it: the same test with a 30s deadline still failed at
// roughly the same rate as with 2s (#805), while this probe is a signal that a
// subscription is genuinely live.
func awaitResourceSubscriptions(t *testing.T, srv *Server, ctx context.Context, updated <-chan string, uris ...string) {
	t.Helper()
	// A notification for ANY of the requested URIs is progress: a probe that
	// arrives late still proves the subscription it names is registered, and
	// the re-probe loop can leave more than one of them in flight.
	remaining := make(map[string]bool, len(uris))
	for _, uri := range uris {
		remaining[uri] = true
	}
	deadline := time.After(subscriptionWait)
	for len(remaining) > 0 {
		for uri := range remaining {
			srv.notifyResourceUpdated(ctx, uri)
		}
		select {
		case got := <-updated:
			delete(remaining, got)
		case <-time.After(subscriptionProbeInterval):
		case <-deadline:
			t.Fatalf("subscriptions never registered within %s, still missing: %v", subscriptionWait, remaining)
		}
	}
	// Every URI is registered, but probes sent before its registration landed
	// can still be on the wire, and a test that asserts on what arrives NEXT
	// would read one of those as its own trigger's output.
	drainQuiescent(t, updated)
}

// drainQuiescent empties updated and keeps it empty for quiet. A plain
// non-blocking drain is not enough: a notification already sent is delivered
// asynchronously by the client session, so it can land after the drain has
// seen an empty channel. A test whose next assertion is "nothing arrives" would
// then read the probe as the thing it is looking for.
func drainQuiescent(t *testing.T, updated <-chan string) {
	t.Helper()
	deadline := time.After(subscriptionWait)
	for {
		select {
		case <-updated:
			// A probe still arriving; keep waiting for the quiet it implies.
		case <-time.After(subscriptionQuiet):
			return
		case <-deadline:
			t.Fatalf("probe notifications kept arriving for %s after every subscription registered", subscriptionWait)
		}
	}
}

// awaitNotification asserts the next notification is for want. The wait is on
// the event itself — a channel the client's handler writes — so it ends when the
// notification arrives and only expires if it never does.
func awaitNotification(t *testing.T, updated <-chan string, want string) {
	t.Helper()
	select {
	case got := <-updated:
		if got != want {
			t.Errorf("notified URI = %q, want %q", got, want)
		}
	case <-time.After(notificationWait):
		t.Fatalf("no resources/updated notification for %q within %s", want, notificationWait)
	}
}

func TestResourceSubscription_NotifiesOnMemorySave(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	const uri = "ghost://project/abc123/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "triggers subscription notify", "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A real client would only ever hit the registration race by chance right
	// at subscribe time, and it cannot retry a notification it never received —
	// so wait for the subscription to be live before the notify under test,
	// rather than re-sending the notify on a short timer.
	awaitResourceSubscriptions(t, srv, ctx, updated, uri)

	srv.notifyResourceUpdated(ctx, uri)
	awaitNotification(t, updated, uri)

	if err := session.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
}

func TestResourceSubscription_NotifiesNameSubscriberOnToolSave(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()

	// Create a project whose hash ID differs from its name, exactly as the
	// session-start hook does (path-hashed ID + human-readable name).
	if err := store.EnsureProject(ctx, "6bdc098af7f5", "/home/wayne/git/myproj", "myproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// Clients read (and thus subscribe to) resources by project name.
	const nameURI = "ghost://project/myproj/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: nameURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The save below emits its notifications exactly once, so a subscription
	// that had not registered yet when it ran would lose them for good (#805).
	awaitResourceSubscriptions(t, srv, ctx, updated, nameURI)

	// Drive the real tool handler, which resolves name -> hash internally.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "myproj", "content": "name subscriber should be notified", "category": "fact"},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}

	awaitNotification(t, updated, nameURI)
}

func TestResourceSubscription_NotifiesHashSubscriberOnToolSave(t *testing.T) {
	// Guards the previously-working direction: emitting both aliases must not
	// disturb delivery to a client subscribed by the resolved hash ID.
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if err := store.EnsureProject(ctx, "6bdc098af7f5", "/home/wayne/git/myproj", "myproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 4)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	const hashURI = "ghost://project/6bdc098af7f5/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: hashURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Same one-shot trigger as the name-URI sibling, and the same race.
	awaitResourceSubscriptions(t, srv, ctx, updated, hashURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "myproj", "content": "hash subscriber should still be notified", "category": "fact"},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}

	awaitNotification(t, updated, hashURI)
}

// TestResourceSubscription_RejectsUnknownURI exercises handleSubscribe
// directly rather than through session.Subscribe. Under go-sdk 1.7.0's
// SEP-2575 protocol, ClientSession.Subscribe dispatches "subscriptions/listen"
// fire-and-forget (see callSubscriptionsListen in the SDK's transport.go) and
// always returns nil once the call is sent, regardless of whether the
// server's SubscribeHandler later rejects the URI — so the rejection is no
// longer observable through the client round trip. handleSubscribe is
// Ghost's own validation logic; test it directly.
func TestResourceSubscription_RejectsUnknownURI(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	req := &mcp.SubscribeRequest{Params: &mcp.SubscribeParams{URI: "http://not-ghost/resource"}}
	if err := srv.handleSubscribe(ctx, req); err == nil {
		t.Error("expected error subscribing to non-ghost:// URI")
	}
}

// resolveAnswer is the canonical resolve RESOLVED reply under the #640 verdict
// contract: a RESOLVED must name what closed the note, or the parser reads it
// as a KEEP. Fakes that mean "this harness says resolved" must use it.
const resolveAnswer = "RESOLVED | closed-by: the tracking issue closed"

func writeFakeClaude(t *testing.T, path, answer string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
printf '%s' '` + answer + `'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude binary: %v", err)
	}
}

// TestGhostResolve_DryRunByDefault covers the plain dry-run path: a fake
// `claude` CLI on PATH classifies (unknown client name falls back to the best
// CLI on PATH), the seeded memory is confirmed as resolved evidence, and the
// dry run must not write resolved_at.
func TestGhostResolve_DryRunByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Fake `claude` binary on PATH, answering RESOLVED so the classifier
	// confirms the seeded memory (see internal/ai/cli_client_test.go for the
	// same pattern used within the ai package). The client reports itself as
	// claude-code so session routing selects the fake claude: unknown clients
	// no longer fall back to claude-first PATH ordering.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	writeFakeClaude(t, bin, resolveAnswer)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	session := connectedClientNamed(t, srv, "claude-code")

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "would resolve") {
		t.Errorf("expected dry-run output to contain %q, got %q", "would resolve", text.Text)
	}

	// Dry-run must not write: the seeded memory must still be an eligible
	// resolve candidate (resolved_at IS NULL).
	cands, err := store.ResolveCandidates(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	found := false
	for _, m := range cands {
		if m.Content == content {
			found = true
		}
	}
	if !found {
		t.Error("expected seeded memory to remain an eligible resolve candidate after dry-run (resolved_at must still be NULL)")
	}
}

// TestGhostResolve_ReportsUnknownVerdict verifies that an unparseable fake
// harness reply is surfaced as UNKNOWN rather than silently looking like a
// successful KEEP-only pass.
func TestGhostResolve_ReportsUnknownVerdict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), "MAYBE")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	session := connectedClientNamed(t, srv, "claude-code")

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "1 UNKNOWN") {
		t.Errorf("expected UNKNOWN count in resolve summary, got %q", text.Text)
	}
}

// TestGhostResolve_UsesSessionHarness covers the session-scoped backend
// selection: an MCP client that reports itself as `opencode` must classify via
// the opencode binary even though `claude` sorts first in the PATH fallback
// order. Both fake binaries are on PATH and answer differently — claude says
// KEEP, opencode says RESOLVED — so a confirmed result proves opencode (the
// session's own harness) was the one consulted.
func TestGhostResolve_UsesSessionHarness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), "KEEP")
	// opencode answers with its JSON-lines format (see
	// internal/ai/opencode_client_test.go's fakeOpenCodeBinary).
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"text\",\"part\":{\"type\":\"text\",\"text\":\"RESOLVED | closed-by: the tracking issue closed\"}}'\n"), 0o755); err != nil {
		t.Fatalf("write fake opencode binary: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "opencode", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "1 confirmed evidence") {
		t.Errorf("expected opencode (RESOLVED) to confirm the memory, got %q", text.Text)
	}
}

// TestGhostResolve_UsesConfiguredBinaryAndModelPin covers the cli.*_binary +
// cli.model_resolve path: a fake opencode binary that only answers RESOLVED
// when invoked with `-m opencode/big-pickle` must be used even though a decoy
// opencode (answering KEEP) sits on PATH, and the configured pin must be passed
// constructor-level. The MCP server is long-lived, so the whole test would fail
// (decoy used, badly) if the pin were threaded via process env instead.
func TestGhostResolve_UsesConfiguredBinaryAndModelPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binaries require a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	// The configured opencode binary lives outside PATH; the decoy on PATH
	// answers KEEP, so a PATH-resolved harness would not confirm anything.
	dir := t.TempDir()
	openBin := filepath.Join(dir, "opencode")
	script := `#!/bin/sh
case " $* " in
  *" -m opencode/big-pickle "*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"RESOLVED | closed-by: the tracking issue closed"}}';;
  *) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';;
esac
`
	if err := os.WriteFile(openBin, []byte(script), 0o755); err != nil {
		t.Fatalf("write configured opencode binary: %v", err)
	}
	decoyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(decoyDir, "opencode"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"text\",\"part\":{\"type\":\"text\",\"text\":\"KEEP\"}}'\n"), 0o755); err != nil {
		t.Fatalf("write decoy opencode binary: %v", err)
	}
	t.Setenv("PATH", decoyDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv.SetResolveCLI(config.CLIConfig{
		OpenCodeBinary: openBin,
		ModelResolve:   "opencode/big-pickle",
	})

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	session := connectedClientNamed(t, srv, "opencode")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "1 confirmed evidence") {
		t.Errorf("expected configured opencode with the model pin to confirm, got %q", text.Text)
	}
}

// TestGhostResolve_AppliesWithCLI covers the write path: a client reporting
// claude-code with a fake `claude` CLI on PATH classifies RESOLVED and
// apply:true must stamp resolved_at — the CLI is a full-trust primary now that
// MCP sampling is retired (see
// docs/superpowers/specs/2026-08-24-resolve-sampling-path-design.md), so the
// seeded memory must no longer be an eligible resolve candidate afterwards.
func TestGhostResolve_AppliesWithCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	writeFakeClaude(t, bin, resolveAnswer)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	session := connectedClientNamed(t, srv, "claude-code")

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "resolved 1") {
		t.Errorf("expected applied write output, got %q", text.Text)
	}

	cands, err := store.ResolveCandidates(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	for _, m := range cands {
		if m.Content == content {
			t.Error("expected seeded memory to be stamped resolved_at after apply:true — it must no longer be a candidate")
		}
	}
}

// TestGhostResolve_RequiresCLIBinary covers the no-CLI failure mode: a client
// whose harness is known (claude-code) but has no binary anywhere on PATH must
// get a clean error naming the harness instead of silently degrading.
func TestGhostResolve_RequiresCLIBinary(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Empty PATH: exec.LookPath fails for every CLI backend.
	t.Setenv("PATH", t.TempDir())

	session := connectedClientNamed(t, srv, "claude-code")

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when no CLI binary is available, got %+v", result.Content)
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if !strings.Contains(sb.String(), `calling harness "claude-code" is unavailable`) {
		t.Errorf("expected error to name the unavailable harness, got %q", sb.String())
	}
}

// TestGhostResolve_UnknownClientUndetectedErrors covers the silent-claude
// contract: an unknown MCP client with no detectable harness ancestor must
// error, even though a `claude` binary is sitting on PATH. The detection seam
// pins the undetected case because the test process's own ancestor chain can
// legitimately contain a harness (running `go test` from an opencode session).
func TestGhostResolve_UnknownClientUndetectedErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), resolveAnswer)
	t.Setenv("PATH", dir)

	old := detectCallingSource
	detectCallingSource = func() string { return "" }
	t.Cleanup(func() { detectCallingSource = old })

	session := connectedClient(t, srv)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result for an undetectable caller, got %+v", result.Content)
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if !strings.Contains(sb.String(), "cannot determine the calling harness") {
		t.Errorf("expected undetectable-harness error, got %q", sb.String())
	}
}

// TestGhostResolve_DetectedHarnessMissingDoesNotFallBackToClaude is the
// reported-bug regression: an unknown client in an opencode session (OPENCODE
// env marker) with only `claude` installed must error naming opencode. The old
// claude-first fallback would have run claude here and returned success.
func TestGhostResolve_DetectedHarnessMissingDoesNotFallBackToClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	const content = "root cause: fixed in v2, no further action needed"
	if _, _, _, err := store.Upsert(ctx, "abc123", "gotcha", content, "manual", 0.5, []string{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	dir := t.TempDir()
	writeFakeClaude(t, filepath.Join(dir, "claude"), resolveAnswer)
	// Only the temp dir is on PATH: the fake claude resolves, opencode cannot.
	t.Setenv("PATH", dir)
	t.Setenv("OPENCODE", "1")

	session := connectedClient(t, srv)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result when the detected harness (opencode) is unavailable, got %+v", result.Content)
	}
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	if !strings.Contains(sb.String(), `"opencode"`) {
		t.Errorf("expected error to name the detected harness, got %q", sb.String())
	}
}

// TestDecisionRecordSupersedesArg covers both shapes of the optional
// supersedes argument through the real tool handler. The common case — no
// supersedes — must not emit a warning: that argument is optional, so a stray
// UPDATE against an empty id would put "WARNING: could not mark  as
// superseded" on every ordinary ghost_decision_record result.
func TestDecisionRecordSupersedesArg(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	record := func(args map[string]any) string {
		t.Helper()
		args["project_id"] = "test-project"
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "ghost_decision_record", Arguments: args,
		})
		if err != nil {
			t.Fatalf("CallTool ghost_decision_record: %v", err)
		}
		if result.IsError {
			t.Fatalf("ghost_decision_record returned an error result: %+v", result.Content)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("expected TextContent, got %T", result.Content[0])
		}
		return text.Text
	}

	plain := record(map[string]any{
		"title": "Use Redis for the job queue", "decision": "Redis lists as the backend",
		"rationale": "already deployed",
	})
	if strings.Contains(plain, "WARNING") {
		t.Errorf("a decision recorded without supersedes must not warn, got %q", plain)
	}
	if strings.Contains(plain, "superseded") {
		t.Errorf("a decision recorded without supersedes must not mention supersession, got %q", plain)
	}

	decisions, err := store.ListDecisions(ctx, "abc123", "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	oldID := decisions[0].ID

	reversal := record(map[string]any{
		"title": "Reverse: use Postgres", "decision": "SKIP LOCKED on Postgres",
		"rationale": "Redis lost jobs on failover", "supersedes": oldID,
	})
	if strings.Contains(reversal, "WARNING") {
		t.Fatalf("supersession of a real decision should succeed, got %q", reversal)
	}
	if !strings.Contains(reversal, oldID) {
		t.Errorf("result should name the superseded decision %s, got %q", oldID, reversal)
	}

	after, err := store.ListDecisions(ctx, "abc123", "active", 10)
	if err != nil {
		t.Fatalf("ListDecisions active: %v", err)
	}
	if len(after) != 1 || after[0].ID == oldID {
		t.Errorf("expected only the replacement to remain active, got %+v", after)
	}
}

func TestSaveGlobal_NotifiesEmbeddingWorker(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ch := make(chan string, 4)
	srv.SetEmbedder(&mockEmbedder{}, ch)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "global memory should trigger embedding", "category": "fact"},
	}); err != nil {
		t.Fatalf("CallTool ghost_save_global: %v", err)
	}

	select {
	case got := <-ch:
		if got != "_global" {
			t.Errorf("projectCh notified with %q, want %q", got, "_global")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for embedding worker notification on ghost_save_global")
	}
}

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		in       string
		maxBytes int
		want     string
	}{
		{"hello", 10, "hello"},
		{"hello", 4, "hell"},
		{"héllo", 2, "h"}, // é is 2 bytes starting at index 1 — must not split
		{"日本語", 4, "日"},   // each rune is 3 bytes
		{"日本語", 6, "日本"},
		{"", 5, ""},
	}
	for _, tc := range tests {
		got := truncateUTF8(tc.in, tc.maxBytes)
		if got != tc.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.maxBytes, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) produced invalid UTF-8: %q", tc.in, tc.maxBytes, got)
		}
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"shorter than 8", "abc", "abc"},
		{"exactly 8", "12345678", "12345678"},
		{"longer than 8", "123456789abcdef", "12345678"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortID(tc.id); got != tc.want {
				t.Errorf("shortID(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

// shortTaskListStore wraps a real store but overrides ListTasks to return a
// fixed, caller-supplied task instead of querying SQLite. CreateTask always
// mints a 32-char hex ID (hex(randomblob(16)), see internal/memory/schema.go),
// so a task ID shorter than 8 characters can never occur through the public
// Store API — this wrapper is the only way to get one in front of the
// ghost_task_list tool and the project-tasks resource template, to prove
// their output formatting doesn't panic on one.
type shortTaskListStore struct {
	provider.MemoryStore
	task memory.Task
}

func (s shortTaskListStore) ListTasks(ctx context.Context, projectID, status string, limit int) ([]memory.Task, error) {
	if status != "" && status != s.task.Status {
		return nil, nil
	}
	return []memory.Task{s.task}, nil
}

// TestGhostTaskList_ShortTaskID proves the ghost_task_list tool formats a
// task ID via shortID rather than an unguarded t.ID[:8] slice. Before the
// fix, t.ID[:8] on this 3-character ID panics with "slice bounds out of
// range"; shortID returns it unchanged.
func TestGhostTaskList_ShortTaskID(t *testing.T) {
	store := testStore(t)
	wrapped := shortTaskListStore{
		MemoryStore: store,
		task: memory.Task{
			ID:        "abc",
			ProjectID: "abc123",
			Title:     "short id task",
			Status:    "pending",
			Priority:  1,
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(wrapped, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_task_list",
		Arguments: map[string]any{"project_id": "abc123"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_task_list: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_task_list returned an error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "`abc`") {
		t.Errorf("expected output to contain the short task id `abc`, got: %s", text.Text)
	}
}

// TestProjectTasksResource_ShortTaskID is the resource-template counterpart
// of TestGhostTaskList_ShortTaskID: it exercises the second t.ID[:8] call
// site, in the ghost://project/{project_id}/tasks resource template.
func TestProjectTasksResource_ShortTaskID(t *testing.T) {
	store := testStore(t)
	wrapped := shortTaskListStore{
		MemoryStore: store,
		task: memory.Task{
			ID:        "abc",
			ProjectID: "abc123",
			Title:     "short id task",
			Status:    "pending",
			Priority:  1,
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(wrapped, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ghost://project/abc123/tasks"})
	if err != nil {
		t.Fatalf("ReadResource tasks: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected resource contents")
	}
	if !strings.Contains(result.Contents[0].Text, "`abc`") {
		t.Errorf("expected output to contain the short task id `abc`, got: %s", result.Contents[0].Text)
	}
}

func TestGhostProjectDelete_DryRunByDefault(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	var memIDs []string
	for i := 0; i < 4; i++ {
		id, err := store.Create(ctx, "abc123", memory.Memory{
			Category: "fact", Content: "seed memory for delete test", Source: "manual", Importance: 0.5, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		memIDs = append(memIDs, id)
	}
	// Seed memory_links, tasks, decisions too, not just
	// memories, with mutually distinct counts (memories=5 [4 seeded here + 1
	// from RecordDecision's decision_log row], memory_links=3, tasks=2,
	// decisions=1) so a swap of any pair of summary fields in
	// the tool's output formatting would be caught here the same way the
	// store-layer mutation test catches a transposition.
	for _, pair := range [][2]string{{memIDs[0], memIDs[1]}, {memIDs[0], memIDs[2]}, {memIDs[1], memIDs[2]}} {
		if err := store.CreateLink(ctx, pair[0], pair[1], "related", 0.8, "auto"); err != nil {
			t.Fatalf("CreateLink %v: %v", pair, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := store.CreateTask(ctx, "abc123", "Fix the bug", "needs triage", 1); err != nil {
			t.Fatalf("CreateTask %d: %v", i, err)
		}
	}
	if _, _, _, err := store.RecordDecision(ctx, "abc123", "seed decision", "did the thing", "because", nil, nil); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "Would delete") {
		t.Errorf("expected dry-run framing %q in response, got %q", "Would delete", text.Text)
	}
	if !strings.Contains(text.Text, "memories:     5") {
		t.Errorf("expected summary line %q in response, got %q", "memories:     5", text.Text)
	}
	if !strings.Contains(text.Text, "memory_links: 3") {
		t.Errorf("expected summary line %q in response, got %q", "memory_links: 3", text.Text)
	}
	if !strings.Contains(text.Text, "tasks:        2") {
		t.Errorf("expected summary line %q in response, got %q", "tasks:        2", text.Text)
	}
	if !strings.Contains(text.Text, "decisions:    1") {
		t.Errorf("expected summary line %q in response, got %q", "decisions:    1", text.Text)
	}
	if !strings.Contains(text.Text, "token_usage:  0") {
		t.Errorf("expected summary line %q in response, got %q", "token_usage:  4", text.Text)
	}
	if !strings.Contains(text.Text, "audit_log:    0") {
		t.Errorf("expected summary line %q in response, got %q", "audit_log:    0", text.Text)
	}
	// The two audit-trail lines, which were the gap in this list: a Contains set
	// that stops at audit_log leaves a renderer free to drop a field the same
	// struct's other renderer prints, and a missing line reads as a zero.
	if !strings.Contains(text.Text, "retrievals:   0") {
		t.Errorf("expected summary line %q in response, got %q", "retrievals:   0", text.Text)
	}
	if !strings.Contains(text.Text, "audits:       0") {
		t.Errorf("expected summary line %q in response, got %q", "audits:       0", text.Text)
	}

	all, err := store.GetAll(ctx, "abc123", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("expected memories to survive dry-run, got %d memories", len(all))
	}
}

func TestGhostProjectDelete_ApplyRemovesProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "will this survive apply", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "Deleted") {
		t.Errorf("expected apply framing %q in response, got %q", "Deleted", text.Text)
	}
	if !strings.Contains(text.Text, "memories:     1") {
		t.Errorf("expected summary line %q in response, got %q", "memories:     1", text.Text)
	}

	id, _, err := store.ResolveProject(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" {
		t.Error("expected project to be gone after apply")
	}
}

func TestGhostProjectDelete_RejectsGlobal(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "_global", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result deleting _global, got success")
	}
}

func TestGhostProjectDelete_NotifiesSubscribersOnApply(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "subscriber should hear about this deletion", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	const contextURI = "ghost://project/test-project/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: contextURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	const tasksURI = "ghost://project/test-project/tasks"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: tasksURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	const decisionsURI = "ghost://project/test-project/decisions"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: decisionsURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The deletion notifications are emitted exactly once and can't be
	// retried, so confirm each subscription actually registered before
	// triggering the real delete.
	awaitResourceSubscriptions(t, srv, ctx, updated, contextURI, tasksURI, decisionsURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project", "apply": true},
	}); err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}

	want := map[string]bool{contextURI: false, tasksURI: false, decisionsURI: false}
	deadline := time.After(notificationWait)
	for remaining := len(want); remaining > 0; {
		select {
		case got := <-updated:
			if seen, ok := want[got]; !ok {
				t.Errorf("notified unexpected URI %q", got)
			} else if seen {
				// Duplicate notification for the same URI; ignore.
			} else {
				want[got] = true
				remaining--
			}
		case <-deadline:
			t.Fatalf("timed out waiting for notifications, still missing: %+v", want)
		}
	}
}

func TestGhostProjectDelete_DryRunDoesNotNotify(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "dry run must not notify anyone", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	const contextURI = "ghost://project/test-project/context"
	if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: contextURI}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// A dry run that notifies nobody only proves something if this subscriber
	// was actually registered: without the probe, a subscription that lost the
	// registration race would silence the tool too, and the assertion below
	// would pass for the wrong reason.
	awaitResourceSubscriptions(t, srv, ctx, updated, contextURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project"},
	}); err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}

	select {
	case got := <-updated:
		t.Fatalf("expected no notification on dry-run, got %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}
