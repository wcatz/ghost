# Obsidian Typed Edge Fields Implementation Plan

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Emit Breadcrumbs-compatible typed edge frontmatter fields (`causes`, `contradicts`, `elaborates`, `related`, `supersedes`) on memory notes while leaving `## Related` prose unchanged.

**Architecture:** A new `fmEdges` helper in `internal/obsidian/render.go` groups the links already passed to `renderMemory` into the five relation buckets under the endpoint rules from the design spec, then appends one sorted YAML flow-list line per non-empty bucket after `source:`. `export.go` and the CLI are untouched; determinism comes from fixed key order plus lexicographic filename sort of targets.

**Tech Stack:** Go 1.26, existing `internal/obsidian` render/export tests (`go test`), no new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-22-obsidian-typed-edge-fields-design.md`
**Branch:** `feat/obsidian-typed-edge-fields` (already created; plan + spec live here)

---

## Background

`renderMemory(m, links, fileFor)` (`internal/obsidian/render.go:161`) writes 10 fixed frontmatter keys then a `## Related` body listing every link. Breadcrumbs reads typed frontmatter properties, not prose.

Endpoint rules (from the approved design):

| Relation | Emit on | Points at |
|---|---|---|
| `causes`, `contradicts`, `supersedes` | source endpoint only | target filename |
| `related`, `elaborates` | both endpoints | the other endpoint’s filename |

Then: resolve through `fileFor` (skip missing — never a dead wikilink), sort filenames by byte order, omit empty keys. Unknown relations never get a frontmatter key (prose only).

`## Related` body output must stay byte-identical.

## File structure

| File | Change |
|---|---|
| `internal/obsidian/render.go` | Add `edgeRelations`, `fmEdges`; call after `source:` in `renderMemory` |
| `internal/obsidian/render_test.go` | Update `TestRenderMemory` golden; add mixed / filtered / target-endpoint tests |
| `internal/obsidian/export_test.go` | Add `TestExportTypedEdgesDeterministic` + `snapshotVault` helper |
| `docs/superpowers/plans/2026-09-22-obsidian-typed-edge-fields.md` | This file |

No changes to `export.go`, `cmd/ghost/main.go`, schema, store, or MCP.

---

### Task 0: Baseline and plan commit

**Files:** create `docs/superpowers/plans/2026-09-22-obsidian-typed-edge-fields.md`

- [ ] **Step 1:** Confirm branch and green baseline:

```bash
git branch --show-current   # feat/obsidian-typed-edge-fields
go test ./internal/obsidian/ -count=1
```

Expected: branch name printed; all `internal/obsidian` tests `ok`.

- [ ] **Step 2:** Commit the plan:

```bash
git add docs/superpowers/plans/2026-09-22-obsidian-typed-edge-fields.md
git commit -m "docs: add Obsidian typed edge fields plan"
```

Expected: one new commit on `feat/obsidian-typed-edge-fields`.

---

### Task 1: Failing render tests for typed edge fields

**Files:**
- Modify: `internal/obsidian/render_test.go`
- Test only in this task (implementation is Task 2)

- [ ] **Step 1: Update `TestRenderMemory` golden**

In `internal/obsidian/render_test.go`, the existing test builds one `related` link with `fileFor` resolving `beef000011223344` → `other-note-beef0000.md`. After this feature the frontmatter gains `related: ["[[other-note-beef0000]]"]` between `source: mcp` and `---`.

Replace the `want` string in `TestRenderMemory` so it reads exactly:

```go
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
```

Leave the second assertion in that test (empty `fileFor` → short ID, no `[[`) unchanged — it must keep passing because a missing target omits the typed key entirely.

- [ ] **Step 2: Add `TestRenderMemoryTypedEdges`**

Append to `internal/obsidian/render_test.go`:

