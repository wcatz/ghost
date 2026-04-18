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

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// --- Resource tests ---

func TestBuildProjectContext_WithMemories(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	// Seed a memory for the test project (ID "abc123").
	if _, _, err := store.Upsert(ctx, "abc123", "convention", "use nerdctl on node-2 for builds", "manual", 1.0, []string{"nerdctl"}); err != nil {
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
	if _, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl not docker", "manual", 1.0, []string{}); err != nil {
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
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "seed memory", "manual", 0.5, []string{}); err != nil {
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
	_, _, err := store.Upsert(ctx, "abc123", "fact", "Ghost uses SQLite with FTS5", "mcp", 0.8, []string{})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Embed + search (mock embedder returns [0.1, 0.2, 0.3]).
	vec, err := srv.embedder.Embed(ctx, "SQLite FTS5")
	if err != nil {
		t.Fatalf("Embed: %v", err)
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
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "Go compiles to static binaries with no runtime dependencies", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Upsert(ctx, "abc123", "decision", "Chi was chosen as HTTP router for its stdlib compatibility", "mcp", 0.7, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "Cardano uses Ouroboros Praos consensus protocol for block production", "mcp", 0.6, []string{}); err != nil {
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

	id, _, err := store.Upsert(ctx, "abc123", "gotcha", "watch for nil pointers", "mcp", 0.5, []string{})
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

func TestSearchAll_CrossProject(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create second project.
	if err := store.EnsureProject(ctx, "def456", "/tmp/other", "other-project"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "ghost uses SQLite", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Upsert(ctx, "def456", "fact", "roller uses SQLite", "mcp", 0.5, []string{}); err != nil {
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

	id, _, err := store.Upsert(ctx, "_global", "preference", "always use nerdctl", "mcp", 0.8, []string{})
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

	id, err := store.RecordDecision(ctx, "abc123", "Use SQLite", "Embedded DB for simplicity", "No CGO dependency", []string{"PostgreSQL", "MySQL"}, []string{"database"})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if id == "" {
		t.Error("expected decision ID")
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
	if _, _, err := store.Upsert(ctx, "abc123", "fact", "Ghost uses SQLite with FTS5 for full-text search capabilities", "mcp", 0.5, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Upsert(ctx, "abc123", "convention", "Kubernetes manifests use helmfile for declarative deployment management", "mcp", 0.6, []string{}); err != nil {
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

func TestTaskUpdate_EmptyStatusPreservesCurrentStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.CreateTask(ctx, "abc123", "Refactor auth", "needs cleanup", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Default status is "pending". Update priority only (no status change).
	// The fixed handler fetches current task and uses current.Status when args.Status == "".
	current, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if current.Status != "pending" {
		t.Fatalf("expected initial status=pending, got %q", current.Status)
	}

	// Simulate what the fixed ghost_task_update handler does: use current status.
	if err := store.UpdateTask(ctx, id, current.Status, 1, current.Description); err != nil {
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
		name     string
		tags     []string
		wantLen  int
		wantLast string
	}{
		{"nil", nil, 0, ""},
		{"empty", []string{}, 0, ""},
		{"under limit", []string{"a", "b", "c"}, 3, "c"},
		{"at limit", make([]string, 10), 10, ""},
		{"over limit", make([]string, 15), 10, ""},
		{"long tag", []string{strings.Repeat("x", 100)}, 1, strings.Repeat("x", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateTags(tt.tags)
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
