# Delete Project Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `Store.DeleteProject` method, backing both a `ghost project delete <name>` CLI subcommand and a `ghost_project_delete` MCP tool, that permanently removes a project and everything under it (memories, tags, embeddings, links, tasks, decisions, plus the two tables that don't cascade: token_usage, audit_log) — dry-run by default, explicit apply required, `_global` always refused.

**Architecture:** One `Store.DeleteProject(ctx, input string, apply bool) (DeleteProjectSummary, error)` method is the single source of truth for both surfaces, following the same dry-run/apply convention `resolve.Run`/`supersede.Run` already use. It resolves the project via the existing `ResolveProject` (id/name/path/basename), refuses `_global` unconditionally, computes row counts across every affected table (the dry-run view), and — only when `apply` is true — deletes inside one transaction: two explicit `DELETE`s for the non-cascading tables, then one `DELETE FROM projects` that cascades to everything else via the `ON DELETE CASCADE` foreign keys already in `schema.go`.

**Tech Stack:** Go 1.26+, SQLite (modernc.org/sqlite, pure Go), `go test`/`go vet`, modelcontextprotocol/go-sdk (MCP tool registration).

**Spec:** `docs/superpowers/specs/2026-08-17-delete-project-design.md`

---

## File Structure

| File | Change |
|---|---|
| `internal/memory/store.go` | Add `DeleteProjectSummary` type + `DeleteProject` method |
| `internal/memory/store_test.go` | Add `TestDeleteProject_*` tests |
| `internal/provider/provider.go` | Add `DeleteProject` to `MemoryStore` interface |
| `cmd/ghost/main.go` | Add `ghost project delete` subcommand + printUsage entry |
| `cmd/ghost/main_test.go` | Add `TestConfirmProjectDeleteName_*` tests |
| `internal/mcpserver/mcpserver.go` | Add `ghost_project_delete` MCP tool |
| `internal/mcpserver/mcpserver_test.go` | Add `TestGhostProjectDelete_*` tests |
| `README.md` | Tool count 19→20, add `ghost_project_delete` to the tool table |
| `CLAUDE.md` | Tool count 19→20 |
| `docs/architecture.md` | Tool count (already-stale) 18→20 in both places it appears |

---

### Task 1: `Store.DeleteProject` — resolution guards, dry-run counts, apply

**Files:**
- Modify: `internal/memory/store.go` (add after `MergeProject`/`mergeProjectLocked`, i.e. right before the `ResolveProject` doc comment block, so all project-management methods stay grouped)
- Test: `internal/memory/store_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/memory/store_test.go`, right after `TestMergeProject` and its helpers (or at the end of the file if easier to locate — anywhere in the file works, `package memory` sees all of it):

```go
// seedFullProject creates one memory-link pair, one task, one decision (which
// also creates its own linked memory row, source='decision_log'), one
// token_usage row, and one audit_log row for projectID — one of everything
// DeleteProject must account for. Returns the two linked memory IDs.
func seedFullProject(t *testing.T, s *Store, ctx context.Context, projectID string) (mem1, mem2 string) {
	t.Helper()

	var err error
	mem1, err = s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory one", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem1: %v", err)
	}
	mem2, err = s.Create(ctx, projectID, Memory{
		Category: "fact", Content: "seed memory two", Source: "manual", Importance: 0.5, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("seed mem2: %v", err)
	}
	if err := s.CreateLink(ctx, mem1, mem2, "related", 0.8, "auto"); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := s.CreateTask(ctx, projectID, "seed task", "desc", 1); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if _, _, err := s.RecordDecision(ctx, projectID, "seed decision", "did the thing", "because", nil, nil); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	if err := s.RecordUsage(ctx, projectID, "claude-opus-4-6", TokenUsage{InputTokens: 10, OutputTokens: 5}); err != nil {
		t.Fatalf("seed token_usage: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (action, project_id) VALUES ('test-action', ?)`, projectID,
	); err != nil {
		t.Fatalf("seed audit_log: %v", err)
	}
	return mem1, mem2
}

