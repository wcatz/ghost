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
aliases: ["Embedding backfill bug: ticker only swept seen projects."]
type: memory
category: gotcha
importance: 0.8
pinned: false
project: ghost
tags: [embedding, backfill]
created: 2026-07-06
updated: 2026-07-08
source: mcp
related: ["[[other-note-beef0000]]"]
---
> [!info] Mirrored from Ghost — edits here are not synced back.

Embedding backfill bug: ticker only swept seen projects.
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
// line per key (the invariant hasGhostID's scan depends on — it reads to the
// first --- and takes the one ghost_id: line there) and must not change the
// YAML shape of their line, whatever the store holds. ghost_id comes first as
// the convention a reader sees. The note body is not frontmatter and stays
// verbatim.
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
	// Alias is the first content line; the ": " forces quoting so it stays a
	// single valid scalar.
	if !strings.Contains(got, `aliases: ["Body content: stays verbatim, even with colons"]`+"\n") {
		t.Errorf("alias must be quoted single-line preview:\n%s", got)
	}
	// Tags: flow-structural characters force per-item quoting, preserving the
	// tag content rather than stripping it.
	if !strings.Contains(got, `tags: ["a,b", "x[0]", "quo\"te"]`+"\n") {
		t.Errorf("hostile tags must be quoted, not stripped:\n%s", got)
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
	if len(lines) != 11 {
		t.Errorf("frontmatter must hold exactly 11 single-line keys, got %d:\n%s", len(lines), rest[:end])
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
	// ghost_id first: the convention a reader sees. The scan does not need it.
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
	// ghost_id first: the convention a reader sees. The scan does not need it.
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

// TestRenderMemoryTypedEdges: typed frontmatter edge fields for the five
// Breadcrumbs relations — directional keys only on the source endpoint,
// related/elaborates on both, multi-target lists sorted by filename, keys in
// fixed alphabetical order after source:, unknown relations prose-only.
func TestRenderMemoryTypedEdges(t *testing.T) {
	const self = "74a37cba00112233"
	m := memory.Memory{
		ID: self, ProjectID: "ghost", Category: "gotcha",
		Content:    "Embedding backfill bug: ticker only swept seen projects.",
		Importance: 0.8, Source: "mcp", Tags: []string{"embedding", "backfill"},
		CreatedAt: "2026-07-06 12:00:00", UpdatedAt: "2026-07-08 09:30:00",
	}
	links := []memory.Link{
		// Directional, m is source → emit supersedes.
		{SourceID: self, TargetID: "dead000011223344", Relation: "supersedes", Strength: 0.9},
		// Directional, m is source → emit contradicts.
		{SourceID: self, TargetID: "aaaa000011223344", Relation: "contradicts", Strength: 0.65},
		// Directional, m is target → no causes key on this note.
		{SourceID: "cafe000011223344", TargetID: self, Relation: "causes", Strength: 0.7},
		// Symmetric-ish: both endpoints get related / elaborates.
		{SourceID: self, TargetID: "beef000011223344", Relation: "related", Strength: 0.83},
		{SourceID: self, TargetID: "third000011223344", Relation: "related", Strength: 0.5},
		{SourceID: self, TargetID: "fifth000011223344", Relation: "elaborates", Strength: 0.6},
		// Unknown relation: prose only, never a frontmatter key.
		{SourceID: self, TargetID: "beef000011223344", Relation: "mystery", Strength: 0.1},
	}
	fileFor := map[string]string{
		"aaaa000011223344":  "zebra-note-aaaa0000.md",
		"dead000011223344":  "old-note-dead0000.md",
		"beef000011223344":  "other-note-beef0000.md",
		"third000011223344": "alpha-note-third0000.md",
		"fifth000011223344": "detail-note-fifth0000.md",
		"cafe000011223344":  "upstream-cafe0000.md",
	}
	got := renderMemory(m, links, fileFor)

	// ## Related lists only links frontmatter cannot show: directed links where
	// m is the target (no frontmatter key on this endpoint) and relations
	// outside the typed edge keys. Typed, resolvable links emitted to
	// frontmatter appear there only.
	for _, prose := range []string{
		"- [[upstream-cafe0000]] — causes (0.70)",
		"- [[other-note-beef0000]] — mystery (0.10)",
	} {
		if !strings.Contains(got, prose) {
			t.Errorf("## Related missing %q:\n%s", prose, got)
		}
	}
	for _, gone := range []string{
		"- [[old-note-dead0000]] — supersedes (0.90)",
		"- [[zebra-note-aaaa0000]] — contradicts (0.65)",
		"- [[alpha-note-third0000]] — related (0.50)",
		"- [[detail-note-fifth0000]] — elaborates (0.60)",
	} {
		if strings.Contains(got, gone) {
			t.Errorf("## Related must not duplicate frontmatter link %q:\n%s", gone, got)
		}
	}

	// Alphabetical key order after source (contradicts before elaborates,
	// causes absent because m is target only); related targets sorted by
	// filename (alpha < other), not by strength.
	wantKeys := []string{
		"source: mcp\n",
		"contradicts: [\"[[zebra-note-aaaa0000]]\"]\n",
		"elaborates: [\"[[detail-note-fifth0000]]\"]\n",
		"related: [\"[[alpha-note-third0000]]\", \"[[other-note-beef0000]]\"]\n",
		"supersedes: [\"[[old-note-dead0000]]\"]\n",
		"---\n",
	}
	i := strings.Index(got, "\n---\n")
	if i < 0 {
		t.Fatalf("no closing frontmatter fence:\n%s", got)
	}
	front := got[:i+len("\n---\n")]
	pos := 0
	for _, key := range wantKeys {
		j := strings.Index(front[pos:], key)
		if j < 0 {
			t.Fatalf("frontmatter missing or out of order %q (searched from offset %d):\n%s", key, pos, front)
		}
		pos += j + len(key)
	}
	// causes/mystery have no key here; contradicts is positively covered
	// above (target-endpoint absence lives in TargetEndpoint below).
	for _, forbidden := range []string{"causes:", "mystery:"} {
		if strings.Contains(front, forbidden) {
			t.Errorf("frontmatter must not contain %q:\n%s", forbidden, front)
		}
	}
}
