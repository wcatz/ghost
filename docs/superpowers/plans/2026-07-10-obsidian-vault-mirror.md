# Obsidian Vault Mirror Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ghost obsidian export|sync` mirrors memories, decisions, and tasks into a plain-Markdown folder Obsidian reads natively — one-way, prune-safe, zero new dependencies.

**Architecture:** New `internal/obsidian/` package (render → vault-safety → export walk → poll loop) consuming the concrete `*memory.Store` over a read-only SQLite connection (hook.go pattern). CLI dispatch matches the existing hand-parsed-flags style in `cmd/ghost/main.go`. Spec: `docs/superpowers/specs/2026-07-10-obsidian-vault-mirror-design.md`.

**Tech Stack:** Go 1.26, stdlib only (no fsnotify, no yaml dep — hand-rolled fixed-field frontmatter). Change detection via `PRAGMA data_version` (changes only when *another* connection commits — exactly right for a read-only watcher).

**Verified API surface used (do not invent others):**
- `memory.OpenDB(dbPath) (*sql.DB, error)` — schema.go:243; read-only DSN pattern: `sql.Open("sqlite", dbPath+"?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")` — hook.go:111
- `memory.NewStore(db, *slog.Logger) *Store` — store.go:59
- `Store.ListProjects(ctx) ([]memory.Project{ID,Path,Name,...})`
- `Store.GetAll(ctx, projectID, limit) ([]memory.Memory{ID,ProjectID,Category,Content,Importance,Source,Tags,Pinned,CreatedAt,UpdatedAt,...})`
- `Store.GetLinks(ctx, memoryID) ([]memory.Link{SourceID,TargetID,Relation,Strength,...})` — valid links only, both endpoints
- `Store.ListTasks(ctx, projectID, status, limit)` / `Store.ListDecisions(ctx, projectID, status, limit)` — empty status = all
- `Store.EnsureProject/Create/CreateLink/CreateTask/RecordDecision/Delete` (tests only)
- `config.Load()`, `config.DataDir()`; timestamps are SQLite `YYYY-MM-DD HH:MM:SS` strings — date = `s[:10]`

---

### Task 1: ObsidianConfig

**Files:**
- Modify: `internal/config/config.go` (struct + defaults map)
- Test: `internal/config/config_test.go` (append)

- [ ] **Step 1: Write the failing test** (append to existing `config_test.go`, matching its existing style)

```go
func TestObsidianDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Obsidian.VaultDir != "" {
		t.Errorf("VaultDir default = %q, want empty (resolved at use time)", cfg.Obsidian.VaultDir)
	}
	if cfg.Obsidian.Interval != "30s" {
		t.Errorf("Interval default = %q, want 30s", cfg.Obsidian.Interval)
	}
}
```

Note: `vault_dir` defaults to **empty** in config; the CLI resolves empty → `~/Documents/GhostVault` at run time (`os.UserHomeDir`), because compiled defaults can't expand `~`.

- [ ] **Step 2: Run: `go test ./internal/config/ -run TestObsidianDefaults -v` — expect FAIL (`cfg.Obsidian undefined`)**

- [ ] **Step 3: Implement.** In `config.go`: add to the `Config` struct `Obsidian ObsidianConfig \`koanf:"obsidian"\``; add:

```go
// ObsidianConfig controls the Obsidian vault mirror (ghost obsidian export|sync).
type ObsidianConfig struct {
	VaultDir string `koanf:"vault_dir"` // empty = ~/Documents/GhostVault, resolved by the CLI
	Interval string `koanf:"interval"`  // sync poll cadence, time.ParseDuration format
}
```

and to the `defaults` map: `"obsidian.vault_dir": "",` and `"obsidian.interval": "30s",`.

- [ ] **Step 4: Run: `go test ./internal/config/ -v` — expect PASS (all, not just the new one)**
- [ ] **Step 5: Commit: `git add internal/config/ && git commit -m "feat(obsidian): add obsidian config section"`**

---

### Task 2: Renderer — slug, frontmatter, memory notes

**Files:**
- Create: `internal/obsidian/render.go`, `internal/obsidian/render_test.go`

- [ ] **Step 1: Write failing tests**

```go
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
		Content: "Embedding backfill bug: ticker only swept seen projects.",
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
```