// countRows returns the number of rows in table matching a project_id column,
// queried directly so the assertion doesn't depend on DeleteProject's own
// counting logic being correct.
func countRows(t *testing.T, s *Store, table, projectID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestDeleteProject_NotFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, err := s.DeleteProject(ctx, "no-such-project", false)
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected %q in error, got: %v", "not found", err)
	}
}

func TestDeleteProject_RefusesGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	for _, apply := range []bool{false, true} {
		_, err := s.DeleteProject(ctx, "_global", apply)
		if err == nil {
			t.Fatalf("expected error deleting _global (apply=%v)", apply)
		}
	}

	// The seeded global preference must have survived both attempts.
	mems, err := s.GetAll(ctx, "_global", 100)
	if err != nil {
		t.Fatalf("GetAll _global: %v", err)
	}
	if len(mems) == 0 {
		t.Error("expected _global memories to survive refused delete attempts")
	}
}

func TestDeleteProject_DryRunCountsAndWritesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedFullProject(t, s, ctx, testProject)

	summary, err := s.DeleteProject(ctx, testProject, false)
	if err != nil {
		t.Fatalf("DeleteProject dry-run: %v", err)
	}

	if summary.ProjectID != testProject {
		t.Errorf("ProjectID = %q, want %q", summary.ProjectID, testProject)
	}
	// 3 memories: the 2 seeded directly + 1 from RecordDecision's decision_log row.
	if summary.Memories != 3 {
		t.Errorf("Memories = %d, want 3", summary.Memories)
	}
	if summary.MemoryLinks != 1 {
		t.Errorf("MemoryLinks = %d, want 1", summary.MemoryLinks)
	}
	if summary.Tasks != 1 {
		t.Errorf("Tasks = %d, want 1", summary.Tasks)
	}
	if summary.Decisions != 1 {
		t.Errorf("Decisions = %d, want 1", summary.Decisions)
	}
	if summary.TokenUsage != 1 {
		t.Errorf("TokenUsage = %d, want 1", summary.TokenUsage)
	}
	if summary.AuditLog != 1 {
		t.Errorf("AuditLog = %d, want 1", summary.AuditLog)
	}

	// Dry-run must write nothing: every row must still be there.
	if n := countRows(t, s, "memories", testProject); n != 3 {
		t.Errorf("memories after dry-run = %d, want 3 (unchanged)", n)
	}
	if n := countRows(t, s, "tasks", testProject); n != 1 {
		t.Errorf("tasks after dry-run = %d, want 1 (unchanged)", n)
	}
	if n := countRows(t, s, "token_usage", testProject); n != 1 {
		t.Errorf("token_usage after dry-run = %d, want 1 (unchanged)", n)
	}
	if _, _, err := s.ResolveProject(ctx, testProject); err != nil {
		t.Fatalf("ResolveProject after dry-run: %v", err)
	}
}

func TestDeleteProject_ApplyRemovesEverythingIncludingOrphanTables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedFullProject(t, s, ctx, testProject)

	summary, err := s.DeleteProject(ctx, testProject, true)
	if err != nil {
		t.Fatalf("DeleteProject apply: %v", err)
	}
	if summary.Memories != 3 || summary.MemoryLinks != 1 || summary.Tasks != 1 ||
		summary.Decisions != 1 || summary.TokenUsage != 1 || summary.AuditLog != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	for _, table := range []string{"memories", "tasks", "decisions", "token_usage", "audit_log"} {
		if n := countRows(t, s, table, testProject); n != 0 {
			t.Errorf("%s after apply = %d, want 0", table, n)
		}
	}
	var linkCount int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM memory_links
		WHERE source_id NOT IN (SELECT id FROM memories) OR target_id NOT IN (SELECT id FROM memories)
	`).Scan(&linkCount); err != nil {
		t.Fatalf("check dangling links: %v", err)
	}
	if linkCount != 0 {
		t.Errorf("expected no dangling memory_links pointing at deleted memories, got %d", linkCount)
	}

	// The project row itself is gone.
	id, _, err := s.ResolveProject(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveProject after apply: %v", err)
	}
	if id != "" {
		t.Errorf("expected project to be gone, ResolveProject still returns id %q", id)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/... -run TestDeleteProject -v`
