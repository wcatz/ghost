# Obsidian Orphan Folder Cleanup Implementation Plan

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clean up orphaned project folders in the Obsidian vault when projects are deleted from Ghost.

**Architecture:** Add a second pass to `prune` that scans the vault root for top-level directories not in the current project set, and removes any containing Ghost-managed content. The caller (`Export`) passes the full set of known project folders to distinguish orphans from active projects.

**Tech Stack:** Go, `os`, `filepath`, `path/filepath`

## Global Constraints

- Go 1.26+ (see `go.mod`)
- Pure Go SQLite (modernc.org/sqlite — no CGO)
- Tests: `go test ./internal/obsidian/...`
- Lint: `go vet ./...`

---

### Task 1: Add orphan folder cleanup to `prune` and update `Export` caller

**Files:**
- Modify: `internal/obsidian/vault.go:94` (prune signature + new logic)
- Modify: `internal/obsidian/export.go:154` (prune call site)

**Interfaces:**
- Consumes: `subtrees []string`, `keep map[string]string` (existing), `knownFolders []string` (new)
- Produces: `prune` signature becomes `prune(root string, subtrees []string, keep map[string]string, knownFolders []string) error`

- [ ] **Step 1: Update `prune` signature in `vault.go`**

Change line 94 from:

```go
func prune(root string, subtrees []string, keep map[string]string) error {
```

to:

```go
func prune(root string, subtrees []string, keep map[string]string, knownFolders []string) error {
```

- [ ] **Step 2: Add orphan folder cleanup after existing subtree walk in `vault.go`**

After the existing `for _, sub := range subtrees` loop (line 129) and before the final `return nil` (line 130), add the orphan cleanup pass:

```go
	// Orphan cleanup: remove vault top-level directories that are not in the
	// current project set but contain Ghost-managed content. This handles
	// projects deleted from the DB since the last export. Skipped when
	// knownFolders is nil (filtered export — we can't know what's orphaned).
	if len(knownFolders) > 0 {
		known := make(map[string]bool, len(knownFolders))
		for _, f := range knownFolders {
			known[f] = true
		}
		entries, err := readDir(root)
		if err != nil {
			return fmt.Errorf("read vault root for orphan scan: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == markerName {
				continue
			}
			if known[e.Name()] {
				continue
			}
			if !filepath.IsLocal(e.Name()) {
				return fmt.Errorf("refusing to prune: orphan dir %q escapes vault root", e.Name())
			}
			dir := filepath.Join(root, e.Name())
			if hasGhostContent(dir) {
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove orphan folder %s: %w", e.Name(), err)
				}
			}
		}
	}
```

- [ ] **Step 3: Add `hasGhostContent` helper in `vault.go`**

Add this function before `prune` (after `hasGhostID`):

```go
// hasGhostContent reports whether dir contains any .md file with a ghost_id
// in its frontmatter — the signature of a Ghost-managed note.
func hasGhostContent(dir string) bool {
	var found bool
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		if _, ok := hasGhostID(path); ok {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
```

- [ ] **Step 4: Update `Export` caller in `export.go`**

Change line 154 from:

```go
	if err := prune(vaultDir, subtrees, keep); err != nil {
```

to:

```go
	// Build the full set of known project folders (including truncated
	// projects) so prune can distinguish deleted projects from active ones.
	var knownFolders []string
	if projectFilter == "" {
		knownFolders = make([]string, 0, len(folders))
		for _, f := range folders {
			knownFolders = append(knownFolders, f)
		}
	}
	if err := prune(vaultDir, subtrees, keep, knownFolders); err != nil {
```

- [ ] **Step 5: Verify compilation**

Run: `go vet ./internal/obsidian/...`
Expected: no errors

- [ ] **Step 6: Run existing tests to confirm no regressions**

Run: `go test ./internal/obsidian/... -v -count=1`
Expected: all existing tests pass

- [ ] **Step 7: Commit**