- [ ] **Step 2: Run: `go test ./internal/obsidian/ -v` — expect FAIL (package doesn't exist / undefined)**

- [ ] **Step 3: Implement `render.go`**

```go
// Package obsidian mirrors Ghost's store into a plain-Markdown folder that
// Obsidian reads natively. Strictly one-way: Ghost → vault.
package obsidian

import (
	"fmt"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const banner = "> [!info] Mirrored from Ghost — edits here are not synced back.\n"

// slug derives a stable-ish, readable filename prefix from the first ~6
// content words: lowercase, alnum-only, dash-joined, max 40 chars.
func slug(content string) string {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(content)) {
		var b strings.Builder
		for _, r := range w {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			words = append(words, b.String())
		}
		if len(words) == 6 {
			break
		}
	}
	s := strings.Join(words, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "note"
	}
	return s
}

func id8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func fileName(m memory.Memory) string {
	return slug(m.Content) + "-" + id8(m.ID) + ".md"
}

func date(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// fm writes one frontmatter line.
func fm(b *strings.Builder, key, val string) {
	fmt.Fprintf(b, "%s: %s\n", key, val)
}

func renderMemory(m memory.Memory, links []memory.Link, fileFor map[string]string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", m.ID)
	fm(&b, "type", "memory")
	fm(&b, "category", m.Category)
	fm(&b, "importance", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", m.Importance), "0"), "."))
	fm(&b, "pinned", fmt.Sprintf("%v", m.Pinned))
	fm(&b, "project", m.ProjectID)
	fm(&b, "tags", "["+strings.Join(m.Tags, ", ")+"]")
	fm(&b, "created", date(m.CreatedAt))
	fm(&b, "updated", date(m.UpdatedAt))
	fm(&b, "source", m.Source)
	b.WriteString("---\n")
	b.WriteString(banner)
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(m.Content, "\n"))
	b.WriteString("\n")
	if len(links) > 0 {
		b.WriteString("\n## Related\n")
		for _, l := range links {
			other := l.TargetID
			if other == m.ID {
				other = l.SourceID
			}
			if f, ok := fileFor[other]; ok {
				fmt.Fprintf(&b, "- [[%s]] — %s (%.2f)\n", strings.TrimSuffix(f, ".md"), l.Relation, l.Strength)
			} else {
				fmt.Fprintf(&b, "- %s — %s (%.2f)\n", id8(other), l.Relation, l.Strength)
			}
		}
	}
	return b.String()
}
```

Note on `importance`: `%.2f` then trim trailing zeros → `0.8`, `0.75`, `1`. Adjust the test expectation only if you change the format — they must agree.

- [ ] **Step 4: Run: `go test ./internal/obsidian/ -v` — expect PASS**
- [ ] **Step 5: Commit: `git add internal/obsidian/ && git commit -m "feat(obsidian): note renderer — slug, frontmatter, memory notes"`**

---

### Task 3: Renderer — decision + task notes

**Files:** Modify: `internal/obsidian/render.go`; Test: `internal/obsidian/render_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestRenderDecision(t *testing.T) {
	d := memory.Decision{
		ID: "dec0000011223344", ProjectID: "ghost", Title: "Use SQLite",
		Decision: "SQLite over Postgres.", Rationale: "Zero infra.",
		Alternatives: []string{"Postgres", "BoltDB"}, Status: "active",
		Tags: []string{"storage"}, CreatedAt: "2026-07-01 08:00:00", UpdatedAt: "2026-07-01 08:00:00",
	}
	got := renderDecision(d)
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
	for _, want := range []string{"ghost_id: task000011223344", "type: task", "status: active",
		"priority: 2", "# Ship mirror", "Build it."} {
		if !strings.Contains(got, want) {
			t.Errorf("renderTask missing %q in:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run: `go test ./internal/obsidian/ -v` — expect FAIL (undefined renderDecision/renderTask)**

- [ ] **Step 3: Implement** (in `render.go`; decisions/tasks slug from Title: add `func fileNameFor(title, id string) string { return slug(title) + "-" + id8(id) + ".md" }`)

```go
func renderDecision(d memory.Decision) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", d.ID)
	fm(&b, "type", "decision")
	fm(&b, "status", d.Status)
	fm(&b, "project", d.ProjectID)
	fm(&b, "tags", "["+strings.Join(d.Tags, ", ")+"]")
	fm(&b, "created", date(d.CreatedAt))
	fm(&b, "updated", date(d.UpdatedAt))
	b.WriteString("---\n")
	b.WriteString(banner)
	fmt.Fprintf(&b, "\n# %s\n\n%s\n", d.Title, strings.TrimRight(d.Decision, "\n"))
	if d.Rationale != "" {
		fmt.Fprintf(&b, "\n## Rationale\n\n%s\n", strings.TrimRight(d.Rationale, "\n"))
	}
	if len(d.Alternatives) > 0 {
		b.WriteString("\n## Alternatives\n\n")
		for _, a := range d.Alternatives {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	return b.String()
}

func renderTask(t memory.Task) string {
	var b strings.Builder
	b.WriteString("---\n")
	fm(&b, "ghost_id", t.ID)
	fm(&b, "type", "task")
	fm(&b, "status", t.Status)
	fm(&b, "priority", fmt.Sprintf("%d", t.Priority))
	fm(&b, "project", t.ProjectID)
	fm(&b, "created", date(t.CreatedAt))
	fm(&b, "updated", date(t.UpdatedAt))
	b.WriteString("---\n")
	b.WriteString(banner)
	fmt.Fprintf(&b, "\n# %s\n", t.Title)
	if t.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(t.Description, "\n"))
	}
	if t.Notes != "" {
		fmt.Fprintf(&b, "\n## Notes\n\n%s\n", strings.TrimRight(t.Notes, "\n"))
	}
	return b.String()
}
```

- [ ] **Step 4: Run: `go test ./internal/obsidian/ -v` — expect PASS**
- [ ] **Step 5: Commit: `git commit -am "feat(obsidian): decision and task note renderers"`**

---

### Task 4: Vault safety — marker, diff-write, prune guard

**Files:** Create: `internal/obsidian/vault.go`, `internal/obsidian/vault_test.go`

- [ ] **Step 1: Failing tests**

```go
package obsidian

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureVault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault")
	if err := ensureVault(dir); err != nil { // fresh dir: created + marker
		t.Fatalf("fresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if err := ensureVault(dir); err != nil { // idempotent
		t.Fatalf("second run: %v", err)
	}
	// Existing non-empty dir WITHOUT marker → refuse.
	dirty := t.TempDir()
	os.WriteFile(filepath.Join(dirty, "keep.md"), []byte("user file"), 0o644)
	if err := ensureVault(dirty); err == nil {
		t.Fatal("expected refusal for unmarked non-empty dir")
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.md")
	w1, err := writeIfChanged(p, "hello")
	if err != nil || !w1 {
		t.Fatalf("first write: wrote=%v err=%v", w1, err)
	}
	w2, _ := writeIfChanged(p, "hello")
	if w2 {
		t.Fatal("unchanged content must not rewrite")
	}
	w3, _ := writeIfChanged(p, "hello2")
	if !w3 {
		t.Fatal("changed content must rewrite")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, markerName), []byte(`{"schema_version":1}`), 0o644)
	sub := filepath.Join(root, "proj", "Memories")
	os.MkdirAll(sub, 0o755)
	ghostNote := "---\nghost_id: dead0000\ntype: memory\n---\nbody\n"
	os.WriteFile(filepath.Join(sub, "stale-dead0000.md"), []byte(ghostNote), 0o644)
	os.WriteFile(filepath.Join(sub, "user-note.md"), []byte("no frontmatter"), 0o644)

	if err := prune(root, []string{"proj"}, map[string]bool{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "stale-dead0000.md")); !os.IsNotExist(err) {
		t.Fatal("stale ghost note should be deleted")
	}
	if _, err := os.Stat(filepath.Join(sub, "user-note.md")); err != nil {
		t.Fatal("user note must survive")
	}
	// No marker → prune must refuse to delete anything.
	os.Remove(filepath.Join(root, markerName))
	os.WriteFile(filepath.Join(sub, "stale2-beef0000.md"), []byte(ghostNote), 0o644)
	if err := prune(root, []string{"proj"}, map[string]bool{}); err == nil {
		t.Fatal("prune without marker must error")
	}
}
```

- [ ] **Step 2: Run: `go test ./internal/obsidian/ -run 'TestEnsureVault|TestWriteIfChanged|TestPrune' -v` — expect FAIL**

- [ ] **Step 3: Implement `vault.go`**

```go
package obsidian

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const markerName = ".ghost-vault"