```go
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
		"dead000011223344": "old-note-dead0000.md",
		"beef000011223344": "other-note-beef0000.md",
		"third000011223344": "alpha-note-third0000.md",
		"fifth000011223344": "detail-note-fifth0000.md",
		"cafe000011223344": "upstream-cafe0000.md",
	}
	got := renderMemory(m, links, fileFor)

	// Alphabetical key order after source; no causes (m is target only);
	// related targets sorted by filename (alpha < other), not by strength.
	wantKeys := []string{
		"source: mcp\n",
		"elaborates: [\"[[detail-note-fifth0000]]\"]\n",
		"related: [\"[[alpha-note-third0000]]\", \"[[other-note-beef0000]]\"]\n",
		"supersedes: [\"[[old-note-dead0000]]\"]\n",
		"---\n",
	}
	front := got
	if i := strings.Index(got, "\n---\n"); i >= 0 {
		front = got[:i+len("\n---\n")]
	}
	pos := 0
	for _, key := range wantKeys {
		j := strings.Index(front[pos:], key)
		if j < 0 {
			t.Fatalf("frontmatter missing or out of order %q (searched from offset %d):\n%s", key, pos, front)
		}
		pos += j + len(key)
	}
	for _, forbidden := range []string{"causes:", "contradicts:", "mystery:"} {
		if strings.Contains(front, forbidden) {
			t.Errorf("frontmatter must not contain %q:\n%s", forbidden, front)
		}
	}
	// ## Related prose still lists every link including mystery and the
	// directed link where m is target.
	for _, prose := range []string{
		"- [[old-note-dead0000]] — supersedes (0.90)",
		"- [[upstream-cafe0000]] — causes (0.70)",
		"- [[other-note-beef0000]] — mystery (0.10)",
		"- [[alpha-note-third0000]] — related (0.50)",
	} {
		if !strings.Contains(got, prose) {
			t.Errorf("## Related missing %q:\n%s", prose, got)
		}
	}
}
```

- [ ] **Step 3: Add `TestRenderMemoryTypedEdgesFilteredTarget` and target-endpoint case**

Append to `internal/obsidian/render_test.go`:

```go
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
	if strings.Contains(got, "dead0000") && !strings.Contains(got, "- dead0000 — supersedes") {
		t.Errorf("filtered target must still appear as short ID prose:\n%s", got)
	}
	// Related prose still has the short-ID line (id8 of beef… is present as
	// wikilink; dead… as plain id8 — assert the plain form exists).
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
		"cafe000011223344": "upstream-cafe0000.md",
		"beef000011223344": "other-note-beef0000.md",
		"fifth000011223344": "detail-note-fifth0000.md",
	}
	got := renderMemory(m, links, fileFor)

	if strings.Contains(got, "supersedes:") {
		t.Errorf("target of directed supersedes must not emit supersedes:\n%s", got)
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
```

- [ ] **Step 4: Run tests — expect failures**

```bash
go test ./internal/obsidian/ -count=1 -run 'TestRenderMemory' -v
```

Expected: FAIL. `TestRenderMemory` missing `related: ["[[other-note-beef0000]]"]`; `TestRenderMemoryTypedEdges*` fail (undefined behavior / missing keys). Unlinked tests (`TestRenderMemoryAlias`, `TestRenderHostileFrontmatter`) may still pass — that is fine.

Do **not** commit until Task 2 makes them pass.

---

### Task 2: Implement `fmEdges` and wire it into `renderMemory`

**Files:**
- Modify: `internal/obsidian/render.go` (imports + new helper + one call site)

- [ ] **Step 1: Add `sort` to imports**

In `internal/obsidian/render.go`, change the import block to:

```go
import (
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)
```

- [ ] **Step 2: Add `edgeRelations` and `fmEdges` immediately before `renderMemory`**

Insert this block after `fmTags` (ends ~line 159) and before `func renderMemory`:

