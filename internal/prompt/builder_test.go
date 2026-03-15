package prompt

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/mode"
	"github.com/wcatz/ghost/internal/project"
)

// mockMemoryStore implements memoryQuerier for testing
type mockMemoryStore struct {
	memories      []memory.Memory
	learnedCtx    string
	shouldError   bool
}

func (m *mockMemoryStore) GetTopMemories(ctx context.Context, projectID string, limit int) ([]memory.Memory, error) {
	if m.shouldError {
		return nil, context.DeadlineExceeded
	}
	if len(m.memories) > limit {
		return m.memories[:limit], nil
	}
	return m.memories, nil
}

func (m *mockMemoryStore) GetLearnedContext(ctx context.Context, projectID string) (string, error) {
	if m.shouldError {
		return "", context.DeadlineExceeded
	}
	return m.learnedCtx, nil
}

func TestNewBuilder(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	if builder == nil {
		t.Fatal("expected non-nil builder")
	}
	if builder.store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestBuildSystemBlocks_ThreeBlocks(t *testing.T) {
	store := &mockMemoryStore{
		memories: []memory.Memory{
			{Category: "pattern", Content: "Use TDD", Importance: 0.8},
		},
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{
		ID:       "test123",
		Name:     "testproject",
		Path:     "/test/path",
		Language: "Go",
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	// Should have 3 blocks: static personality, project context, memories
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	// Block 1: Static personality (cached)
	if blocks[0].CacheControl == nil {
		t.Error("expected block 1 to be cached")
	}
	if !strings.Contains(blocks[0].Text, "You are Ghost") {
		t.Error("expected block 1 to contain personality")
	}

	// Block 2: Project context (cached)
	if blocks[1].CacheControl == nil {
		t.Error("expected block 2 to be cached")
	}
	if !strings.Contains(blocks[1].Text, "testproject") {
		t.Error("expected block 2 to contain project name")
	}
	if !strings.Contains(blocks[1].Text, "Mode: code") {
		t.Error("expected block 2 to contain mode")
	}

	// Block 3: Memories (not cached)
	if blocks[2].CacheControl != nil {
		t.Error("expected block 3 to NOT be cached")
	}
	if !strings.Contains(blocks[2].Text, "Ghost memories") {
		t.Error("expected block 3 to contain memories")
	}
}

func TestBuildSystemBlocks_StaticPersonality(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["chat"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 1 {
		t.Fatal("expected at least 1 block")
	}

	personality := blocks[0].Text
	expectedPhrases := []string{
		"You are Ghost",
		"memory-first coding agent",
		"CAPABILITIES",
		"RULES",
		"RESPONSE STYLE",
	}

	for _, phrase := range expectedPhrases {
		if !strings.Contains(personality, phrase) {
			t.Errorf("expected personality to contain %q", phrase)
		}
	}
}

func TestBuildSystemBlocks_ProjectContext(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	projCtx := &project.Context{
		ID:          "abc123",
		Name:        "myproject",
		Path:        "/home/user/myproject",
		Language:    "Go",
		GitBranch:   "feat/new-feature",
		GitStatus:   "3 files modified",
		LastCommits: []string{"abc123 Add feature", "def456 Fix bug"},
		TestCommand: "go test ./...",
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 2 {
		t.Fatal("expected at least 2 blocks")
	}

	block2 := blocks[1].Text

	expectedFields := map[string]string{
		"Project: myproject":      "project name",
		"Path: /home/user/myproject": "project path",
		"Language: Go":            "language",
		"Git branch: feat/new-feature": "git branch",
		"Git status: 3 files modified": "git status",
		"Last commit: abc123 Add feature": "last commit",
		"Test command: go test ./...": "test command",
		"Mode: code":                  "mode name",
	}

	for field, desc := range expectedFields {
		if !strings.Contains(block2, field) {
			t.Errorf("expected block 2 to contain %s: %q", desc, field)
		}
	}
}

func TestBuildSystemBlocks_ClaudeMD(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	claudeMD := "# Project Instructions\nUse tabs not spaces"

	projCtx := &project.Context{
		ID:       "test",
		Name:     "test",
		Path:     "/test",
		Language: "Go",
		ClaudeMD: claudeMD,
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	block2 := blocks[1].Text

	if !strings.Contains(block2, "Project instructions (CLAUDE.md)") {
		t.Error("expected CLAUDE.md header in block 2")
	}
	if !strings.Contains(block2, claudeMD) {
		t.Error("expected CLAUDE.md content in block 2")
	}
}

func TestBuildSystemBlocks_ClaudeMD_Truncation(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	// Create content longer than 2000 chars
	longContent := strings.Repeat("instruction ", 300)

	projCtx := &project.Context{
		ID:       "test",
		Name:     "test",
		Path:     "/test",
		Language: "Go",
		ClaudeMD: longContent,
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	block2 := blocks[1].Text

	if !strings.Contains(block2, "(truncated)") {
		t.Error("expected truncation marker for long CLAUDE.md")
	}
}

func TestBuildSystemBlocks_Memories(t *testing.T) {
	store := &mockMemoryStore{
		memories: []memory.Memory{
			{Category: "pattern", Content: "Use TDD", Importance: 0.9},
			{Category: "convention", Content: "Kebab-case filenames", Importance: 0.7, Pinned: true},
			{Category: "gotcha", Content: "Avoid global state", Importance: 0.8},
		},
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 3 {
		t.Fatal("expected 3 blocks")
	}

	block3 := blocks[2].Text

	if !strings.Contains(block3, "Ghost memories") {
		t.Error("expected memories header")
	}
	if !strings.Contains(block3, "Use TDD") {
		t.Error("expected memory content")
	}
	if !strings.Contains(block3, "[pattern]") {
		t.Error("expected category tag")
	}
	if !strings.Contains(block3, "(imp: 0.9)") {
		t.Error("expected importance score")
	}
	if !strings.Contains(block3, "[pinned]") {
		t.Error("expected pinned marker")
	}
}

func TestBuildSystemBlocks_LearnedContext(t *testing.T) {
	store := &mockMemoryStore{
		learnedCtx: "Developer prefers functional style. Uses strict linting.",
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 3 {
		t.Fatal("expected 3 blocks")
	}

	block3 := blocks[2].Text

	if !strings.Contains(block3, "Learned context") {
		t.Error("expected learned context header")
	}
	if !strings.Contains(block3, "functional style") {
		t.Error("expected learned context content")
	}
}

func TestBuildSystemBlocks_RecentGitActivity(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	projCtx := &project.Context{
		ID:       "test",
		Name:     "test",
		Path:     "/test",
		Language: "Go",
		LastCommits: []string{
			"abc123 Add feature X",
			"def456 Fix bug Y",
			"ghi789 Refactor Z",
		},
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 3 {
		t.Fatal("expected 3 blocks")
	}

	block3 := blocks[2].Text

	if !strings.Contains(block3, "Recent git activity") {
		t.Error("expected git activity header")
	}
	if !strings.Contains(block3, "def456 Fix bug Y") {
		t.Error("expected commit in recent activity")
	}
	if !strings.Contains(block3, "ghi789 Refactor Z") {
		t.Error("expected commit in recent activity")
	}

	// First commit should be in block 2, not block 3
	block2 := blocks[1].Text
	if !strings.Contains(block2, "Last commit: abc123 Add feature X") {
		t.Error("expected first commit in block 2")
	}
}

func TestBuildSystemBlocks_NoMemories(t *testing.T) {
	store := &mockMemoryStore{
		memories: []memory.Memory{}, // Empty
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{
		ID:          "test",
		Name:        "test",
		Path:        "/test",
		Language:    "Go",
		LastCommits: []string{"abc123 commit"}, // Only 1 commit, so no "Recent git activity"
	}

	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	// Should only have 2 blocks if no memories and no multi-commit history
	if len(blocks) != 2 {
		t.Errorf("expected 2 blocks when no memories, got %d", len(blocks))
	}
}

func TestBuildSystemBlocks_ErrorFetchingMemories(t *testing.T) {
	store := &mockMemoryStore{
		shouldError: true,
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["code"]

	// Should not panic, should gracefully handle error
	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 2 {
		t.Fatal("expected at least 2 blocks even on error")
	}
}

func TestBuildSystemBlocks_DifferentModes(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}

	modes := []string{"chat", "code", "debug", "review", "plan", "refactor"}

	for _, modeName := range modes {
		t.Run(modeName, func(t *testing.T) {
			m := mode.Modes[modeName]
			blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

			if len(blocks) < 2 {
				t.Fatal("expected at least 2 blocks")
			}

			block2 := blocks[1].Text
			if !strings.Contains(block2, "Mode: "+modeName) {
				t.Errorf("expected mode name %q in block 2", modeName)
			}
			if !strings.Contains(block2, m.SystemHint) {
				t.Errorf("expected system hint for mode %q", modeName)
			}
		})
	}
}

func TestBuildSystemBlocks_MemoryLimit(t *testing.T) {
	// Create 30 memories
	mems := make([]memory.Memory, 30)
	for i := 0; i < 30; i++ {
		mems[i] = memory.Memory{
			Category:   "fact",
			Content:    "Memory " + string(rune('A'+i)),
			Importance: 0.5,
		}
	}

	store := &mockMemoryStore{
		memories: mems,
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 3 {
		t.Fatal("expected 3 blocks")
	}

	block3 := blocks[2].Text

	// Should only include top 20
	memoryCount := strings.Count(block3, "[fact]")
	if memoryCount > 20 {
		t.Errorf("expected max 20 memories, got %d", memoryCount)
	}
}

func TestBuildSystemBlocks_CachingStrategy(t *testing.T) {
	store := &mockMemoryStore{
		memories: []memory.Memory{
			{Category: "pattern", Content: "test", Importance: 0.8},
		},
	}
	builder := NewBuilder(store)

	projCtx := &project.Context{ID: "test", Name: "test", Path: "/test", Language: "Go"}
	m := mode.Modes["code"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(blocks))
	}

	// Block 1 and 2 should be cached
	if blocks[0].CacheControl == nil {
		t.Error("expected block 1 (personality) to be cached")
	}
	if blocks[1].CacheControl == nil {
		t.Error("expected block 2 (project context) to be cached")
	}

	// Block 3 should NOT be cached (dynamic memories)
	if blocks[2].CacheControl != nil {
		t.Error("expected block 3 (memories) to NOT be cached")
	}
}

func TestBuildSystemBlocks_EmptyProject(t *testing.T) {
	store := &mockMemoryStore{}
	builder := NewBuilder(store)

	projCtx := &project.Context{
		ID:       "empty",
		Name:     "empty",
		Path:     "/empty",
		Language: "unknown",
	}

	m := mode.Modes["chat"]

	blocks := builder.BuildSystemBlocks(context.Background(), projCtx, m)

	if len(blocks) < 2 {
		t.Fatal("expected at least 2 blocks")
	}

	// Should still work with minimal context
	block2 := blocks[1].Text
	if !strings.Contains(block2, "Project: empty") {
		t.Error("expected project name in minimal context")
	}
}
