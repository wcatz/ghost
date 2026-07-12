package obsidian

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSlug(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Embedding backfill bug (fixed in v0.9.3): the worker's ticker", "embedding-backfill-bug-fixed-in-v093"},
		{"???", "note"},
		{"", "note"},
		{"UPPER case Words here now okay more words ignored", "upper-case-words-here-now-okay"},
		// Join exceeds 40 chars and the cut lands on a dash: truncate, then
		// trim the trailing dash (result is 39 chars).
		{"abcdefghi abcdefghi abcdefghi abcdefghi abcdefghi", "abcdefghi-abcdefghi-abcdefghi-abcdefghi"},
	}
	for _, c := range cases {
		if got := slug(c.in); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFileName(t *testing.T) {
	m := memory.Memory{ID: "74a37cba00112233", Content: "Embedding backfill bug"}
	if got := fileName(m); got != "embedding-backfill-bug-74a37cba.md" {
		t.Errorf("fileName = %q", got)
	}
}

func TestRenderMemory(t *testing.T) {
	m := memory.Memory{
		ID: "74a37cba00112233", ProjectID: "ghost", Category: "gotcha",
		Content:    "Embedding backfill bug: ticker only swept seen projects.",
		Importance: 0.8, Source: "mcp", Tags: []string{"embedding", "backfill"},
		Pinned: false, CreatedAt: "2026-07-06 12:00:00", UpdatedAt: "2026-07-08 09:30:00",
	}
	links := []memory.Link{{SourceID: m.ID, TargetID: "beef000011223344", Relation: "related", Strength: 0.83}}
	fileFor := map[string]string{"beef000011223344": "other-note-beef0000.md"}
	got := renderMemory(m, links, fileFor)
	want := `---
ghost_id: 74a37cba00112233
type: memory
category: gotcha
importance: 0.8
pinned: false
project: ghost
tags: [embedding, backfill]
created: 2026-07-06
updated: 2026-07-08
source: mcp
---
> [!info] Mirrored from Ghost — edits here are not synced back.

Embedding backfill bug: ticker only swept seen projects.

## Related
- [[other-note-beef0000]] — related (0.83)
`
	if got != want {
		t.Errorf("renderMemory mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// Link target absent from fileFor → plain ID, no broken wikilink.
	got2 := renderMemory(m, links, map[string]string{})
	if strings.Contains(got2, "[[") || !strings.Contains(got2, "beef0000") {
		t.Errorf("missing target should render short ID without wikilink:\n%s", got2)
	}
}

// TestRenderHostileFrontmatter: frontmatter values must occupy exactly one
// line per key (the ghost_id-first invariant prune depends on) and must not
// change the YAML shape of their line, whatever the store holds. The note
// body is not frontmatter and stays verbatim.
func TestRenderHostileFrontmatter(t *testing.T) {
	m := memory.Memory{
		ID: "bad0000011223344", ProjectID: "evil: proj\nect", Category: "fact",
		Content:    "Body content: stays verbatim, even with colons\nand newlines.",
		Importance: 0.7, Source: "mcp",
		Tags:      []string{"a,b", "x[0]", `quo"te`},
		CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
	}
	got := renderMemory(m, nil, nil)

	if !strings.HasPrefix(got, "---\nghost_id: bad0000011223344\n") {
		t.Errorf("ghost_id must stay the first frontmatter line:\n%s", got)
	}
	// Newline flattened to a space, then quoted because of ": ".
	if !strings.Contains(got, "project: \"evil: proj ect\"\n") {
		t.Errorf("hostile project value must be flattened and quoted:\n%s", got)
	}
	// Tags: structural flow characters stripped before the join.
	if !strings.Contains(got, "tags: [ab, x0, quote]\n") {
		t.Errorf("hostile tags must be sanitized:\n%s", got)
	}
	// Body is untouched.
	if !strings.Contains(got, "Body content: stays verbatim, even with colons\nand newlines.") {
		t.Errorf("note body must stay verbatim:\n%s", got)
	}
	// Block integrity: exactly the 10 emitted keys between the fences, one
	// line each — nothing injected a stray line.
	rest := strings.TrimPrefix(got, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("no closing frontmatter fence:\n%s", got)
	}
	lines := strings.Split(rest[:end], "\n")
	if len(lines) != 10 {
		t.Errorf("frontmatter must hold exactly 10 single-line keys, got %d:\n%s", len(lines), rest[:end])
	}
	for _, line := range lines {
		if !strings.Contains(line, ": ") {
			t.Errorf("frontmatter line lost its key-value shape: %q", line)
		}
	}
}

func TestRenderDecision(t *testing.T) {
	d := memory.Decision{
		ID: "dec0000011223344", ProjectID: "ghost", Title: "Use SQLite",
		Decision: "SQLite over Postgres.", Rationale: "Zero infra.",
		Alternatives: []string{"Postgres", "BoltDB"}, Status: "active",
		Tags: []string{"storage"}, CreatedAt: "2026-07-01 08:00:00", UpdatedAt: "2026-07-01 08:00:00",
	}
	got := renderDecision(d)
	// ghost_id must be the first frontmatter key — prune's hasGhostID depends on it.
	if !strings.HasPrefix(got, "---\nghost_id: dec0000011223344\n") {
		t.Errorf("renderDecision must open with ghost_id-first frontmatter:\n%s", got)
	}
	for _, want := range []string{"ghost_id: dec0000011223344", "type: decision", "status: active",
		"# Use SQLite", "SQLite over Postgres.", "## Rationale", "Zero infra.", "## Alternatives", "- Postgres"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderDecision missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderTask(t *testing.T) {
	tk := memory.Task{
		ID: "task000011223344", ProjectID: "ghost", Title: "Ship mirror",
		Description: "Build it.", Status: "active", Priority: 2,
		CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
	}
	got := renderTask(tk)
	// ghost_id must be the first frontmatter key — prune's hasGhostID depends on it.
	if !strings.HasPrefix(got, "---\nghost_id: task000011223344\n") {
		t.Errorf("renderTask must open with ghost_id-first frontmatter:\n%s", got)
	}
	for _, want := range []string{"ghost_id: task000011223344", "type: task", "status: active",
		"priority: 2", "# Ship mirror", "Build it."} {
		if !strings.Contains(got, want) {
			t.Errorf("renderTask missing %q in:\n%s", want, got)
		}
	}
}