```go
// edgeRelations is the fixed alphabetical emission order of the five typed
// frontmatter edge keys. directed=true means the field is written only on
// the link's source endpoint; directed=false (related, elaborates) writes on
// both endpoints, matching ## Related's both-endpoint view.
var edgeRelations = []struct {
	name     string
	directed bool
}{
	{name: "causes", directed: true},
	{name: "contradicts", directed: true},
	{name: "elaborates", directed: false},
	{name: "related", directed: false},
	{name: "supersedes", directed: true},
}

// fmEdges appends typed edge fields for m after the existing frontmatter
// keys. Targets missing from fileFor are skipped (no dead wikilinks under
// --project); a relation with zero remaining targets omits its key. Filenames
// are sorted so re-exports stay byte-identical regardless of GetLinks order.
func fmEdges(b *strings.Builder, m memory.Memory, links []memory.Link, fileFor map[string]string) {
	byRel := make(map[string][]string)
	seen := make(map[string]map[string]bool)
	for _, l := range links {
		var directed bool
		var known bool
		for _, er := range edgeRelations {
			if er.name == l.Relation {
				directed, known = er.directed, true
				break
			}
		}
		if !known {
			continue
		}
		other := l.TargetID
		if directed {
			if l.SourceID != m.ID {
				continue
			}
		} else if other == m.ID {
			other = l.SourceID
		}
		f, ok := fileFor[other]
		if !ok {
			continue
		}
		if seen[l.Relation] == nil {
			seen[l.Relation] = make(map[string]bool)
		}
		if seen[l.Relation][f] {
			continue
		}
		seen[l.Relation][f] = true
		byRel[l.Relation] = append(byRel[l.Relation], f)
	}
	for _, er := range edgeRelations {
		targets := byRel[er.name]
		if len(targets) == 0 {
			continue
		}
		sort.Strings(targets)
		items := make([]string, 0, len(targets))
		for _, f := range targets {
			items = append(items, yamlScalar("[["+strings.TrimSuffix(f, ".md")+"]]", true))
		}
		fmt.Fprintf(b, "%s: [%s]\n", er.name, strings.Join(items, ", "))
	}
}
```

- [ ] **Step 3: Call `fmEdges` after `source:`**

In `renderMemory`, replace:

```go
	fm(&b, "source", m.Source)
	b.WriteString("---\n")
```

with:

```go
	fm(&b, "source", m.Source)
	fmEdges(&b, m, links, fileFor)
	b.WriteString("---\n")
```

- [ ] **Step 4: Run render tests**

```bash
go test ./internal/obsidian/ -count=1 -run 'TestRenderMemory' -v
```

Expected: PASS — all `TestRenderMemory*` including the three new typed-edge tests.

- [ ] **Step 5: Run the whole package**

```bash
go test ./internal/obsidian/ -count=1
```

