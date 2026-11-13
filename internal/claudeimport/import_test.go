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
	cases := []struct{ name, in, want string }{
		{"unix", "/home/wayne/git/ghost", "-home-wayne-git-ghost"},
		{"windows", `C:\Users\me\repo`, "C--Users-me-repo"},
		{"dot_dir", "/srv/u/.config/app", "-srv-u--config-app"},
		{"underscore", "/srv/u/git/my_proj", "-srv-u-git-my-proj"},
		{"space", "/srv/u/My Docs/x", "-srv-u-My-Docs-x"},
		{"nonascii", "/srv/u/café/repo", "-srv-u-caf--repo"},
		// Claude Code's regex runs over UTF-16 code units: a non-BMP rune is two.
		{"nonbmp_two_hyphens", "/srv/u/😀/repo", "-srv-u----repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeProjectPath(tc.in); got != tc.want {
				t.Errorf("EncodeProjectPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := ClaudeMemoryDir("/nonexistent/project/path"); got != "" {
		t.Errorf("expected empty for missing dir, got %q", got)
	}
}

// TestEncodeProjectPathLongName pins the >200-char branch against Claude
// Code's encoder: truncate to 200 UTF-16 units, append '-' and the base-36
// absolute value of a Java-style hashCode of the FULL path.
func TestEncodeProjectPathLongName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"at_cap_is_unhashed", "/" + strings.Repeat("d", 199), "-" + strings.Repeat("d", 199)},
		{"past_cap_truncates_and_hashes", "/" + strings.Repeat("b", 200), "-" + strings.Repeat("b", 199) + "-86ms41"},
		// This hash is a negative int32; Math.abs is what avoids a "-" prefix.
		{"negative_hash_takes_absolute_value", "/" + strings.Repeat("c", 201), "-" + strings.Repeat("c", 199) + "-id6lpo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EncodeProjectPath(tc.in); got != tc.want {
				t.Errorf("EncodeProjectPath = %q, want %q", got, tc.want)
			}
		})
	}
	a := "/" + strings.Repeat("b", 200)
	if EncodeProjectPath(a) == EncodeProjectPath(a+"x") {
		t.Error("paths sharing the kept 200-unit prefix encoded identically: the hash must cover the full path")
	}
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name, raw, wantName, wantDesc, wantType, wantBody string
	}{
		{"with_frontmatter", "---\nname: Work autonomously\ndescription: Keep pushing forward\ntype: feedback\n---\nStop asking for approval.",
			"Work autonomously", "Keep pushing forward", "feedback", "\nStop asking for approval."},
		{"without_frontmatter", "# Just plain markdown\n\nSome content here.", "", "", "", "# Just plain markdown\n\nSome content here."},
		{"quoted_values", "---\nname: \"Quoted name\"\ntype: 'single'\n---\nBody.", "Quoted name", "", "single", "\nBody."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc, ftype, body := parseFrontmatter(tc.raw)
			if name != tc.wantName || desc != tc.wantDesc || ftype != tc.wantType || body != tc.wantBody {
				t.Errorf("got (%q, %q, %q, %q), want (%q, %q, %q, %q)", name, desc, ftype, body,
					tc.wantName, tc.wantDesc, tc.wantType, tc.wantBody)
			}
		})
	}
}