Expected: FAIL — `s.DeleteProject` and `s.CreateLink`'s companion type don't compile yet (`undefined: DeleteProjectSummary` / `s.DeleteProject undefined`).

- [ ] **Step 3: Implement `DeleteProjectSummary` and `DeleteProject`**

In `internal/memory/store.go`, add immediately after `mergeProjectLocked`'s closing brace (i.e. right before the `// ResolveProject resolves an identifier...` doc comment):

```go
// DeleteProjectSummary reports what DeleteProject found (dry-run) or removed
// (apply) for one project, across every table that references it.
type DeleteProjectSummary struct {
	ProjectID   string
	ProjectName string
	Memories    int
	MemoryLinks int
	Tasks       int
	Decisions   int
	TokenUsage  int
	AuditLog    int
}

// DeleteProject permanently removes a project and everything under it.
// memories (and their tags, embeddings, and links), tasks, decisions,
// ghost_state, and memory_snapshots all cascade from the projects row via
// ON DELETE CASCADE (see schema.go). token_usage and audit_log carry a
// project_id column but no foreign key, so they're deleted explicitly in the
// same transaction.
//
// input is resolved exactly like every other command resolves a project (see
// ResolveProject): id, name, path-prefix, or basename all work.
//
// apply=false computes and returns the summary without writing anything.
// apply=true performs the same computation, then actually deletes everything
// in one transaction, returning the summary of what was removed. _global can
// never be deleted, in either mode — it's shared across every project's
// context injection.
//
// ResolveProject takes its own RLock internally, so it's called here before
// this method takes s.mu itself — taking s.mu first and then calling
// ResolveProject would deadlock against sync.RWMutex's non-reentrant lock.
func (s *Store) DeleteProject(ctx context.Context, input string, apply bool) (DeleteProjectSummary, error) {
	id, name, err := s.ResolveProject(ctx, input)
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("resolve project: %w", err)
	}
	if id == "" {
		return DeleteProjectSummary{}, fmt.Errorf("project %q not found", input)
	}
	if id == "_global" {
		return DeleteProjectSummary{}, fmt.Errorf("refusing to delete the _global project")
	}

	if apply {
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}

	summary := DeleteProjectSummary{ProjectID: id, ProjectName: name}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = ?`, id,
	).Scan(&summary.Memories); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count memories: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM memory_links
		WHERE source_id IN (SELECT id FROM memories WHERE project_id = ?)
		   OR target_id IN (SELECT id FROM memories WHERE project_id = ?)
	`, id, id).Scan(&summary.MemoryLinks); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count memory_links: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE project_id = ?`, id,
	).Scan(&summary.Tasks); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count tasks: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE project_id = ?`, id,
	).Scan(&summary.Decisions); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count decisions: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM token_usage WHERE project_id = ?`, id,
	).Scan(&summary.TokenUsage); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count token_usage: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE project_id = ?`, id,
	).Scan(&summary.AuditLog); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count audit_log: %w", err)
	}

	if !apply {
		return summary, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("begin delete tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM token_usage WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete token_usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete audit_log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("commit delete: %w", err)
	}

	s.logger.Info("deleted project", "project_id", id, "project_name", name,
		"memories", summary.Memories, "tasks", summary.Tasks, "decisions", summary.Decisions)
	return summary, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/... -run TestDeleteProject -v`
Expected: `PASS` for all 4 new tests.

- [ ] **Step 5: Run the full package test suite and vet**

Run: `go vet ./internal/memory/... && go test ./internal/memory/...`
Expected: both clean, no regressions in the rest of the package.

- [ ] **Step 6: Commit**

```bash
git add internal/memory/store.go internal/memory/store_test.go
git commit -m "feat(memory): add Store.DeleteProject with dry-run/apply"
```