// ensureVault prepares dir as a Ghost-managed mirror target. Fresh or empty
// dirs are initialized with the marker; a non-empty dir without the marker is
// refused — Ghost never adopts a folder it didn't create.
func ensureVault(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create vault dir: %w", err)
		}
		entries = nil
	} else if err != nil {
		return fmt.Errorf("read vault dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, markerName)); err == nil {
		return nil
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s exists, is not empty, and has no %s marker — refusing to manage it (use a fresh directory)", dir, markerName)
	}
	return os.WriteFile(filepath.Join(dir, markerName), []byte(`{"schema_version":1}`+"\n"), 0o644)
}

// writeIfChanged writes content atomically (temp+rename), skipping the write
// when the file already has identical content — no mtime churn.
func writeIfChanged(path, content string) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".ghost-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// hasGhostID reports whether a file's frontmatter carries a ghost_id key —
// the only files prune may touch. Only the frontmatter block (between the
// opening and closing --- lines) is scanned, never the note body.
func hasGhostID(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return "", false
	}
	for _, line := range lines[1:] {
		if line == "---" { // end of frontmatter — stop before the body
			break
		}
		if id, ok := strings.CutPrefix(line, "ghost_id: "); ok {
			return strings.TrimSpace(id), true
		}
	}
	return "", false
}