func TestSkipFile(t *testing.T) {
	for name, want := range map[string]bool{
		"MEMORY.md": true, "memory.md": true, "plan_unified.md": true, "research_01_sqlite.md": true,
		"feedback_autonomous.md": false, "project_architecture.md": false,
	} {
		if got := skipFile(name); got != want {
			t.Errorf("skipFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsStub(t *testing.T) {
	for in, want := range map[string]bool{
		"short": true,
		"Migrated to Ghost MCP. Run ghost_project_context.":                              true,
		"This is a real memory with substantial content about the project architecture.": false,
	} {
		if got := isStub(in); got != want {
			t.Errorf("isStub(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMapCategory(t *testing.T) {
	for in, want := range map[string]string{
		"feedback": "preference", "Feedback": "preference", "project": "architecture", "reference": "fact",
		"pattern": "pattern", "decision": "decision", "gotcha": "gotcha", "user": "preference", "unknown": "fact", "": "fact",
	} {
		if got := mapCategory(in); got != want {
			t.Errorf("mapCategory(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImportanceForType(t *testing.T) {
	for in, want := range map[string]float32{"feedback": 0.9, "project": 0.8, "unknown": defaultImportance} {
		if got := importanceForType(in); got != want {
			t.Errorf("importanceForType(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseMemoryFile(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "feedback_autonomous.md", "---\nname: Work autonomously\ndescription: Keep pushing forward without asking permission\ntype: feedback\n---\n"+
		"Stop asking for approval. Pick the next task and do it.\n\n**Why:** User finds it disruptive when pausing to ask.")
	writeTempFile(t, dir, "ghost-capabilities.md", "# Ghost Capabilities Reference\n\n## 9 Modes\nGhost supports 9 operating modes for different tasks.")
	writeTempFile(t, dir, "MEMORY.md", "# Memory Index\n- [feedback_autonomous.md](feedback_autonomous.md)")
	writeTempFile(t, dir, "plan_unified.md", "# Unified Plan\nStep 1: do things.")
	writeTempFile(t, dir, "code-review.md", "Migrated to Ghost MCP.")

	for _, tc := range []struct {
		file, wantCat, wantIn string
		wantImp               float32
	}{
		{"feedback_autonomous.md", "preference", "Work autonomously", 0.9},
		{"ghost-capabilities.md", "fact", "9 Modes", defaultImportance},
	} {
		content, cat, imp, skip, err := ParseMemoryFile(filepath.Join(dir, tc.file))
		if err != nil || skip {
			t.Fatalf("%s: skip=%v err=%v", tc.file, skip, err)
		}
		if cat != tc.wantCat || imp != tc.wantImp || !strings.Contains(content, tc.wantIn) {
			t.Errorf("%s: category=%q importance=%v content=%q", tc.file, cat, imp, content)
		}
	}
	for _, f := range []string{"MEMORY.md", "plan_unified.md", "code-review.md"} {
		if _, _, _, skip, err := ParseMemoryFile(filepath.Join(dir, f)); err != nil || !skip {
			t.Errorf("%s: skip=%v err=%v, want skipped", f, skip, err)
		}
	}
}

func TestImport_EndToEnd(t *testing.T) {
	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTempFile(t, memDir, "feedback_autonomous.md", "---\nname: Work autonomously\ndescription: Keep pushing forward\ntype: feedback\n---\nStop asking for approval. Pick the next task and do it.")
	writeTempFile(t, memDir, "project_architecture.md", "---\nname: Architecture overview\ndescription: System design notes\ntype: project\n---\nThe system uses a 3-layer architecture with memory, orchestrator, and AI layers.")
	writeTempFile(t, memDir, "MEMORY.md", "# Index\n- [feedback_autonomous.md](feedback_autonomous.md)")
	writeTempFile(t, memDir, "plan_unified.md", "# Plan\nShould be skipped.")

	store := storeFor(t, "test-project")
	ctx, logger := context.Background(), slog.Default()
	imported, _, _, _, err := importFromDir(ctx, store, "test-project", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 2 {
		t.Errorf("imported = %d, want 2 (feedback + project)", imported)
	}
	count, err := store.CountMemories(ctx, "test-project")
	if err != nil || count != 2 {
		t.Fatalf("count = %d (err %v), want 2", count, err)
	}

	// ghost mcp init calls Import on every run: a re-import must skip every
	// file whose exact content is already stored.
	imported2, _, _, _, err := importFromDir(ctx, store, "test-project", memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if count2, _ := store.CountMemories(ctx, "test-project"); imported2 != 0 || count2 != count {
		t.Errorf("re-import: imported=%d count=%d, want 0 and %d (idempotent)", imported2, count2, count)
	}
}

func writeTempFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A duplicate that falls below the pre-skip window must still be skipped by
// the exact-content check inside the write transaction.
func TestImport_PreskipBoundedFallsThroughToTransaction(t *testing.T) {
	orig := preSkipWindow
	preSkipWindow = 2
	t.Cleanup(func() { preSkipWindow = orig })

	memDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dup := "This exact memory content appears again and again to test bounded preskip behavior here."
	writeTempFile(t, memDir, "dup_below.md", "---\nname: Dup below window\ndescription: Duplicate below pre-skip window\ntype: reference\n---\n"+dup+"\n")

	ctx, logger := context.Background(), slog.Default()
	const projectID = "test-project-bounded"
	store := storeFor(t, projectID)
	// Three existing rows against a window of two; the duplicate is the oldest-read last.
	for _, c := range []string{
		"Existing memory one has enough content to satisfy minimum length here.",
		"Existing memory two has enough content to satisfy minimum length here.",
		"Dup below window — Duplicate below pre-skip window\n\n" + strings.TrimSpace(dup),
	} {
		if _, _, _, err := store.UpsertWithOptions(ctx, projectID, "fact", c, source, 0.7, importTags, memory.UpsertOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	imported, refused, failed, notRegular, err := importFromDir(ctx, store, projectID, memDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(refused)+len(failed)+len(notRegular) != 0 || imported != 0 {
		t.Fatalf("imported=%d refused=%v failed=%v notRegular=%v, want a plain skip", imported, refused, failed, notRegular)
	}
	if after, _ := store.CountMemories(ctx, projectID); after != 3 {
		t.Fatalf("count = %d, want 3 (duplicate row created)", after)
	}
}