```bash
git add internal/obsidian/vault.go internal/obsidian/export.go
git commit -m "obsidian: add orphan project folder cleanup to prune"
```

---

### Task 2: Add tests for orphan folder cleanup

**Files:**
- Modify: `internal/obsidian/export_test.go`

**Interfaces:**
- Consumes: `Exporter.Export(ctx, vaultDir, projectFilter)` (existing), `memory.Store` (existing)
- Produces: three new test functions

- [ ] **Step 1: Write `TestExportPrunesDeletedProjectFolder`**

Add after `TestExportReclaimsOrphanedTmpFiles`:

```go
func TestExportPrunesDeletedProjectFolder(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "doomed", "/tmp/doomed", "doomed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "doomed", memory.Memory{Category: "fact", Content: "Doomed fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	// Both project folders exist after initial export.
	for _, proj := range []string{"ghost", "doomed"} {
		notes, _ := filepath.Glob(filepath.Join(vault, proj, "Memories", "*.md"))
		if len(notes) != 1 {
			t.Fatalf("want 1 note in %s, got %d", proj, len(notes))
		}
	}

	// Delete the doomed project from the DB.
	if _, err := store.DeleteProject(ctx, "doomed", true); err != nil {
		t.Fatal(err)
	}

	// Re-export — orphan folder should be pruned.
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vault, "doomed")); !os.IsNotExist(err) {
		t.Error("deleted project's vault folder should be removed")
	}
	ghostNotes, _ := filepath.Glob(filepath.Join(vault, "ghost", "Memories", "*.md"))
	if len(ghostNotes) != 1 {
		t.Errorf("surviving project should be untouched, got %d notes", len(ghostNotes))
	}
}
```

- [ ] **Step 2: Write `TestExportSkipsOrphanCleanupWhenFiltered`**

Add after the previous test:

```go
func TestExportSkipsOrphanCleanupWhenFiltered(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "other", "/tmp/other", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "other", memory.Memory{Category: "fact", Content: "Other fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	// Export unfiltered first to create both folders.
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}

	// Now export with filter — orphan cleanup should be skipped.
	if err := ex.Export(ctx, vault, "ghost"); err != nil {
		t.Fatal(err)
	}
	// The "other" folder must survive even though it's not in the filtered set.
	notes, _ := filepath.Glob(filepath.Join(vault, "other", "Memories", "*.md"))
	if len(notes) != 1 {
		t.Errorf("filtered export must not prune other project's folder, got %d notes", len(notes))
	}
}
```

- [ ] **Step 3: Write `TestExportIgnoresUserFolders`**

Add after the previous test:

```go
func TestExportIgnoresUserFolders(t *testing.T) {
	store := seedStore(t)
	ctx := context.Background()
	if _, err := store.Create(ctx, "ghost", memory.Memory{Category: "fact", Content: "Ghost fact", Importance: 0.8, Source: "mcp"}); err != nil {
		t.Fatal(err)
	}

	vault := filepath.Join(t.TempDir(), "vault")
	ex := &Exporter{Store: store, Logger: slog.Default()}
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}

	// Create a user folder with non-Ghost .md files (no ghost_id frontmatter).
	userDir := filepath.Join(vault, "my-notes")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "jotting.md"), []byte("# My Note\nJust a note."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-export — user folder must survive.
	if err := ex.Export(ctx, vault, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userDir); os.IsNotExist(err) {
		t.Error("user-created folder should not be pruned")
	}
}
```

- [ ] **Step 4: Run the new tests**

Run: `go test ./internal/obsidian/... -v -run "TestExportPrunesDeletedProjectFolder|TestExportSkipsOrphanCleanupWhenFiltered|TestExportIgnoresUserFolders" -count=1`
Expected: all three tests pass

- [ ] **Step 5: Run full test suite**

Run: `go test ./internal/obsidian/... -v -count=1`
Expected: all tests pass (existing + new)

- [ ] **Step 6: Commit**

```bash
git add internal/obsidian/export_test.go
git commit -m "obsidian: add tests for orphan folder cleanup"
```