// prune deletes Ghost-managed .md files under the given vault subtrees whose
// ghost_id is not in keep. All three guards from the spec are enforced.
func prune(root string, subtrees []string, keep map[string]bool) error {
	if _, err := os.Stat(filepath.Join(root, markerName)); err != nil {
		return fmt.Errorf("refusing to prune: %s marker not found in %s", markerName, root)
	}
	for _, sub := range subtrees {
		base := filepath.Join(root, sub)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil //nolint:nilerr // missing subtree is fine
			}
			if id, ok := hasGhostID(path); ok && !keep[id] {
				return os.Remove(path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run: `go test ./internal/obsidian/ -v` — expect PASS.** Check the `hasGhostID` frontmatter-end logic against the test with a body containing `ghost_id:`-like text if you harden it — v1 scans only until the closing `---` line after the first.
- [ ] **Step 5: Commit: `git add internal/obsidian/ && git commit -m "feat(obsidian): vault marker, atomic diff-writes, guarded pruning"`**

---

### Task 5: Export walk

**Files:** Create: `internal/obsidian/export.go`, `internal/obsidian/export_test.go`

- [ ] **Step 1: Failing integration test**

```go
package obsidian

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func seedStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "ghost", "/tmp/ghost", "ghost"); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestExport(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	id1, _ := store.Create(ctx, "ghost", memory.Memory{Category: "gotcha", Content: "First fact about embedding", Importance: 0.8, Source: "mcp"})
	id2, _ := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Second fact about linking", Importance: 0.7, Source: "mcp"})
	if err := store.CreateLink(ctx, id1, id2, "related", 0.83, "auto"); err != nil {
		t.Fatal(err)
	}
	store.RecordDecision(ctx, "ghost", "Use SQLite", "SQLite it is", "zero infra", []string{"Postgres"}, nil)
	store.CreateTask(ctx, "ghost", "Ship mirror", "Build it", 2)

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatalf("export: %v", err)
	}

	mems, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems) != 2 {
		t.Fatalf("want 2 memory notes, got %d", len(mems))
	}
	// Wikilink present between the two memories.
	data, _ := os.ReadFile(mems[0])
	other, _ := os.ReadFile(mems[1])
	if !strings.Contains(string(data)+string(other), "[[") {
		t.Error("expected a wikilink in Related section")
	}
	decs, _ := filepath.Glob(filepath.Join(vault, "ghost", "Decisions", "*.md"))
	tasks, _ := filepath.Glob(filepath.Join(vault, "ghost", "Tasks", "*.md"))
	if len(decs) != 1 || len(tasks) != 1 {
		t.Fatalf("want 1 decision + 1 task, got %d + %d", len(decs), len(tasks))
	}
	// Note: RecordDecision also saves a companion memory — memory note count
	// may include it; adjust the assertion above if so (verify against actual
	// behavior, decisions.go RecordDecision doc says "also saves as a memory").

	// Idempotence: second export rewrites nothing (mtimes unchanged).
	before, _ := os.Stat(mems[0])
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(mems[0])
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged note was rewritten")
	}

	// Deletion prunes.
	if err := store.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	mems2, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(mems2) >= len(mems) {
		t.Errorf("deleted memory's note should be pruned: %d -> %d", len(mems), len(mems2))
	}
}
```

**Heads-up baked into the test:** `RecordDecision` also creates a companion memory (decisions.go doc comment) — the first memory-count assertion will likely see 3, not 2. Run once, read the actual count, and pin the assertion to reality. Do not weaken the wikilink/prune/mtime assertions.

- [ ] **Step 2: Run: `go test ./internal/obsidian/ -run TestExport -v` — expect FAIL (undefined Exporter)**

- [ ] **Step 3: Implement `export.go`**

```go
package obsidian

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/wcatz/ghost/internal/memory"
)

const globalProjectID = "_global"

// Exporter mirrors the store into a vault directory.
type Exporter struct {
	Store  *memory.Store
	Logger *slog.Logger
}

// Export performs one full deterministic mirror pass. projectFilter of ""
// mirrors everything; otherwise that project (by ID or name) plus _global.
func (e *Exporter) Export(ctx context.Context, vaultDir, projectFilter string) error {
	if err := ensureVault(vaultDir); err != nil {
		return err
	}
	projects, err := e.Store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	selected := projects[:0:0]
	matched := false
	for _, p := range projects {
		switch {
		case projectFilter == "":
			selected = append(selected, p)
		case p.ID == projectFilter || p.Name == projectFilter:
			selected = append(selected, p)
			matched = true
		case p.ID == globalProjectID:
			selected = append(selected, p) // globals ride along with any filter
		}
	}
	if projectFilter != "" && !matched {
		return fmt.Errorf("no project matches %q", projectFilter)
	}

	// Pass 1: load all memories, build the global id → filename map for wikilinks.
	type projData struct {
		p        memory.Project
		folder   string
		memories []memory.Memory
	}
	var data []projData
	fileFor := make(map[string]string)
	for _, p := range selected {
		mems, err := e.Store.GetAll(ctx, p.ID, 100000)
		if err != nil {
			return fmt.Errorf("load memories for %s: %w", p.ID, err)
		}
		folder := folderName(p)
		for _, m := range mems {
			fileFor[m.ID] = fileName(m)
		}
		data = append(data, projData{p: p, folder: folder, memories: mems})
	}

	// Pass 2: render + diff-write + collect keep-set, then prune.
	keep := make(map[string]bool)
	var subtrees []string
	written, skipped := 0, 0
	for _, d := range data {
		subtrees = append(subtrees, d.folder)
		for _, m := range d.memories {
			links, err := e.Store.GetLinks(ctx, m.ID)
			if err != nil {
				return fmt.Errorf("links for %s: %w", m.ID, err)
			}
			keep[m.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Memories", fileFor[m.ID]), renderMemory(m, links, fileFor))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		decisions, err := e.Store.ListDecisions(ctx, d.p.ID, "", 100000)
		if err != nil {
			return fmt.Errorf("decisions for %s: %w", d.p.ID, err)
		}
		for _, dec := range decisions {
			keep[dec.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Decisions", fileNameFor(dec.Title, dec.ID)), renderDecision(dec))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
		tasks, err := e.Store.ListTasks(ctx, d.p.ID, "", 100000)
		if err != nil {
			return fmt.Errorf("tasks for %s: %w", d.p.ID, err)
		}
		for _, tk := range tasks {
			keep[tk.ID] = true
			w, err := writeIfChanged(filepath.Join(vaultDir, d.folder, "Tasks", fileNameFor(tk.Title, tk.ID)), renderTask(tk))
			if err != nil {
				return err
			}
			count(&written, &skipped, w)
		}
	}
	if err := prune(vaultDir, subtrees, keep); err != nil {
		return err
	}
	e.Logger.Info("obsidian export complete", "projects", len(data), "written", written, "unchanged", skipped)
	return nil
}

func count(written, skipped *int, wrote bool) {
	if wrote {
		*written++
	} else {
		*skipped++
	}
}

// folderName maps a project to its vault folder. _global gets "Global";
// otherwise the sanitized project name (fallback: ID).
func folderName(p memory.Project) string {
	if p.ID == globalProjectID {
		return "Global"
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	var b []rune
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == 0:
			b = append(b, '-')
		default:
			b = append(b, r)
		}
	}
	return string(b)
}
```

- [ ] **Step 4: Run: `go test ./internal/obsidian/ -v` — fix the memory-count assertion to the real value (see heads-up), then expect PASS**
- [ ] **Step 5: Run the full suite: `go test ./... && go vet ./...` — expect PASS**
- [ ] **Step 6: Commit: `git add internal/obsidian/ && git commit -m "feat(obsidian): full export walk with wikilinks and pruning"`**

---

### Task 6: CLI wiring — `ghost obsidian export`

**Files:** Modify: `cmd/ghost/main.go` (switch + new `runObsidian` + `printUsage`)

- [ ] **Step 1: Add dispatch** — in the `main()` switch (after `case "upgrade":`):

```go
case "obsidian":
	runObsidian()
	return
```

- [ ] **Step 2: Implement `runObsidian`** (same file, mirrors `runReflect` style; read-only DB like hook.go:111):

```go
// runObsidian implements `ghost obsidian export|sync` — a one-way mirror of
// the store into an Obsidian-readable Markdown vault.
func runObsidian() {
	if len(os.Args) < 3 || (os.Args[2] != "export" && os.Args[2] != "sync") {
		fmt.Fprintln(os.Stderr, `Usage: ghost obsidian <export|sync> [flags]

Flags:
  --out string       Vault directory (default ~/Documents/GhostVault or obsidian.vault_dir)
  --project string   Mirror a single project (plus Global)
  --interval string  sync only: poll cadence (default 30s or obsidian.interval)`)
		os.Exit(1)
	}
	mode := os.Args[2]
	var out, project, interval string
	for i := 3; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--out" && i+1 < len(os.Args):
			out = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--out="):
			out = strings.TrimPrefix(os.Args[i], "--out=")
		case os.Args[i] == "--project" && i+1 < len(os.Args):
			project = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--project="):
			project = strings.TrimPrefix(os.Args[i], "--project=")
		case os.Args[i] == "--interval" && i+1 < len(os.Args):
			interval = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--interval="):
			interval = strings.TrimPrefix(os.Args[i], "--interval=")
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if out == "" {
		out = cfg.Obsidian.VaultDir
	}
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resolve home dir: %v\n", err)
			os.Exit(1)
		}
		out = filepath.Join(home, "Documents", "GhostVault")
	}
	if interval == "" {
		interval = cfg.Obsidian.Interval
	}

	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	// Read-only: safe alongside a live MCP server (same pattern as the session hook).
	db, err := sql.Open("sqlite", dbPath+"?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ex := &obsidian.Exporter{Store: store, Logger: logger}
	ctx := context.Background()
	if mode == "export" {
		if err := ex.Export(ctx, out, project); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Mirrored to %s\n", out)
		return
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad --interval %q: %v\n", interval, err)
		os.Exit(1)
	}
	fmt.Printf("Syncing to %s every %s (Ctrl-C to stop)\n", out, d)
	if err := obsidian.Sync(ctx, ex, db, out, project, d); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
```

Imports to add in main.go: `"database/sql"`, `"time"`, `"github.com/wcatz/ghost/internal/obsidian"` (check `context`, `log/slog`, `path/filepath`, `strings` — most already imported).

- [ ] **Step 3: Add to `printUsage()`** (find the existing usage block and append in style):

```text
  obsidian export      Mirror memories to an Obsidian vault (one-way)
  obsidian sync        Keep the vault mirror fresh (polls for DB changes)
```

- [ ] **Step 4: `Sync` doesn't exist yet — stub it in `internal/obsidian/sync.go` so the build compiles** (full implementation is Task 7):

```go
package obsidian

import (
	"context"
	"database/sql"
	"time"
)

// Sync re-exports whenever the database changes, polling PRAGMA data_version.
func Sync(ctx context.Context, ex *Exporter, db *sql.DB, vaultDir, projectFilter string, interval time.Duration) error {
	return ex.Export(ctx, vaultDir, projectFilter) // poll loop lands in the next commit
}
```

- [ ] **Step 5: Build + smoke test: `go build ./... && go vet ./...` then `go run ./cmd/ghost obsidian export --out /tmp/ghost-vault-smoke && ls /tmp/ghost-vault-smoke`** — expect real memories mirrored from the live local DB (read-only, harmless), folders per project.
- [ ] **Step 6: Commit: `git add cmd/ghost/ internal/obsidian/ && git commit -m "feat(obsidian): ghost obsidian export CLI"`**

---

### Task 7: Sync poll loop + final verification

**Files:** Modify: `internal/obsidian/sync.go`; Create: `internal/obsidian/sync_test.go`

- [ ] **Step 1: Failing test** — `data_version` only moves for *other* connections, so the test writes via a second connection to a file-backed DB:

```go
package obsidian

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSyncDetectsChange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	writeDB, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	writeStore := memory.NewStore(writeDB, logger)
	defer writeStore.Close()
	ctx := context.Background()
	writeStore.EnsureProject(ctx, "p1", "/tmp/p1", "p1")
	writeStore.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "first memory", Importance: 0.7, Source: "test"})

	readDB, err := memory.OpenDB(dbPath) // second connection: sees writeDB's commits as data_version bumps
	if err != nil {
		t.Fatal(err)
	}
	readStore := memory.NewStore(readDB, logger)
	defer readStore.Close()

	vault := filepath.Join(dir, "vault")
	ex := &Exporter{Store: readStore, Logger: logger}

	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Sync(syncCtx, ex, readDB, vault, "", 50*time.Millisecond) }()

	// Initial export happens immediately.
	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 1
	})
	// A write from the other connection is picked up within a few ticks.
	writeStore.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "second memory", Importance: 0.7, Source: "test"})
	waitFor(t, func() bool {
		m, _ := filepath.Glob(filepath.Join(vault, "p1", "Memories", "*.md"))
		return len(m) == 2
	})
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("sync returned: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
```

- [ ] **Step 2: Run: `go test ./internal/obsidian/ -run TestSyncDetectsChange -v` — expect FAIL (stub exports once, never re-exports)**

- [ ] **Step 3: Implement the real loop**

```go
package obsidian

import (
	"context"
	"database/sql"
	"time"
)

// Sync mirrors once immediately, then re-exports whenever PRAGMA data_version
// reports a commit from another connection. Runs until ctx is cancelled.
func Sync(ctx context.Context, ex *Exporter, db *sql.DB, vaultDir, projectFilter string, interval time.Duration) error {
	if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
		return err
	}
	last, err := dataVersion(ctx, db)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			v, err := dataVersion(ctx, db)
			if err != nil {
				ex.Logger.Warn("obsidian sync: data_version poll failed", "error", err)
				continue
			}
			if v == last {
				continue
			}
			last = v
			if err := ex.Export(ctx, vaultDir, projectFilter); err != nil {
				ex.Logger.Warn("obsidian sync: export failed, will retry next change", "error", err)
			}
		}
	}
}

func dataVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
```

**Caveat to verify while running the test:** `data_version` is per-connection, and `database/sql` pools connections. The store sets `SetMaxOpenConns(1)` inside `OpenDB` (check schema.go:243-260 — if the pool limit is set there, the PRAGMA reads consistently from one connection; if not, add `db.SetMaxOpenConns(1)` for the read connection in `runObsidian` and the test). The test will catch it either way — if it flakes, this is why.

- [ ] **Step 4: Run: `go test ./internal/obsidian/ -race -count=1 -v` — expect PASS (all package tests, race-clean)**
- [ ] **Step 5: Full verification: `go test -race -count=1 ./... && go vet ./...` — expect PASS everywhere**
- [ ] **Step 6: Commit: `git add internal/obsidian/ && git commit -m "feat(obsidian): sync loop via data_version polling"`**
- [ ] **Step 7: README — add a short "Obsidian mirror" subsection under How it works** (3-4 sentences + the two commands; follow the README's existing tone; mention one-way + prune-guards + `.ghost-vault` marker), then `git add README.md && git commit -m "docs: obsidian vault mirror section"`
- [ ] **Step 8: Push + PR: `git push -u origin feat/obsidian-mirror && gh pr create --title "feat: obsidian vault mirror (ghost obsidian export|sync)" --body "<summary + spec/plan links + test evidence>"`** — CI must pass; merge per repo convention (squash), delete branch.