---

### Task 2: Isolation test — deleting one project must not touch another's rows

**Files:**
- Test: `internal/memory/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/memory/store_test.go`:

```go
func TestDeleteProject_IsolatesOtherProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const otherProject = "other-project"
	if err := s.EnsureProject(ctx, otherProject, "/tmp/other-project", "other-project"); err != nil {
		t.Fatalf("EnsureProject other: %v", err)
	}

	seedFullProject(t, s, ctx, testProject)
	seedFullProject(t, s, ctx, otherProject)

	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	expected := map[string]int{"memories": 3, "tasks": 1, "decisions": 1, "token_usage": 1, "audit_log": 1}
	for table, want := range expected {
		if n := countRows(t, s, table, otherProject); n != want {
			t.Errorf("%s for %s after deleting %s = %d, want %d (should be untouched)", table, otherProject, testProject, n, want)
		}
	}
	var links int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM memory_links l
		JOIN memories m ON m.id = l.source_id
		WHERE m.project_id = ?`, otherProject).Scan(&links); err != nil {
		t.Fatalf("count memory_links: %v", err)
	}
	if links != 1 {
		t.Errorf("memory_links for %s = %d, want 1", otherProject, links)
	}
	if id, _, err := s.ResolveProject(ctx, otherProject); err != nil || id == "" {
		t.Errorf("expected %s to still exist, ResolveProject: id=%q err=%v", otherProject, id, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/memory/... -run TestDeleteProject_IsolatesOtherProjects -v`
Expected: FAIL if Task 1's implementation has any bug scoping deletes to the wrong project (it shouldn't, but this is the check that proves it). If Task 1 is correct, this may pass immediately — that's fine, TDD's "verify it fails first" step here is really "verify the assertions are meaningful," so read the failure output carefully if it does fail before moving on.

- [ ] **Step 3: Run test to verify it passes**

Run: `go test ./internal/memory/... -run TestDeleteProject -v`
Expected: `PASS` for all 5 `TestDeleteProject_*` tests now.

- [ ] **Step 4: Commit**

```bash
git add internal/memory/store_test.go
git commit -m "test(memory): verify DeleteProject doesn't leak across projects"
```

---

### Task 3: Wire `DeleteProject` into `provider.MemoryStore`

**Files:**
- Modify: `internal/provider/provider.go:76` (the "Project management" group, right after `MergeProject`)

- [ ] **Step 1: Add the method to the interface**

In `internal/provider/provider.go`, change:

```go
	// Project management
	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	ListProjectNames(ctx context.Context) ([]string, error)
	MergeProject(ctx context.Context, oldID, newID string) error
```

to:

```go
	// Project management
	ListProjects(ctx context.Context) ([]memory.Project, error)
	EnsureProject(ctx context.Context, id, path, name string) error
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	ListProjectNames(ctx context.Context) ([]string, error)
	MergeProject(ctx context.Context, oldID, newID string) error
	DeleteProject(ctx context.Context, input string, apply bool) (memory.DeleteProjectSummary, error)
```

- [ ] **Step 2: Verify the build**

Run: `go build ./... && go vet ./...`
Expected: both clean — `*Store` already satisfies the wider interface since `DeleteProject` was added in Task 1.

- [ ] **Step 3: Commit**

```bash
git add internal/provider/provider.go
git commit -m "feat(provider): expose DeleteProject on MemoryStore"
```

---

### Task 4: CLI — `ghost project delete <name-or-id> [--apply]`

**Files:**
- Modify: `cmd/ghost/main.go` (top-level switch around line 47-77, new `runProjectDelete` function, `printUsage` around line 938-968)
- Test: `cmd/ghost/main_test.go`

- [ ] **Step 1: Write the failing test for the confirmation-match helper**

The confirmation-prompt gate's actual decision logic (does what the user typed match the project name?) is pulled into its own pure function so it's unit-testable without subprocess/stdin plumbing — `runProjectDelete` itself, like every other `run*` function in this file, calls `os.Exit` and isn't unit tested directly (matching the existing convention for `runResolve`/`runSupersede`/`resolveProjectOrExit`).

Add to `cmd/ghost/main_test.go`:

```go
func TestConfirmProjectDeleteName_MatchesExactly(t *testing.T) {
	if !confirmProjectDeleteName("my-project\n", "my-project") {
		t.Error("expected trimmed exact match to confirm")
	}
	if !confirmProjectDeleteName("  my-project  ", "my-project") {
		t.Error("expected surrounding whitespace to be trimmed before comparing")
	}
}

func TestConfirmProjectDeleteName_RejectsMismatch(t *testing.T) {
	if confirmProjectDeleteName("my-projec", "my-project") {
		t.Error("expected a partial/typo'd name not to confirm")
	}
	if confirmProjectDeleteName("", "my-project") {
		t.Error("expected an empty input not to confirm")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/ghost/... -run TestConfirmProjectDeleteName -v`
Expected: FAIL — `undefined: confirmProjectDeleteName`.

- [ ] **Step 3: Add the `project` case to the top-level switch**

In `cmd/ghost/main.go`, change:

```go
		case "resolve":
			runResolve()
			return
		case "upgrade":
```

to:

```go
		case "resolve":
			runResolve()
			return
		case "project":
			if len(os.Args) > 2 && os.Args[2] == "delete" {
				runProjectDelete()
				return
			}
			fmt.Fprintln(os.Stderr, "Usage: ghost project delete <name-or-id> [--apply]")
			os.Exit(1)
		case "upgrade":
```

- [ ] **Step 4: Add `confirmProjectDeleteName` and `runProjectDelete`**

Add both right after `runResolve` (after its closing brace, before the `firstLine` helper — both because they belong with the other `run*` command functions and because `runProjectDelete` uses `firstLine`-adjacent conventions):

```go
// confirmProjectDeleteName reports whether typed (a raw scanned line, not
// yet trimmed) matches expected exactly once surrounding whitespace is
// stripped. Pulled out of runProjectDelete as its own pure function so the
// actual confirmation decision is unit-testable without stdin/os.Exit
// plumbing.
func confirmProjectDeleteName(typed, expected string) bool {
	return strings.TrimSpace(typed) == expected
}

// runProjectDelete implements `ghost project delete <name-or-id> [--apply]`.
// Always prints the dry-run summary first. Without --apply it stops there.
// With --apply, it re-prints the summary and requires re-typing the
// project's name at a prompt before anything is actually deleted — this is
// irreversible and there is no undo, so the flag alone is not enough.
func runProjectDelete() {
	var projectName string
	apply := false
	for i := 3; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--apply":
			apply = true
		case !strings.HasPrefix(os.Args[i], "-"):
			if projectName != "" {
				fmt.Fprintln(os.Stderr, "error: expected exactly one project")
				os.Exit(1)
			}
			projectName = os.Args[i]
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", os.Args[i])
			os.Exit(1)
		}
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost project delete <name-or-id> [flags]

Flags:
  --apply   Actually delete (default is dry-run/preview)

Permanently removes a project: memories, tags, embeddings, links, tasks,
decisions, and cost/audit history. Irreversible. Refuses to delete _global.`)
		os.Exit(1)
	}

	_, logger, store := bootstrap()
	defer store.Close() //nolint:errcheck
	ctx := context.Background()
	_ = logger

	printDeleteSummary := func(summary memory.DeleteProjectSummary, verb string) {
		fmt.Printf("%s %q (%s):\n", verb, summary.ProjectName, summary.ProjectID)
		fmt.Printf("  memories:     %d\n", summary.Memories)
		fmt.Printf("  memory_links: %d\n", summary.MemoryLinks)
		fmt.Printf("  tasks:        %d\n", summary.Tasks)
		fmt.Printf("  decisions:    %d\n", summary.Decisions)
		fmt.Printf("  token_usage:  %d\n", summary.TokenUsage)
		fmt.Printf("  audit_log:    %d\n", summary.AuditLog)
	}

	summary, err := store.DeleteProject(ctx, projectName, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printDeleteSummary(summary, "Would delete")

	if !apply {
		fmt.Println("\nRe-run with --apply to actually delete.")
		return
	}

	fmt.Printf("\nType the project name (%q) to confirm deletion: ", summary.ProjectName)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	if !confirmProjectDeleteName(scanner.Text(), summary.ProjectName) {
		fmt.Fprintln(os.Stderr, "error: confirmation did not match project name — nothing deleted")
		os.Exit(1)
	}

	summary, err = store.DeleteProject(ctx, projectName, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	printDeleteSummary(summary, "Deleted")
}
```

Note: `logger` from `bootstrap()` is unused in this function (unlike `runResolve`, there's no classify provider to build here) — `_ = logger` keeps the compiler happy without changing `bootstrap`'s signature for every caller. Add `"bufio"` to the import block (it's not currently imported in `cmd/ghost/main.go`):

```go
import (
	"bufio"
	"bytes"
	"context"
```

- [ ] **Step 5: Run the confirmation-helper tests to verify they pass**

Run: `go test ./cmd/ghost/... -run TestConfirmProjectDeleteName -v`
Expected: `PASS` for both tests.

- [ ] **Step 6: Add the `printUsage` entry**

In `cmd/ghost/main.go`'s `printUsage`, change:

```go
  resolve <project> [flags]   Mark resolved evidence memories (dry-run by default, --apply to write)
  obsidian export [flags]     Mirror memories to an Obsidian vault (one-way)
```

to:

```go
  resolve <project> [flags]   Mark resolved evidence memories (dry-run by default, --apply to write)
  project delete <name> [flags]  Permanently delete a project and everything under it
                              (dry-run by default, --apply + name re-type to confirm)
  obsidian export [flags]     Mirror memories to an Obsidian vault (one-way)
```

- [ ] **Step 7: Build and smoke-test manually**

Run:
```bash
go build -o /tmp/ghost-delete-check ./cmd/ghost
mkdir -p /tmp/ghost-delete-check-data
XDG_DATA_HOME=/tmp/ghost-delete-check-data /tmp/ghost-delete-check reflect smoketest --apply >/dev/null 2>&1
XDG_DATA_HOME=/tmp/ghost-delete-check-data /tmp/ghost-delete-check project delete no-such-project
XDG_DATA_HOME=/tmp/ghost-delete-check-data /tmp/ghost-delete-check project delete global
echo "no-name" | XDG_DATA_HOME=/tmp/ghost-delete-check-data /tmp/ghost-delete-check project delete global --apply
rm -rf /tmp/ghost-delete-check /tmp/ghost-delete-check-data
```
Expected: first delete errors `project "no-such-project" not found`; second (dry-run on `_global`, resolved via its name `global`) errors `refusing to delete the _global project`; third (apply on `_global` with a deliberately wrong confirmation) also refuses before ever reaching the confirmation prompt, since `DeleteProject`'s `_global` guard fires on the initial dry-run call before the prompt is even shown.

- [ ] **Step 8: Commit**

```bash
git add cmd/ghost/main.go cmd/ghost/main_test.go
git commit -m "feat(cli): add ghost project delete subcommand"
```

---

### Task 5: MCP tool — `ghost_project_delete`

**Files:**
- Modify: `internal/mcpserver/mcpserver.go` (new tool registration, near `ghost_list_projects` around line 1257 — same "project management" neighborhood)
- Test: `internal/mcpserver/mcpserver_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/mcpserver/mcpserver_test.go`:

```go
func TestGhostProjectDelete_DryRunByDefault(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	if _, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "will this survive a dry run", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project"},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "Would delete") {
		t.Errorf("expected dry-run framing %q in response, got %q", "Would delete", text.Text)
	}

	all, err := store.GetAll(ctx, "test-project", 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected memory to survive dry-run, got %d memories", len(all))
	}
}

func TestGhostProjectDelete_ApplyRemovesProject(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)

	ctx := context.Background()
	if _, err := store.Create(ctx, "test-project", memory.Memory{
		Category: "fact", Content: "will this survive apply", Source: "manual", Importance: 0.5, Tags: []string{},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "test-project", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got error result: %+v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, "Deleted") {
		t.Errorf("expected apply framing %q in response, got %q", "Deleted", text.Text)
	}

	id, _, err := store.ResolveProject(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" {
		t.Error("expected project to be gone after apply")
	}
}

func TestGhostProjectDelete_RejectsGlobal(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	if err := store.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_project_delete",
		Arguments: map[string]any{"project": "_global", "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_project_delete: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result deleting _global, got success")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcpserver/... -run TestGhostProjectDelete -v`
Expected: FAIL — `unknown tool "ghost_project_delete"` (or equivalent "tool not found" error from the go-sdk).

- [ ] **Step 3: Register the tool**

In `internal/mcpserver/mcpserver.go`, add near `ghost_list_projects` (after its closing `})`, before the `ghost_memory_pin` registration — matching the file's existing grouping of project-management tools):

```go
	// ghost_project_delete — permanently delete a project and everything
	// under it. Dry-run by default; apply:true actually deletes. _global is
	// always refused, in both modes.
	type projectDeleteArgs struct {
		Project string `json:"project" jsonschema:"the project to delete (id, name, or path)"`
		Apply   bool   `json:"apply,omitempty" jsonschema:"actually delete (default false: dry-run preview only)"`
	}
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "ghost_project_delete",
		Title:       "Delete Project",
		Description: "Permanently deletes a project: memories, tags, embeddings, links, tasks, decisions, and cost/audit history. Irreversible — there is no undo. Dry-run by default (returns counts of what would be removed); pass apply:true to actually delete. Always refuses to delete the _global project. Only use when the user has explicitly and unambiguously asked to delete an entire project, never as a side effect of another request.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			OpenWorldHint:   boolPtr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args projectDeleteArgs) (*mcp.CallToolResult, any, error) {
		if args.Project == "" {
			return nil, nil, fmt.Errorf("project is required")
		}
		summary, err := s.store.DeleteProject(ctx, args.Project, args.Apply)
		if err != nil {
			return nil, nil, fmt.Errorf("ghost_project_delete: %w", err)
		}
		verb := "Would delete"
		if args.Apply {
			verb = "Deleted"
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%s %q (%s):\n", verb, summary.ProjectName, summary.ProjectID)
		fmt.Fprintf(&sb, "  memories:     %d\n", summary.Memories)
		fmt.Fprintf(&sb, "  memory_links: %d\n", summary.MemoryLinks)
		fmt.Fprintf(&sb, "  tasks:        %d\n", summary.Tasks)
		fmt.Fprintf(&sb, "  decisions:    %d\n", summary.Decisions)
		fmt.Fprintf(&sb, "  token_usage:  %d\n", summary.TokenUsage)
		fmt.Fprintf(&sb, "  audit_log:    %d\n", summary.AuditLog)
		if !args.Apply {
			sb.WriteString("\nRe-run with apply:true to actually delete.")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sb.String()}}}, nil, nil
	})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/mcpserver/... -run TestGhostProjectDelete -v`
Expected: `PASS` for all 3 new tests.

- [ ] **Step 5: Run the full test suite and vet**

Run: `go vet ./... && go test ./...`
Expected: everything clean, no regressions anywhere.

- [ ] **Step 6: Update tool-count references**

In `README.md`, change:

```
19 tools, 4 resources:

| Group | Tools |
|---|---|
| Memory | `ghost_memory_save` `ghost_memory_search` `ghost_search_all` `ghost_memories_list` `ghost_memory_update` `ghost_memory_delete` `ghost_memory_pin` `ghost_memory_promote` `ghost_save_global` `ghost_resolve` |
| Context | `ghost_project_context` `ghost_list_projects` `ghost_health` |
```

to:

```
20 tools, 4 resources:

| Group | Tools |
|---|---|
| Memory | `ghost_memory_save` `ghost_memory_search` `ghost_search_all` `ghost_memories_list` `ghost_memory_update` `ghost_memory_delete` `ghost_memory_pin` `ghost_memory_promote` `ghost_save_global` `ghost_resolve` |
| Context | `ghost_project_context` `ghost_list_projects` `ghost_health` `ghost_project_delete` |
```

In `CLAUDE.md`, change:

```
- `internal/mcpserver/` — MCP server: 19 tools + 4 resources + 2 prompts (`recall_project`, `record_decision`)
```

to:

```
- `internal/mcpserver/` — MCP server: 20 tools + 4 resources + 2 prompts (`recall_project`, `record_decision`)
```

In `docs/architecture.md`, change (this was already stale at 18 before this feature — fixing both references to the correct current count in the same edit):

```
    mcpserver.go           18 tools + 4 resources via go-sdk
```

to:

```
    mcpserver.go           20 tools + 4 resources via go-sdk
```

and:

```
                          ... 18 tools total
```

to:

```
                          ... 20 tools total
```

- [ ] **Step 7: Commit**

```bash
git add internal/mcpserver/mcpserver.go internal/mcpserver/mcpserver_test.go README.md CLAUDE.md docs/architecture.md
git commit -m "feat(mcpserver): add ghost_project_delete tool, update tool counts"
```

---

### Task 6: Final verification

- [ ] **Step 1: Full build, vet, test**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all clean.

- [ ] **Step 2: Manual MCP smoke test via CLI-equivalent path**

Run:
```bash
go build -o /tmp/ghost-final-check ./cmd/ghost
mkdir -p /tmp/ghost-final-check-data
XDG_DATA_HOME=/tmp/ghost-final-check-data /tmp/ghost-final-check project delete nonexistent >/dev/null 2>&1  # touch bootstrap() once so ghost.db exists with schema applied
sqlite3 /tmp/ghost-final-check-data/ghost/ghost.db <<'SQL'
INSERT INTO projects (id, path, name) VALUES ('smoketest', 'smoketest', 'smoketest');
INSERT INTO memories (project_id, category, content, source, importance, tags)
VALUES ('smoketest', 'fact', 'smoke test seed memory', 'manual', 0.5, '[]');
SQL
XDG_DATA_HOME=/tmp/ghost-final-check-data /tmp/ghost-final-check project delete smoketest
echo "smoketest" | XDG_DATA_HOME=/tmp/ghost-final-check-data /tmp/ghost-final-check project delete smoketest --apply
XDG_DATA_HOME=/tmp/ghost-final-check-data /tmp/ghost-final-check project delete smoketest
rm -rf /tmp/ghost-final-check /tmp/ghost-final-check-data
```
Note: the originally-drafted bootstrap line here, `ghost reflect smoketest --apply`, can never work — `runReflect` resolves the project via `resolveProjectOrExit`/`ResolveProject`, which only looks projects up and never creates one, and nothing in `bootstrap()` or the session-start hook creates a project named `smoketest` either (`loadSessionContext` explicitly refuses to: "no store yet — never create a phantom empty DB"). This isn't an environment gap (no API key/Ollama) — it's structural, so no tier of `reflect` could ever bootstrap a project that doesn't exist yet. The `sqlite3` seed above inserts exactly what `ghost_memory_save`'s create-on-first-save path (`EnsureProject` + `Create` in `internal/memory/store.go`) would have written for `project_id="smoketest"` (path normalized to id, one `fact` memory), without requiring a live MCP client or an LLM.

Expected: first call prints a dry-run summary with 1 memory; second call (apply, with matching typed confirmation) prints "Deleted ..." with counts; third call (post-delete) errors `project "smoketest" not found`, proving the project is actually gone.

- [ ] **Step 3: Final commit if anything is outstanding**

Run: `git status --short`
Expected: clean (everything already committed per-task). If anything is outstanding, `git add` and commit it with an appropriately scoped message before moving to review/PR.