Expected: all `ok` (export tests still pass; `## Related` body unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/obsidian/render.go internal/obsidian/render_test.go
git commit -m "feat(obsidian): emit typed edge frontmatter fields on memory notes"
```

Expected: one commit; working tree clean for those files.

---

### Task 3: Export-level determinism and typed-key integration test

**Files:**
- Modify: `internal/obsidian/export_test.go`

- [ ] **Step 1: Add `snapshotVault` helper and `TestExportTypedEdgesDeterministic`**

Append to `internal/obsidian/export_test.go`:

```go
// snapshotVault reads every .md under root into relpath → contents, for
// byte-comparing two export passes.
func snapshotVault(t *testing.T, root string) map[string]string {
	t.Helper()
	got := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		got[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestExportTypedEdgesDeterministic: a directed + a symmetric link produce
// typed frontmatter on the source note; a second export of the same store is
// byte-identical (writeIfChanged no-churn holds with fmEdges in the path).
func TestExportTypedEdgesDeterministic(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	id1, err := store.Create(ctx, "ghost", memory.Memory{
		Category: "fact", Content: "New fact that supersedes the old", Importance: 0.8, Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := store.Create(ctx, "ghost", memory.Memory{
		Category: "fact", Content: "Old fact superseded by the new", Importance: 0.7, Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, id1, id2, "supersedes", 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateLink(ctx, id1, id2, "related", 0.5, "auto"); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("first export: %v", err)
	}
	first := snapshotVault(t, vault)
	if len(first) == 0 {
		t.Fatal("expected notes after first export")
	}

	// Source note has both typed keys; target note has only related.
	var srcBody, tgtBody string
	for rel, content := range first {
		switch {
		case strings.Contains(rel, id1[:8]):
			srcBody = content
		case strings.Contains(rel, id2[:8]):
			tgtBody = content
		}
	}
	if srcBody == "" || tgtBody == "" {
		t.Fatalf("missing source/target notes in snapshot: src=%q tgt=%q", srcBody, tgtBody)
	}
	if !strings.Contains(srcBody, "supersedes: ") {
		t.Errorf("source note must carry supersedes typed field:\n%s", srcBody)
	}
	if !strings.Contains(srcBody, "related: ") {
		t.Errorf("source note must carry related typed field:\n%s", srcBody)
	}
	if strings.Contains(tgtBody, "supersedes:") {
		t.Errorf("target of directed supersedes must not emit supersedes:\n%s", tgtBody)
	}
	if !strings.Contains(tgtBody, "related: ") {
		t.Errorf("target must still emit related typed field:\n%s", tgtBody)
	}

	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("second export: %v", err)
	}
	second := snapshotVault(t, vault)
	if len(first) != len(second) {
		t.Fatalf("re-export changed file count: %d -> %d", len(first), len(second))
	}
	for rel, content := range first {
		if second[rel] != content {
			t.Errorf("re-export changed %s", rel)
		}
	}
}
```

- [ ] **Step 2: Run the new export test**

```bash
go test ./internal/obsidian/ -count=1 -run 'TestExportTypedEdgesDeterministic' -v
```

Expected: PASS (Task 2 already landed `fmEdges`). If FAIL, inspect the printed note bodies before changing production code.

- [ ] **Step 3: Full package + vet**

```bash
go vet ./internal/obsidian/
go test ./internal/obsidian/ -count=1
```

Expected: vet silent; all tests `ok`.

- [ ] **Step 4: Commit**

```bash
git add internal/obsidian/export_test.go
git commit -m "test(obsidian): assert typed edge fields and re-export determinism"
```

---

### Task 4: Full verification

**Files:** none (verification only)

- [ ] **Step 1:** Repo-wide vet and tests:

```bash
go vet ./...
go test ./... -count=1
```

Expected: vet silent; every package `ok`. No skipped/suppressed tests.

- [ ] **Step 2:** Confirm scope — only intended files changed:

```bash
git status
git diff main...HEAD --stat
```

Expected: only `docs/superpowers/specs/2026-09-22-obsidian-typed-edge-fields-design.md`, `docs/superpowers/plans/2026-09-22-obsidian-typed-edge-fields.md`, `internal/obsidian/render.go`, `internal/obsidian/render_test.go`, `internal/obsidian/export_test.go` (plus plan/spec commits from Task 0 / brainstorming).

- [ ] **Step 3:** Stop. Do **not** push, open a PR, or merge unless the user asks.

---

## Self-review (plan author)

1. **Spec coverage:** keys/order (edgeRelations), value shape (yamlScalar flow list of `[[stem]]`), direction rules (directed flag), `## Related` unchanged (no edits to prose loop), filtered targets (fileFor skip + omit empty), determinism (sort.Strings + export snapshot test), tests 1–5 in the design’s Testing section (mixed / filtered / no-links via hostile+alias unchanged / goldens / export determinism), implementation shape (`fmEdges` only; export.go untouched).
2. **Placeholders:** None — every step contains complete test/implementation code and exact commands with expected outcomes.
3. **Type consistency:** `fmEdges(b *strings.Builder, m memory.Memory, links []memory.Link, fileFor map[string]string)` matches the call site and `edgeRelations` field names (`name`, `directed`) are used identically in lookup and emit loops.
