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

// TestRenderMemoryAlias: notes carry an `aliases` flow list so Obsidian's
// graph and Quick Switcher show a readable label instead of the id8-suffixed
// filename. The alias is a single-line preview of the content; the flow form
// keeps the one-line-per-key invariant prune depends on, and ghost_id stays
// first.
func TestRenderMemoryAlias(t *testing.T) {
	m := memory.Memory{
		ID: "74a37cba00112233", ProjectID: "ghost", Category: "gotcha",
		Content:    "Embedding backfill bug: ticker only swept seen projects.",
		Importance: 0.8, Source: "mcp",
		CreatedAt: "2026-07-06 12:00:00", UpdatedAt: "2026-07-08 09:30:00",
	}
	got := renderMemory(m, nil, nil)
	// Content holds ": " so the alias is quoted; it is under 60 chars so it is
	// not truncated.
	if !strings.Contains(got, "aliases: [\"Embedding backfill bug: ticker only swept seen projects.\"]\n") {
		t.Errorf("expected quoted single-line alias:\n%s", got)
	}
	if !strings.HasPrefix(got, "---\nghost_id: 74a37cba00112233\n") {
		t.Errorf("ghost_id must stay the first frontmatter line:\n%s", got)
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

// TestRenderFrontmatterYAMLIndicators covers the value shapes the first
// hardening pass missed: Obsidian-idiomatic '#' tags, a ':'-bearing tag, and
// project values that open with a YAML indicator or end with a colon.
func TestRenderFrontmatterYAMLIndicators(t *testing.T) {
	cases := []struct {
		name        string
		projectID   string
		tags        []string
		wantProject string // exact "project: ..." line, without trailing newline
		wantTags    string // exact "tags: ..." line, without trailing newline
	}{
		{
			name:     "obsidian hash tags and colon tag",
			tags:     []string{"#urgent", "status: open"},
			wantTags: `tags: ["#urgent", "status: open"]`,
		},
		{
			name:        "npm-scope project name",
			projectID:   "@org/app",
			wantProject: `project: "@org/app"`,
		},
		{
			name:        "trailing colon project",
			projectID:   "wip:",
			wantProject: `project: "wip:"`,
		},
		{
			name:     "boolean-like tags stay strings",
			tags:     []string{"no", "on"},
			wantTags: `tags: ["no", "on"]`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pid := c.projectID
			if pid == "" {
				pid = "ghost"
			}
			m := memory.Memory{
				ID: "cafe000011223344", ProjectID: pid, Category: "fact",
				Content: "body", Importance: 0.5, Source: "mcp", Tags: c.tags,
				CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
			}
			got := renderMemory(m, nil, nil)
			if c.wantProject != "" && !strings.Contains(got, c.wantProject+"\n") {
				t.Errorf("want %q in:\n%s", c.wantProject, got)
			}
			if c.wantTags != "" && !strings.Contains(got, c.wantTags+"\n") {
				t.Errorf("want %q in:\n%s", c.wantTags, got)
			}
		})
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

	// ## Related prose first (Errorf, not Fatalf) so a missing frontmatter
	// key below cannot mask a prose regression. Prose still lists every
	// link including contradicts, mystery, and the directed link where m
	// is target.
	for _, prose := range []string{
		"- [[old-note-dead0000]] — supersedes (0.90)",
		"- [[zebra-note-aaaa0000]] — contradicts (0.65)",
		"- [[upstream-cafe0000]] — causes (0.70)",
		"- [[other-note-beef0000]] — mystery (0.10)",
		"- [[alpha-note-third0000]] — related (0.50)",
	} {
		if !strings.Contains(got, prose) {
			t.Errorf("## Related missing %q:\n%s", prose, got)
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

// TestRenderMemoryTypedEdgesFilteredTarget: an other-endpoint absent from
// fileFor (e.g. --project filtered it out) never becomes a typed wikilink;
// prose keeps the short-ID fallback. If every entry for a relation is
// skipped, the key is omitted entirely.
func TestRenderMemoryTypedEdgesFilteredTarget(t *testing.T) {
	m := memory.Memory{
		ID: "74a37cba00112233", ProjectID: "ghost", Category: "fact",
		Content: "Filtered edge target", Importance: 0.5, Source: "mcp",
		CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
	}
	links := []memory.Link{
		{SourceID: m.ID, TargetID: "beef000011223344", Relation: "related", Strength: 0.83},
		{SourceID: m.ID, TargetID: "dead000011223344", Relation: "supersedes", Strength: 0.9},
	}
	// beef resolved; dead filtered out of the vault selection.
	fileFor := map[string]string{"beef000011223344": "other-note-beef0000.md"}
	got := renderMemory(m, links, fileFor)

	if !strings.Contains(got, "related: [\"[[other-note-beef0000]]\"]\n") {
		t.Errorf("resolved related target must appear as typed field:\n%s", got)
	}
	if strings.Contains(got, "supersedes:") {
		t.Errorf("fully filtered relation must omit its key:\n%s", got)
	}
	// The supersedes prose uses short id8 dead0000 because fileFor has no
	// entry for that filtered target.
	if !strings.Contains(got, "- dead0000 — supersedes (0.90)") {
		t.Errorf("want short-ID supersedes prose line:\n%s", got)
	}
}

// TestRenderMemoryTypedEdgesTargetEndpoint: when m is the TARGET of a
// directional link, it emits no typed key for that relation; when m is the
// target of related/elaborates, it still emits pointing at the source.
func TestRenderMemoryTypedEdgesTargetEndpoint(t *testing.T) {
	m := memory.Memory{
		ID: "74a37cba00112233", ProjectID: "ghost", Category: "fact",
		Content: "Target endpoint note", Importance: 0.5, Source: "mcp",
		CreatedAt: "2026-07-10 10:00:00", UpdatedAt: "2026-07-10 10:00:00",
	}
	links := []memory.Link{
		{SourceID: "cafe000011223344", TargetID: m.ID, Relation: "supersedes", Strength: 0.9},
		{SourceID: "beef000011223344", TargetID: m.ID, Relation: "related", Strength: 0.8},
		{SourceID: "fifth000011223344", TargetID: m.ID, Relation: "elaborates", Strength: 0.7},
	}
	fileFor := map[string]string{
		"cafe000011223344":  "upstream-cafe0000.md",
		"beef000011223344":  "other-note-beef0000.md",
		"fifth000011223344": "detail-note-fifth0000.md",
	}
	got := renderMemory(m, links, fileFor)

	if strings.Contains(got, "supersedes:") {
		t.Errorf("target of directed supersedes must not emit supersedes:\n%s", got)
	}
	// This fixture has no contradicts link at all, so the key must never
	// appear (positive contradicts coverage lives in TestRenderMemoryTypedEdges).
	if strings.Contains(got, "contradicts:") {
		t.Errorf("fixture has no contradicts link; frontmatter must not contain it:\n%s", got)
	}
	if !strings.Contains(got, "related: [\"[[other-note-beef0000]]\"]\n") {
		t.Errorf("target of related must emit related at the source:\n%s", got)
	}
	if !strings.Contains(got, "elaborates: [\"[[detail-note-fifth0000]]\"]\n") {
		t.Errorf("target of elaborates must emit elaborates at the source:\n%s", got)
	}
	// Prose still shows the directed edge from the target's point of view.
	if !strings.Contains(got, "- [[upstream-cafe0000]] — supersedes (0.90)") {
		t.Errorf("prose must still list directed link touching m:\n%s", got)
	}
}
