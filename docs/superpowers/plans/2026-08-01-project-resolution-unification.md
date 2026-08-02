# Project Resolution Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace three divergent project-identifier-resolution implementations (`hook.go`'s `lookupProject`, `store.go`'s `ResolveProjectByName`, `mcpserver.go`'s `resolveProjectID`) with one `Store.ResolveProject` method used everywhere.

**Architecture:** Add `Store.ResolveProject(ctx, input) (id, name string, err error)` implementing a 4-step lookup chain (exact ID → exact name → path-prefix → basename-of-input). Migrate all three call sites onto it, deleting the old functions. `ghost_memory_save`'s auto-create call site gets an explicit raw-input fallback so it keeps creating brand-new projects on first save.

**Tech Stack:** Go 1.26, `database/sql` (modernc.org/sqlite), table-driven tests with `:memory:` SQLite.

---

## Task 1: Add `Store.ResolveProject` and `Store.ListProjectNames`

**Files:**
- Modify: `internal/memory/store.go` (add methods near `ResolveProjectByName`, currently at lines 295-309; add after `ListProjects`, currently lines 145-166)
- Test: `internal/memory/store_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/memory/store_test.go`, replacing `TestStoreResolveProjectByName` (lines 966-985) entirely:

```go
func TestStoreResolveProject_ByID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, name, err := s.ResolveProject(ctx, testProject)
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != testProject || name != "test" {
		t.Errorf("got id=%q name=%q, want id=%q name=%q", id, name, testProject, "test")
	}
}

func TestStoreResolveProject_ByName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, name, err := s.ResolveProject(ctx, "test")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != testProject || name != "test" {
		t.Errorf("got id=%q name=%q, want id=%q name=%q", id, name, testProject, "test")
	}
}

func TestStoreResolveProject_PathPrefix(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "ghostid", "/home/wayne/git/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/home/wayne/git/ghost/internal/memory")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "ghostid" || name != "ghost" {
		t.Errorf("got id=%q name=%q, want id=%q name=%q", id, name, "ghostid", "ghost")
	}
}

func TestStoreResolveProject_LongestPathWins(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "parent", "/home/wayne/git", "parent"); err != nil {
		t.Fatalf("EnsureProject parent: %v", err)
	}
	if err := s.EnsureProject(ctx, "child", "/home/wayne/git/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject child: %v", err)
	}

	id, _, err := s.ResolveProject(ctx, "/home/wayne/git/ghost/cmd")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "child" {
		t.Errorf("longest path should win, got %q, want %q", id, "child")
	}
}

func TestStoreResolveProject_NoPrefixFalseMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "ghostid", "/home/wayne/git/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/home/wayne/git/ghost-extra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("ghost-extra should NOT prefix-match ghost, got id=%q name=%q", id, name)
	}
}

func TestStoreResolveProject_BasenameFallback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Path shorter than the LENGTH(path) > 10 guard, so prefix matching can't fire —
	// isolates the basename-of-input fallback (case 4).
	if err := s.EnsureProject(ctx, "ghostid", "/x/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, name, err := s.ResolveProject(ctx, "/some/unrelated/path/ghost")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "ghostid" || name != "ghost" {
		t.Errorf("basename fallback: got id=%q name=%q, want id=%q name=%q", id, name, "ghostid", "ghost")
	}
}

func TestStoreResolveProject_NoMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, name, err := s.ResolveProject(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("expected empty on no match, got id=%q name=%q", id, name)
	}
}

func TestStoreResolveProject_ClosedDB(t *testing.T) {
	s := testStore(t)
	s.Close() //nolint:errcheck
	ctx := context.Background()

	_, _, err := s.ResolveProject(ctx, "test")
	if err == nil {
		t.Fatal("expected error on closed DB, got nil")
	}
}

func TestStoreListProjectNames(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "ghostid", "/home/wayne/git/ghost", "ghost"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	names, err := s.ListProjectNames(ctx)
	if err != nil {
		t.Fatalf("ListProjectNames: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d: %v", len(names), names)
	}
	// ListProjects orders by name ASC — "ghost" before "test".
	if names[0] != "ghost" || names[1] != "test" {
		t.Errorf("got %v, want [ghost test]", names)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/memory/... -run TestStoreResolveProject -v`
