package claudeimport

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestClaudeMemoryDir(t *testing.T) {
	t.Run("encoding", func(t *testing.T) {
		got := EncodeProjectPath("/home/wayne/git/ghost")
		want := "-home-wayne-git-ghost"
		if got != want {
			t.Errorf("EncodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("encoding_windows", func(t *testing.T) {
		got := EncodeProjectPath(`C:\Users\me\repo`)
		want := "C--Users-me-repo"
		if got != want {
			t.Errorf("EncodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("encoding_special_chars", func(t *testing.T) {
		if got := EncodeProjectPath("/srv/u/.config/app"); got != "-srv-u--config-app" {
			t.Errorf("dot in path: got %q, want %q", got, "-srv-u--config-app")
		}
		if got := EncodeProjectPath("/srv/u/git/my_proj"); got != "-srv-u-git-my-proj" {
			t.Errorf("underscore: got %q, want %q", got, "-srv-u-git-my-proj")
		}
		if got := EncodeProjectPath("/srv/u/git/foo.bar"); got != "-srv-u-git-foo-bar" {
			t.Errorf("dot: got %q, want %q", got, "-srv-u-git-foo-bar")
		}
		if got := EncodeProjectPath("/srv/u/My Docs/x"); got != "-srv-u-My-Docs-x" {
			t.Errorf("space: got %q, want %q", got, "-srv-u-My-Docs-x")
		}
	})

	t.Run("encoding_nonascii", func(t *testing.T) {
		if got := EncodeProjectPath("/srv/u/café/repo"); got != "-srv-u-caf--repo" {
			t.Errorf("non-ascii: got %q, want %q", got, "-srv-u-caf--repo")
		}
	})

	t.Run("encoding_nonbmp_is_two_hyphens", func(t *testing.T) {
		// Claude Code's [^a-zA-Z0-9] regex runs over UTF-16 code units, so a
		// non-BMP character (an emoji) is two units and gets two '-' — not the
		// one a rune-wise fold would produce.
		if got := EncodeProjectPath("/srv/u/😀/repo"); got != "-srv-u----repo" {
			t.Errorf("emoji: got %q, want %q", got, "-srv-u----repo")
		}
	})

	t.Run("returns_empty_for_missing_dir", func(t *testing.T) {
		got := ClaudeMemoryDir("/nonexistent/project/path")
		if got != "" {
			t.Errorf("expected empty for missing dir, got %q", got)
		}
	})
}

// TestEncodeProjectPathLongName pins the >200-char branch against Claude
// Code's own encoder: truncate to 200 UTF-16 code units, then append '-' and
// the base-36 absolute value of a Java-style String.hashCode of the FULL path.
// A mismatch here is silent — the encoded name points at a directory Claude
// Code never wrote.
func TestEncodeProjectPathLongName(t *testing.T) {
	t.Run("at_cap_is_unhashed", func(t *testing.T) {
		atCap := "/" + strings.Repeat("d", 199) // folds to exactly 200 units
		if got, want := EncodeProjectPath(atCap), "-"+strings.Repeat("d", 199); got != want {
			t.Errorf("EncodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("past_cap_truncates_and_hashes", func(t *testing.T) {
		// "-" + 200 b's folds to 201 units; claude keeps the first 200 and
		// appends the base-36 hash of "/bbb…b" (201 b's).
		overCap := "/" + strings.Repeat("b", 200)
		if got, want := EncodeProjectPath(overCap), "-"+strings.Repeat("b", 199)+"-86ms41"; got != want {
			t.Errorf("EncodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("negative_hash_takes_absolute_value", func(t *testing.T) {
		// "/ccc…c" (201 c's) hashes to a negative int32; Math.abs before
		// toString(36) is what makes this "id6lpo" rather than a "-" prefix.
		overCap := "/" + strings.Repeat("c", 201)
		if got, want := EncodeProjectPath(overCap), "-"+strings.Repeat("c", 199)+"-id6lpo"; got != want {
			t.Errorf("EncodeProjectPath = %q, want %q", got, want)
		}
	})

	t.Run("hash_covers_the_whole_path", func(t *testing.T) {
		// Two paths sharing the kept 200-unit prefix must not collide, which
		// only holds if the hash is over the full path rather than the kept
		// prefix.
		a := "/" + strings.Repeat("b", 200)
		b := a + "x"
		if EncodeProjectPath(a) == EncodeProjectPath(b) {
			t.Errorf("paths %q and %q shared a 200-unit prefix and encoded identically", a, b)
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
	imported, _, err := importFromDir(ctx, store, projectID, memDir, logger)
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
	//
	// Store.Upsert itself is non-destructive (a duplicate save creates a new
	// linked row rather than merging), so idempotency here relies on
	// importFromDir's own dedup guard: it skips any file whose exact content
	// already exists for the project, rather than calling Upsert at all.
	// This matters because ghost mcp init calls Import unconditionally on
	// every run and documents itself as safe to re-run.
	imported2, _, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported2 != 0 {
		t.Errorf("imported2 = %d, want 0 (dedup guard skips already-imported content)", imported2)
	}
	count2, err := store.CountMemories(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != count {
		t.Errorf("after re-import: count = %d, want %d (idempotent)", count2, count)
	}
}

func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// failTransientStore wraps a store and fails the first UpsertWithOptions call
// with a transient error, then delegates. It stands in for a busy store losing
// its BEGIN budget on one file during a first-contact import.
type failTransientStore struct {
	*memory.Store
	calls int
}

func (s *failTransientStore) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts memory.UpsertOptions) (string, string, float64, error) {
	s.calls++
	if s.calls == 1 {
		return "", "", 0, fmt.Errorf("simulated transient store failure")
	}
	return s.Store.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, opts)
}

// TestTransientFailureIsRetriedAndRerunIsNoopAfterDelete is the regression test
// for the durable per-file marker (#1235). A file that failed transiently leaves
// no marker, so a later call retries it. Once imported, later calls are no-ops —
// even after the user deletes the memory, because the delete tombstone keeps the
// content and the marker check matches it.
func TestTransientFailureIsRetriedAndRerunIsNoopAfterDelete(t *testing.T) {
	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "notes.md", `---
name: Notes
description: Test notes
type: project
---
Some project notes here.`)

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(ctx, "proj", filepath.Dir(memDir), "proj"); err != nil {
		t.Fatal(err)
	}

	// Run 1: the store fails the first upsert — a transient error.
	transient := &failTransientStore{Store: store}
	imported, skipped, err := importFromDir(ctx, transient, "proj", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 0 {
		t.Errorf("run 1: imported = %d, want 0 (transient failure)", imported)
	}
	if len(skipped) != 1 || skipped[0] != "notes.md" {
		t.Errorf("run 1: skipped = %v, want [notes.md]", skipped)
	}

	// Run 2: the store is healthy — the file is retried and imports.
	imported, skipped, err = importFromDir(ctx, store, "proj", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 1 {
		t.Errorf("run 2: imported = %d, want 1 (retried after transient failure)", imported)
	}
	if len(skipped) != 0 {
		t.Errorf("run 2: skipped = %v, want []", skipped)
	}

	// The imported row carries the file's marker: that is the durable record
	// the next run reads instead of the file's text.
	mems, err := store.GetAll(ctx, "proj", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 {
		t.Fatalf("after import: %d memories, want 1", len(mems))
	}
	if mems[0].SourceRef != importMarkerRef("notes.md") {
		t.Errorf("imported row SourceRef = %q, want %q", mems[0].SourceRef, importMarkerRef("notes.md"))
	}

	// Run 3: the file's marker is present — a true no-op.
	imported, _, err = importFromDir(ctx, store, "proj", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 0 {
		t.Errorf("run 3: imported = %d, want 0 (marker says already imported)", imported)
	}

	// The user deletes the imported memory. The delete tombstone keeps the content.
	if err := store.Delete(ctx, mems[0].ID); err != nil {
		t.Fatal(err)
	}

	// Run 4: the memory is gone, but the tombstone matches — still a no-op.
	imported, _, err = importFromDir(ctx, store, "proj", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 0 {
		t.Errorf("run 4: imported = %d, want 0 (tombstone matches deleted memory)", imported)
	}
	count, err := store.CountMemories(ctx, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("after delete + re-import: count = %d, want 0 (no re-insert)", count)
	}
}

