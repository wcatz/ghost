package claudeimport

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestClaudeMemoryDir(t *testing.T) {
	t.Run("encoding", func(t *testing.T) {
		got := encodeProjectPath("/home/wayne/git/ghost")
		want := "-home-wayne-git-ghost"
		if got != want {
			t.Errorf("encodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("returns_empty_for_missing_dir", func(t *testing.T) {
		got := ClaudeMemoryDir("/nonexistent/project/path")
		if got != "" {
			t.Errorf("expected empty for missing dir, got %q", got)
		}
	})
}

func TestParseFrontmatter(t *testing.T) {
	t.Run("with_frontmatter", func(t *testing.T) {
		raw := "---\nname: Work autonomously\ndescription: Keep pushing forward\ntype: feedback\n---\nStop asking for approval."
		name, desc, ftype, body := parseFrontmatter(raw)
		if name != "Work autonomously" {
			t.Errorf("name = %q", name)
		}
		if desc != "Keep pushing forward" {
			t.Errorf("desc = %q", desc)
		}
		if ftype != "feedback" {
			t.Errorf("type = %q", ftype)
		}
		if body != "\nStop asking for approval." {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("without_frontmatter", func(t *testing.T) {
		raw := "# Just plain markdown\n\nSome content here."
		name, desc, ftype, body := parseFrontmatter(raw)
		if name != "" || desc != "" || ftype != "" {
			t.Errorf("expected empty fields, got name=%q desc=%q type=%q", name, desc, ftype)
		}
		if body != raw {
			t.Errorf("body should equal raw input")
		}
	})

	t.Run("quoted_values", func(t *testing.T) {
		raw := "---\nname: \"Quoted name\"\ntype: 'single'\n---\nBody."
		name, _, ftype, _ := parseFrontmatter(raw)
		if name != "Quoted name" {
			t.Errorf("name = %q, want Quoted name", name)
		}
		if ftype != "single" {
			t.Errorf("type = %q, want single", ftype)
		}
	})
}

func TestSkipFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"MEMORY.md", true},
		{"memory.md", true},
		{"plan_unified.md", true},
		{"plan_something.md", true},
		{"research_01_sqlite.md", true},
		{"feedback_autonomous.md", false},
		{"project_architecture.md", false},
		{"reference_services.md", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skipFile(tc.name)
			if got != tc.want {
				t.Errorf("skipFile(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsStub(t *testing.T) {
	if !isStub("short") {
		t.Error("expected short content to be stub")
	}
	if !isStub("Migrated to Ghost MCP. Run ghost_project_context.") {
		t.Error("expected migration stub to be detected")
	}
	if isStub("This is a real memory with substantial content about the project architecture.") {
		t.Error("expected real content to not be stub")
	}
}

func TestMapCategory(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"feedback", "preference"},
		{"Feedback", "preference"},
		{"project", "architecture"},
		{"reference", "fact"},
		{"pattern", "pattern"},
		{"decision", "decision"},
		{"gotcha", "gotcha"},
		{"user", "preference"},
		{"unknown", "fact"},
		{"", "fact"},
	}
	for _, tc := range cases {
		got := mapCategory(tc.in)
		if got != tc.want {
			t.Errorf("mapCategory(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestImportanceForType(t *testing.T) {
	if got := importanceForType("feedback"); got != 0.9 {
		t.Errorf("feedback importance = %v, want 0.9", got)
	}
	if got := importanceForType("project"); got != 0.8 {
		t.Errorf("project importance = %v, want 0.8", got)
	}
	if got := importanceForType("unknown"); got != defaultImportance {
		t.Errorf("unknown importance = %v, want %v", got, defaultImportance)
	}
}

func TestParseMemoryFile(t *testing.T) {
	dir := t.TempDir()

	// Feedback file with frontmatter.
	writeTempFile(t, dir, "feedback_autonomous.md", `---
name: Work autonomously
description: Keep pushing forward without asking permission
type: feedback
---
Stop asking for approval. Pick the next task and do it.

**Why:** User finds it disruptive when pausing to ask.
**How to apply:** After completing a task, start the next one.`)

	// Plain markdown without frontmatter.
	writeTempFile(t, dir, "ghost-capabilities.md", `# Ghost Capabilities Reference

## 9 Modes
Ghost supports 9 operating modes for different tasks.`)

	// MEMORY.md should be skipped.
	writeTempFile(t, dir, "MEMORY.md", `# Memory Index
- [feedback_autonomous.md](feedback_autonomous.md)`)

	// Plan file should be skipped.
	writeTempFile(t, dir, "plan_unified.md", `# Unified Plan
Step 1: do things.`)

	// Research file should be skipped.
	writeTempFile(t, dir, "research_01_sqlite.md", `# SQLite Research
Lots of content here.`)

	// Stub file should be skipped.
	writeTempFile(t, dir, "code-review.md", `Migrated to Ghost MCP.`)

	t.Run("feedback_with_frontmatter", func(t *testing.T) {
		content, cat, imp, skip, err := ParseMemoryFile(filepath.Join(dir, "feedback_autonomous.md"))
		if err != nil {
			t.Fatal(err)
		}
		if skip {
			t.Fatal("should not skip")
		}
		if cat != "preference" {
			t.Errorf("category = %q, want preference", cat)
		}
		if imp != 0.9 {
			t.Errorf("importance = %v, want 0.9", imp)
		}
		if !strings.Contains(content, "Work autonomously") {
			t.Errorf("content missing name header: %q", content[:80])
		}
		if !strings.Contains(content, "Stop asking for approval") {
			t.Errorf("content missing body")
		}
	})

	t.Run("plain_markdown", func(t *testing.T) {
		content, cat, imp, skip, err := ParseMemoryFile(filepath.Join(dir, "ghost-capabilities.md"))
		if err != nil {
			t.Fatal(err)
		}
		if skip {
			t.Fatal("should not skip")
		}
		if cat != "fact" {
			t.Errorf("category = %q, want fact", cat)
		}
		if imp != defaultImportance {
			t.Errorf("importance = %v, want %v", imp, defaultImportance)
		}
		if !strings.Contains(content, "9 Modes") {
			t.Error("content missing body")
		}
	})

	t.Run("skip_memory_md", func(t *testing.T) {
		_, _, _, skip, err := ParseMemoryFile(filepath.Join(dir, "MEMORY.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !skip {
			t.Error("MEMORY.md should be skipped")
		}
	})

	t.Run("skip_plan", func(t *testing.T) {
		_, _, _, skip, err := ParseMemoryFile(filepath.Join(dir, "plan_unified.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !skip {
			t.Error("plan file should be skipped")
		}
	})

	t.Run("skip_research", func(t *testing.T) {
		_, _, _, skip, err := ParseMemoryFile(filepath.Join(dir, "research_01_sqlite.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !skip {
			t.Error("research file should be skipped")
		}
	})

	t.Run("skip_stub", func(t *testing.T) {
		_, _, _, skip, err := ParseMemoryFile(filepath.Join(dir, "code-review.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !skip {
			t.Error("stub file should be skipped")
		}
	})
}

func TestImport_EndToEnd(t *testing.T) {
	// Set up a fake Claude memory directory.
	claudeDir := t.TempDir()
	memDir := filepath.Join(claudeDir, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeTempFile(t, memDir, "feedback_autonomous.md", `---
name: Work autonomously
description: Keep pushing forward
type: feedback
---
Stop asking for approval. Pick the next task and do it.`)

	writeTempFile(t, memDir, "project_architecture.md", `---
name: Architecture overview
description: System design notes
type: project
---
The system uses a 3-layer architecture with memory, orchestrator, and AI layers.`)

	writeTempFile(t, memDir, "MEMORY.md", `# Index
- [feedback_autonomous.md](feedback_autonomous.md)`)

	writeTempFile(t, memDir, "plan_unified.md", `# Plan
Should be skipped.`)

	// Set up a real SQLite store.
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)

	ctx := context.Background()
	projectID := "test-project"
	if err := store.EnsureProject(ctx, projectID, "/tmp/test", "test"); err != nil {
		t.Fatal(err)
	}

	// Call importFromDir directly since ClaudeMemoryDir won't find our temp dir.
	imported, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatal(err)
	}

	if imported != 2 {
		t.Errorf("imported = %d, want 2 (feedback + project)", imported)
	}

	// Verify memories are in store.
	count, err := store.CountMemories(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Errorf("store count = %d, want >= 2", count)
	}

	// Test idempotency — importing again should not increase count.
	imported2, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	// Upsert may merge, so imported2 could be non-zero but count should stay same.
	count2, err := store.CountMemories(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count {
		t.Errorf("after re-import: count = %d, want %d (idempotent)", count2, count)
	}
	_ = imported2
}

func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