Expected: FAIL — `s.ResolveProject undefined` (compile error), same for `TestStoreListProjectNames`.

- [ ] **Step 3: Implement `ResolveProject` and `ListProjectNames`**

Add to `internal/memory/store.go` immediately after `ResolveProjectByName` (delete `ResolveProjectByName` itself — see Task 4 for the interface-side removal):

```go
// ResolveProject resolves an identifier — a project name, hash ID, or
// filesystem path — to that project's (id, name). Returns ("", "", nil)
// on no match; a non-nil error only indicates a real DB failure.
//
// Lookup order, first hit wins:
//  1. exact id = input
//  2. exact name = input
//  3. if input contains '/': input = path OR input LIKE path || '/%'
//     (ordered by LENGTH(path) DESC — longest/most-specific match wins;
//     LENGTH(path) > 10 guards against a short project path matching too
//     broadly, matching the hook's original lookupProject behavior)
//  4. name = basename(input)
func (s *Store) ResolveProject(ctx context.Context, input string) (id, name string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE id = ? LIMIT 1`, input).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by id: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE name = ? LIMIT 1`, input).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by name: %w", err)
	}

	if strings.Contains(input, "/") {
		err = s.db.QueryRowContext(ctx, `
			SELECT id, name FROM projects
			WHERE (path = ? OR ? LIKE path || '/%') AND LENGTH(path) > 10
			ORDER BY LENGTH(path) DESC LIMIT 1
		`, input, input).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("resolve project by path: %w", err)
		}
	}

	base := filepath.Base(input)
	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE name = ? LIMIT 1`, base).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by basename: %w", err)
	}

	return "", "", nil
}

// ListProjectNames returns all known project names, ordered the same way as
// ListProjects (name ASC) — used to format an actionable CLI error listing
// known projects on a resolution miss.
func (s *Store) ListProjectNames(ctx context.Context) ([]string, error) {
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names, nil
}
```

`context`, `database/sql`, `fmt`, `path/filepath`, and `strings` are already imported in `store.go` (lines 3-13) — no import changes needed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/memory/... -run 'TestStoreResolveProject|TestStoreListProjectNames' -v`
Expected: PASS (9 tests)

- [ ] **Step 5: Commit**

```bash
git add internal/memory/store.go internal/memory/store_test.go
git commit -m "feat(memory): add Store.ResolveProject and ListProjectNames"
```

---

## Task 2: Migrate CLI (`cmd/ghost/main.go`) off `ResolveProjectByName`

**Files:**
- Modify: `cmd/ghost/main.go` (reflect subcommand ~line 188, resolve subcommand ~line 526 — verify exact line numbers first, since this file was already touched by unrelated commits since the spec was written)

- [ ] **Step 1: Locate both call sites**

Run: `grep -n "ResolveProjectByName" cmd/ghost/main.go`

Both currently read:

```go
	projectID, err := store.ResolveProjectByName(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		os.Exit(1)
	}
```

- [ ] **Step 2: Replace both occurrences**

Replace each of the two blocks above with:

```go
	projectID, _, err := store.ResolveProject(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		names, listErr := store.ListProjectNames(ctx)
		if listErr != nil || len(names) == 0 {
			fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		} else {
			fmt.Fprintf(os.Stderr, "error: project %q not found. Known projects: %s\n", projectName, strings.Join(names, ", "))
		}
		os.Exit(1)
	}
```

Check `cmd/ghost/main.go`'s imports for `strings` (`grep -n '"strings"' cmd/ghost/main.go`) — add it to the import block if missing.

- [ ] **Step 3: Build and manually verify**

Run: `go build -o /tmp/ghost-plan-test ./cmd/ghost && /tmp/ghost-plan-test reflect nonexistent-project-xyz`
Expected output: `error: project "nonexistent-project-xyz" not found. Known projects: <comma-separated list>` (or the bare message if the store has zero projects).

- [ ] **Step 4: Commit**

```bash
git add cmd/ghost/main.go
git commit -m "feat(cli): list known projects on reflect/resolve name-not-found"
```

---

## Task 3: Migrate MCP tool layer (`internal/mcpserver/mcpserver.go`) off `resolveProjectID`

**Files:**
- Modify: `internal/mcpserver/mcpserver.go` (delete `resolveProjectID` at lines 251-299; update 16 call sites at lines 310, 380, 431, 521, 585, 655, 698, 893, 930, 980, 1071, 1307, 1357, 1416, 1457, 1509 — re-`grep` first, since Task 2 doesn't touch this file but line numbers may have shifted from other concurrent work)
- Test: `internal/mcpserver/mcpserver_test.go` (delete `TestResolveProjectID_*`, lines 34-89)

`resolveProjectID` returns a single `string`, always non-empty (raw input echoed back on miss). `ResolveProject` returns `(id, name string, err error)`, with `("", "", nil)` on miss. Two call-site shapes:

**Shape A — every site except `ghost_memory_save`:** these only ever used the returned ID, and never depended on a miss falling back to the raw input (the surrounding code either treats `projectID == ""` as "no such project, return empty/error" already, or the DB query below simply returns zero rows for an ID that doesn't exist — same net effect as passing through the raw unresolved input). Replace `s.resolveProjectID(ctx, ctx, X)` (single-value) with the ID half of `store.ResolveProject`:

```go
resolvedProjectID, _, err := s.store.ResolveProject(ctx, X)
if err != nil {
	return nil, nil, fmt.Errorf("resolve project: %w", err)
}
```

Apply this pattern at every call site EXCEPT the one inside the `saveArgs` handler (Shape B below). This includes both `projectID :=` / `resolvedProjectID :=` forms and `args.ProjectID = s.resolveProjectID(...)` forms — for the latter, assign to a new local (e.g. `resolved`) and then set `args.ProjectID = resolved`, since `args.ProjectID` can no longer double as both input and output in one statement without shadowing the error.

**Shape B — the `saveArgs` handler only (currently line 521, inside the function starting at line 506):** `ghost_memory_save` is the sole auto-create path — `EnsureProject` right below it must still receive a real, non-empty project identifier to create when the project is brand new. Per the spec: "`EnsureProject`'s auto-create call in `ghost_memory_save` is unchanged — it still runs after resolution, using whatever ID `ResolveProject` returned (or the raw input, if resolution missed, exactly as today)." Replace:

```go
		args.ProjectID = s.resolveProjectID(ctx, args.ProjectID)
```

with:

```go
		if resolved, _, err := s.store.ResolveProject(ctx, args.ProjectID); err != nil {
			return nil, nil, fmt.Errorf("resolve project: %w", err)
		} else if resolved != "" {
			args.ProjectID = resolved
		}
		// else: no existing project matched — keep args.ProjectID as-is so
		// EnsureProject below creates a new project using the raw input,
		// preserving today's auto-create-on-first-save behavior.
```

- [ ] **Step 1: Update the failing tests first**

In `internal/mcpserver/mcpserver_test.go`, replace `TestResolveProjectID_ByName`, `TestResolveProjectID_ByID`, `TestResolveProjectID_Unknown`, `TestResolveProjectID_NameTakesPrecedence` (lines 34-89) with:

```go
func TestResolveProject_ByName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, _, err := store.ResolveProject(ctx, "test-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" {
		t.Errorf("ResolveProject(name) = %q, want %q", id, "abc123")
	}
}

func TestResolveProject_ByID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, _, err := store.ResolveProject(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" {
		t.Errorf("ResolveProject(id) = %q, want %q", id, "abc123")
	}
}

func TestResolveProject_Unknown(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, name, err := store.ResolveProject(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("ResolveProject(unknown) = %q/%q, want empty/empty", id, name)
	}
}

func TestResolveProject_IDTakesPrecedenceOverName(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create a second project where the name matches the first project's ID.
	if err := store.EnsureProject(ctx, "def456", "/tmp/second", "abc123"); err != nil {
		t.Fatalf("EnsureProject second: %v", err)
	}

	// "abc123" is both project 1's ID and project 2's name — ID match wins (case 1 before case 2).
	id, _, err := store.ResolveProject(ctx, "abc123")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" {
		t.Errorf("ResolveProject should prefer ID match, got %q, want %q", id, "abc123")
	}
}
```

Note the precedence flip from the old `resolveProjectID` (which tried name first, then ID) to `ResolveProject` (ID first, then name) — this matches the spec's stated lookup order ("1. exact id = input, 2. exact name = input"). `TestResolveProjectID_NameTakesPrecedence` asserted the *opposite* precedence and must be replaced, not kept — this is an intentional, spec-mandated behavior change for the one pathological case where a project's name collides with a different project's ID, not a regression in ordinary use.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/mcpserver/... -run TestResolveProject -v`
Expected: FAIL (compile error — `s.store` field access from test needs `store.ResolveProject` which doesn't exist yet if Task 1 wasn't merged first; if Task 1 is already committed, this instead fails because `resolveProjectID`/call sites in `mcpserver.go` haven't been touched yet, which is fine — the new tests call `store.ResolveProject` directly and only need Task 1's method to exist).

- [ ] **Step 3: Delete `resolveProjectID` and migrate all 16 call sites**

Delete lines 251-299 (the full `resolveProjectID` function). Then, for each call site found via `grep -n "resolveProjectID" internal/mcpserver/mcpserver.go`, apply Shape A or Shape B as described above. Re-run the grep after editing to confirm zero remaining references:

Run: `grep -n "resolveProjectID" internal/mcpserver/mcpserver.go`
Expected: no output.

- [ ] **Step 4: Run the full package test suite**

Run: `go test ./internal/mcpserver/... -v`
Expected: PASS, including the new `TestResolveProject_*` tests and all existing `ghost_memory_save`/`ghost_memory_search`/etc. handler tests (auto-create behavior must still pass for any existing test that saves to a brand-new project name).

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/mcpserver.go internal/mcpserver/mcpserver_test.go
git commit -m "feat(mcp): migrate resolveProjectID call sites onto Store.ResolveProject"
```

---

## Task 4: Migrate session-start hook (`internal/mcpinit/hook.go`) off `lookupProject`

**Files:**
- Modify: `internal/mcpinit/hook.go` (delete `lookupProject`, lines 341-358; update its one call site in `loadSessionContext`, line 238)
- Modify: `internal/mcpinit/stophook.go` (update its one call site, line 153)
- Test: `internal/mcpinit/hook_test.go` (lines 34-98)

`lookupProject(db *sql.DB, cwd string) (id, name string)` operates on a raw `*sql.DB`. `Store.ResolveProject` is a method on `*memory.Store`. Both `loadSessionContext` and the stop hook already have an open `*sql.DB` (via `roDSN`) — wrap it with `memory.NewStore(db, logger)` to get a `*memory.Store` without changing the connection or its read-only mode.

- [ ] **Step 1: Check `memory.NewStore`'s signature**

Run: `grep -n "^func NewStore" internal/memory/store.go`
Expected: `func NewStore(db *sql.DB, logger *slog.Logger) *Store` (confirm exact signature before writing the wrapper — adjust the snippet below if the logger param differs).

- [ ] **Step 2: Update the failing tests first**

Replace `internal/mcpinit/hook_test.go` lines 34-98 (`TestLookupProject_ExactMatch` through `TestLookupProject_NoMatch`) with:

```go
func TestResolveProject_ExactMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	id, name, err := s.ResolveProject(context.Background(), "/home/wayne/git/ghost")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("exact match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestResolveProject_SubdirMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	id, name, err := s.ResolveProject(context.Background(), "/home/wayne/git/ghost/internal/mcpinit")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("subdir match: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

// TestResolveProject_NoPrefixFalseMatch is the regression test for the bug:
// a CWD of /home/wayne/git/ghost-extra must NOT match a project at /home/wayne/git/ghost.
func TestResolveProject_NoPrefixFalseMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	id, name, err := s.ResolveProject(context.Background(), "/home/wayne/git/ghost-extra")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("prefix false match: /home/wayne/git/ghost-extra should NOT match /home/wayne/git/ghost, got id=%q name=%q", id, name)
	}
}

func TestResolveProject_LongestPathWins(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "parent", "/home/wayne/git", "parent")
	insertProject(t, db, "child", "/home/wayne/git/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	id, _, err := s.ResolveProject(context.Background(), "/home/wayne/git/ghost/cmd")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "child" {
		t.Errorf("longest path should win, got %q, want %q", id, "child")
	}
}

func TestResolveProject_NameFallback(t *testing.T) {
	db := openTestDB(t)
	// Use a short path so path prefix matching won't trigger (LENGTH(path) > 10 guard).
	// The basename fallback fires when cwd basename matches a project name.
	insertProject(t, db, "abc123", "/x/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	// cwd basename is "ghost" — should match by name even when path doesn't match.
	id, name, err := s.ResolveProject(context.Background(), filepath.Join("/some/unrelated/path", "ghost"))
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "abc123" || name != "ghost" {
		t.Errorf("name fallback: got id=%q name=%q, want abc123/ghost", id, name)
	}
}

func TestResolveProject_NoMatch(t *testing.T) {
	db := openTestDB(t)
	insertProject(t, db, "abc123", "/home/wayne/git/ghost", "ghost")
	s := memory.NewStore(db, testLogger())

	id, name, err := s.ResolveProject(context.Background(), "/home/user/other-project")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if id != "" || name != "" {
		t.Errorf("no match: expected empty, got id=%q name=%q", id, name)
	}
}
```

Add a small test helper right after `openTestDB` (same file) if the package doesn't already have one:

Run: `grep -n "func testLogger" internal/mcpinit/*.go`

If nothing is found, add:

```go
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
```

Add `"io"` and `"log/slog"` to `hook_test.go`'s import block if not already present (check with `grep -n '"io"\|"log/slog"' internal/mcpinit/hook_test.go` first — `context` also needs adding since the new tests use `context.Background()`).

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/mcpinit/... -run TestResolveProject -v`
Expected: FAIL — compile error, `s.ResolveProject` exists (from Task 1) but nothing in this package calls it yet; the failure here is really just confirming the test file compiles once `lookupProject`'s old tests are gone. If Task 1 already landed, this should actually PASS immediately since it only exercises `memory.Store` directly — that's fine, it still proves the new tests are wired correctly before touching `hook.go` itself.

- [ ] **Step 4: Replace `lookupProject`'s call site in `loadSessionContext`**

In `internal/mcpinit/hook.go`, replace line 238:

```go
	projectID, project = lookupProject(db, cwd)
```

with:

```go
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	projectID, project, err = store.ResolveProject(context.Background(), cwd)
	if err != nil {
		return
	}
```

`loadSessionContext`'s named return signature (line 222) already declares `err` implicitly via its other statements using `:=` locally — check whether `err` is already a named return; if not, this needs a local `var err error` instead of reusing an outer one. Run `grep -n "^func loadSessionContext" -A3 internal/mcpinit/hook.go` to confirm the exact signature before editing, since the named returns are `(projectID, project string, memories []sessionMemory, learned string, tasks [][4]string, decisions [][2]string, interactionCount int)` — there is no named `err` return, so declare it locally:

```go
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	var resolveErr error
	projectID, project, resolveErr = store.ResolveProject(context.Background(), cwd)
	if resolveErr != nil {
		return
	}
```

Delete `lookupProject` itself (lines 341-358).

Add `"context"` and `"log/slog"` to `hook.go`'s import block (check first with `grep -n '"context"\|"log/slog"' internal/mcpinit/hook.go` — `"context"` is likely already imported since `loadSessionContext` calls `memory.DemotionPenalties(context.Background(), ...)` at line 283; `"log/slog"` is new).

- [ ] **Step 5: Replace `lookupProject`'s call site in `stophook.go`**

Run: `grep -n -B5 -A2 "lookupProject" internal/mcpinit/stophook.go` to see the exact surrounding code (opens its own `*sql.DB`, same `roDSN` pattern) before editing. Apply the same `memory.NewStore` wrapper + `ResolveProject` call pattern as Step 4, adjusted to that function's existing variable names and error-handling style. Update the comment at line 127 (`// Known limitation: resolution here depends on lookupProject's path/basename ...`) to reference `ResolveProject` instead of `lookupProject`.

- [ ] **Step 6: Run the full package test suite**

Run: `go test ./internal/mcpinit/... -v`
Expected: PASS, all tests including session-start-hook end-to-end tests and stop-hook tests.

- [ ] **Step 7: Commit**

```bash
git add internal/mcpinit/hook.go internal/mcpinit/hook_test.go internal/mcpinit/stophook.go
git commit -m "feat(hook): migrate lookupProject call sites onto Store.ResolveProject"
```

---

## Task 5: Remove `ResolveProjectByName` from the `provider.MemoryStore` interface

**Files:**
- Modify: `internal/provider/provider.go` (line 72)

By this point, Task 1 already deleted the `ResolveProjectByName` method body on `*Store` (Task 1, Step 3 replaces it in place). This task removes the now-dead interface declaration and adds `ResolveProject`/`ListProjectNames` so the interface still accurately describes what `*Store` implements. No other type implements `provider.MemoryStore` (`claudeimport`, `mcpserver`, `embedding` only consume the interface — confirmed via `grep -rln "provider.MemoryStore" --include=*.go`, none define a second implementation), so this is a pure signature edit with no fakes/mocks to update.

- [ ] **Step 1: Update the interface**

In `internal/provider/provider.go`, replace line 72:

```go
	ResolveProjectByName(ctx context.Context, name string) (string, error)
```

with:

```go
	ResolveProject(ctx context.Context, input string) (id, name string, err error)
	ListProjectNames(ctx context.Context) ([]string, error)
```

- [ ] **Step 2: Build the whole module**

Run: `go build ./...`
Expected: succeeds with no errors (this is the final cross-check that no remaining caller anywhere still references the deleted methods/functions).

- [ ] **Step 3: Full test suite + vet**

Run: `go vet ./... && go test ./...`
Expected: PASS, no vet warnings.

- [ ] **Step 4: Commit**

```bash
git add internal/provider/provider.go
git commit -m "refactor(provider): replace ResolveProjectByName with ResolveProject in MemoryStore interface"
```

---

## Self-Review

**Spec coverage:**
- ✅ `Store.ResolveProject` with the exact 4-step lookup order and `("", "", nil)`-on-miss contract — Task 1.
- ✅ `ListProjectNames` — Task 1.
- ✅ CLI near-miss listing on `reflect`/`resolve` — Task 2.
- ✅ MCP tool layer migration, all 16 call sites, `resolveProjectID` deleted — Task 3.
- ✅ `ghost_memory_save`'s `EnsureProject` auto-create preserved via explicit raw-input fallback — Task 3, Shape B.
- ✅ Hook migration, `lookupProject` deleted, both `hook.go` and `stophook.go` call sites — Task 4.
- ✅ Table-driven tests: exact ID, exact name, path-prefix + longest-wins, no-prefix-false-match, basename fallback, no-match, DB error — Task 1, Step 1.
- ✅ Interface cleanup so `provider.MemoryStore` matches the real `*Store` API — Task 5.
- ✅ Non-goals respected: no auto-create added to CLI paths, no fuzzy matching implemented.

**Placeholder scan:** no TBD/TODO; every step shows complete code or an exact `grep`/`go test` command with expected output.

**Type consistency:** `ResolveProject(ctx, input) (id, name string, err error)` is the same signature used consistently across Tasks 1, 2, 3, 4, and the interface in Task 5. `ListProjectNames(ctx) ([]string, error)` likewise consistent between Task 1 and Task 5.

**Sequencing note:** Tasks 1 → 2 → 3 → 4 → 5 must run in this order — Task 2/3/4 all call `store.ResolveProject`, which doesn't exist until Task 1 lands; Task 5 removes the interface method whose implementation Task 1 already deleted, so it must run last.

---

Plan complete and saved to `docs/superpowers/plans/2026-08-01-project-resolution-unification.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration
2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?
