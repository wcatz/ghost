# Remove Dead Conversation-Logging Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the dead `conversations`/`messages` schema and all code that reads/writes it (`CreateConversation`, `AppendMessage`, `GetRecentExchanges`, `GetLatestConversation`, `GetConversationMessages`), plus two related dead-code items in the same file (`ExtractionPrompt` const, `ReflectionInput.LastCommits`/`ProjectLanguage` fields) — no replacement functionality, no data migration.

**Architecture:** These tables are always empty in production (their only writer, `AppendMessage`/`CreateConversation`, has zero production callers — only test code calls them), so removal is a straight schema drop via a new migration step plus deletion of the dead Go code paths that reference them. `LastCommits`/`ProjectLanguage` are separately dead: fields read by the prompt builder but never populated anywhere.

**Tech Stack:** Go 1.26+, SQLite (modernc.org/sqlite, pure Go), `go test`/`go vet`.

**Spec:** `docs/superpowers/specs/2026-08-06-remove-dead-conversation-logging-design.md`

---

## File Structure

| File | Change |
|---|---|
| `internal/memory/schema.go` | Remove `conversations`/`messages` tables + 2 indexes from `initSQL` |
| `internal/memory/migrate.go` | Add `migrateV4`, bump `schemaVersion` to 4 |
| `internal/memory/store.go` | Remove 5 methods + `ConversationMessage` type |
| `internal/provider/provider.go` | Remove 5 interface methods |
| `internal/reflection/prompt.go` | Remove 3 struct fields, 2 prompt sections, `ExtractionPrompt` const |
| `cmd/ghost/main.go` | Remove `GetRecentExchanges` call + struct field |
| `docs/architecture.md` | Update schema table + reflect data-flow diagram |
| `internal/memory/store_test.go` | Remove 3 test functions |
| `internal/reflection/consolidator_test.go` | Remove 1 field from a test literal |

All work happens in one feature branch, one PR. Tasks are ordered migration + schema first, then the Go call sites, then docs, then tests last (since they're the thing that made the dead code "look" alive). Tasks 3-6 touch interface/struct/caller in sequence and the repo will not build partway through that span (interface still declares removed methods, callers still reference removed fields) — **squash Tasks 3-6 into a single commit** so every commit that lands on the branch has `go vet ./...` and `go test ./...` passing; do not commit Tasks 3, 4, or 5 individually.

---

### Task 1: Add migrateV4 dropping conversations/messages, bump schemaVersion

**Files:**
- Modify: `internal/memory/migrate.go:14` (schemaVersion), `internal/memory/migrate.go:21-25` (migrations slice)
- Test: `internal/memory/migrate_test.go` (create if it doesn't exist — check first)

- [ ] **Step 1: Check for an existing migration test file**

Run: `ls internal/memory/migrate_test.go 2>&1`

If it exists, read it fully first to match its existing style/helpers before adding a test. If it doesn't exist, the test in Step 2 goes in `internal/memory/store_test.go` instead (it already has a `testStore`-style DB helper) — check `store_test.go` for a helper that opens a DB at a specific `PRAGMA user_version` or creates one via raw SQL; if none exists, write the test using `sql.Open("sqlite", ":memory:")` directly and run `initSQL`-equivalent DDL by hand for the "old" schema, since `OpenDB` itself always creates fresh DBs at the *current* `schemaVersion` and can't be used to simulate an old on-disk DB.

- [ ] **Step 2: Write the failing test for migrateV4**

Add to `internal/memory/store_test.go` (append near other schema/migration-adjacent tests, or create `internal/memory/migrate_test.go` with `package memory` if Step 1 found no existing file):

```go
func TestMigrateV4DropsConversationsAndMessages(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Build a v3 database by hand: current initSQL minus messages/conversations
	// would already omit them, so instead create the OLD schema (with the
	// tables) directly, stamped at version 3, to prove migrateV4 removes them.
	oldSchema := `
CREATE TABLE projects (
    id TEXT PRIMARY KEY, path TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE conversations (
    id TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    mode TEXT NOT NULL DEFAULT 'chat', title TEXT DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE messages (
    id TEXT PRIMARY KEY DEFAULT (hex(randomblob(16))),
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool_use', 'tool_result')),
    content TEXT NOT NULL, tool_name TEXT, tool_use_id TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("stamp v3: %v", err)
	}

	if err := migrate(db, 3); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("expected version %d, got %d", schemaVersion, version)
	}

	for _, table := range []string{"conversations", "messages"} {
		var count int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("expected %s to be dropped, but it still exists", table)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/memory/... -run TestMigrateV4DropsConversationsAndMessages -v`
Expected: FAIL — `schemaVersion` is still 3, so `migrate(db, 3)` loops zero times and the tables are never dropped (the version-mismatch assertion fails, or the tables still exist).

- [ ] **Step 4: Implement migrateV4**

In `internal/memory/migrate.go`, change line 14:

```go
const schemaVersion = 4
```

Change the `migrations` slice (lines 21-25):

```go
var migrations = []func(*sql.Tx) error{
	migrateV1,
	migrateV2,
	migrateV3,
	migrateV4,
}
```

Add a new function after `migrateV3` (after line 176, before `columnExists`):

```go
// migrateV4 drops the conversations/messages tables. AppendMessage and
// CreateConversation — their only writers — had zero production callers
// (only test code called them), so in every real deployment both tables
// were permanently empty; GetRecentExchanges always returned an empty
// slice. As a safety check against that assumption being wrong on some
// deployment, this aborts without dropping anything if either table is
// non-empty — no preservation/export path is provided; see
// docs/superpowers/specs/2026-08-06-remove-dead-conversation-logging-design.md.
// Either table may already be absent on some on-disk DBs (e.g. a DB created
// between the v3 schema landing and this migration, or one already patched
// by hand) — tableExists guards the COUNT so migrateV4 is a no-op for a
// table that isn't there rather than erroring on "no such table".
func migrateV4(tx *sql.Tx) error {
	for _, table := range []string{"messages", "conversations"} {
		exists, err := tableExists(tx, table)
		if err != nil {
			return fmt.Errorf("check %s exists: %w", table, err)
		}
		if !exists {
			continue
		}
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			return fmt.Errorf("count %s: %w", table, err)
		}
		if count > 0 {
			return fmt.Errorf("refusing to drop non-empty table %s (%d rows)", table, count)
		}
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS messages`); err != nil {
		return fmt.Errorf("drop messages: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS conversations`); err != nil {
		return fmt.Errorf("drop conversations: %w", err)
	}
	return nil
}

// tableExists reports whether the named table exists in the current schema.
func tableExists(tx *sql.Tx, name string) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
```

Check whether `columnExists` (the existing helper right after this function) already has a sibling `tableExists`-style helper before adding a new one — reuse it instead of duplicating if so.

- [ ] **Step 4b: Add fixture coverage for each table-missing case**

Add two more test cases alongside `TestMigrateV4DropsConversationsAndMessages` (table-driven or as separate functions, matching the file's existing style): one that runs the v3 DDL with only `CREATE TABLE conversations` omitted, one with only `CREATE TABLE messages` omitted, each asserting `migrate(db, 3)` still succeeds and ends at `schemaVersion` with neither table present.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/memory/... -run TestMigrateV4DropsConversationsAndMessages -v`
Expected: PASS

- [ ] **Step 5b: Write a fixture test proving migrateV4 refuses to drop non-empty tables**

Add to `internal/memory/store_test.go` (or `migrate_test.go`, matching Step 1's choice):

```go
func TestMigrateV4AbortsOnNonEmptyConversations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(oldSchema); err != nil { // reuse the v3 DDL from Step 2
		t.Fatalf("create old schema: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("stamp v3: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/p1', 'p1')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO conversations (id, project_id) VALUES ('c1', 'p1')`); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (conversation_id, role, content) VALUES ('c1', 'user', 'hi')`); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	if err := migrate(db, 3); err == nil {
		t.Fatal("expected migrate to fail on non-empty conversations/messages tables, got nil error")
	}

	var convCount, msgCount int
	if err := db.QueryRow(`SELECT count(*) FROM conversations`).Scan(&convCount); err != nil {
		t.Fatalf("check conversations: %v", err)
	}
	if convCount != 1 {
		t.Errorf("expected conversations row to survive the aborted migration, got count %d", convCount)
	}
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgCount); err != nil {
		t.Fatalf("check messages: %v", err)
	}
	if msgCount != 1 {
		t.Errorf("expected messages row to survive the aborted migration, got count %d", msgCount)
	}

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 3 {
		t.Errorf("expected user_version to remain 3 after aborted migration, got %d", version)
	}
}
```

`migrateV4` checks `messages` before `conversations` (messages FKs to conversations), so this test seeds both to exercise that ordering and confirms the abort happens before either table is touched.

(Extract the `oldSchema` string from Step 2 into a shared package-level const if it isn't already, so both tests can reuse it.)

Run: `go test ./internal/memory/... -run TestMigrateV4AbortsOnNonEmptyConversations -v`
Expected: PASS

- [ ] **Step 6: Run the full memory package test suite to check for regressions**

Run: `go test ./internal/memory/...`
Expected: PASS (other tests still reference `CreateConversation` etc. at this point — that's fine, they aren't touched until Task 6; this step just confirms Task 1 itself didn't break anything else)

- [ ] **Step 7: Commit**

```bash
git add internal/memory/migrate.go internal/memory/store_test.go
git commit -m "feat(memory): add migrateV4 dropping dead conversations/messages tables"
```

(If you created `internal/memory/migrate_test.go` instead in Step 1, `git add` that file instead of `store_test.go`.)

---

### Task 2: Remove conversations/messages from fresh-DB schema

**Files:**
- Modify: `internal/memory/schema.go:71-91`

- [ ] **Step 1: Remove the dead tables from initSQL**

In `internal/memory/schema.go`, delete lines 71-91 (the `conversations` table, `messages` table, and their two indexes) so that the `memories`-related indexes (ending `...idx_memories_project_source`) are followed directly by the `ghost_state` table:

```go
CREATE INDEX IF NOT EXISTS idx_memories_project_cat ON memories(project_id, category);
CREATE INDEX IF NOT EXISTS idx_memories_project_imp ON memories(project_id, importance DESC);
CREATE INDEX IF NOT EXISTS idx_memories_project_source ON memories(project_id, source);

CREATE TABLE IF NOT EXISTS ghost_state (
```

(Delete everything between `idx_memories_project_source` and `ghost_state` — the `conversations`/`messages` `CREATE TABLE` blocks and `idx_conversations_project`/`idx_messages_conv` indexes.)

- [ ] **Step 2: Verify a fresh DB no longer creates these tables**

Run: `go test ./internal/memory/... -run TestOpenDB -v`

(If no test named `TestOpenDB` exists, instead run the full package tests in the next step.)

Add a direct fresh-DB schema assertion so this isn't inferred only from `migrateV4`'s old-schema fixture in Task 1 (which proves tables are *droppable*, not that `initSQL` no longer creates them):

```go
func TestOpenDBOmitsDeadConversationTables(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"conversations", "messages"} {
		var count int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count)
		if err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		if count != 0 {
			t.Errorf("expected fresh DB to omit %s table, but it exists", name)
		}
	}
}
```

- [ ] **Step 3: Run the full memory package tests**

Run: `go test ./internal/memory/...`
Expected: FAIL for the tests removed in Task 7 (`TestStoreConversations`, `TestStoreGetRecentExchanges`, `TestStoreGetLatestConversationNoRows`) — they still call `CreateConversation`/`AppendMessage`/etc., which still compile fine (not removed until Task 3), but now run against a fresh test DB from `testStore(t)` that simply lacks the tables, so their queries fail at runtime. This is the expected, correct signal at this point in the sequence — proceed straight into Tasks 3-6 (squashed into one commit) without pausing to fix it here; Task 7 deletes these tests for good.

- [ ] **Step 4: Commit**

```bash
git add internal/memory/schema.go
git commit -m "chore(memory): remove dead conversations/messages tables from fresh-DB schema"
```

---

### Task 3: Remove conversation-persistence methods from Store

**Files:**
- Modify: `internal/memory/store.go:1174-1291`

- [ ] **Step 1: Delete the dead methods**

In `internal/memory/store.go`, delete the entire block from the `// --- Conversation persistence ---` comment (line 1174) through the end of `GetConversationMessages` (line 1290), i.e. everything between:

```go
// --- Conversation persistence ---

// CreateConversation starts a new conversation.
```

and (inclusive of):

```go
	return msgs, rows.Err()
}
```

immediately before the `// RecordUsage saves token usage for cost tracking.` comment (line 1292). This removes: `CreateConversation`, `AppendMessage`, `GetRecentExchanges`, `GetLatestConversation`, the `ConversationMessage` type, and `GetConversationMessages` — five funcs/methods and one type, in full.

- [ ] **Step 2: Confirm the package fails to compile (expected — callers still exist)**

Run: `go build ./...`
Expected: FAIL — compile errors in `internal/provider/provider.go` (interface still declares these methods — Go doesn't error on that by itself, interfaces don't need implementations to compile) and in `cmd/ghost/main.go` (calls `store.GetRecentExchanges`) and in `internal/memory/store_test.go` (calls the removed methods/type). This is expected; do NOT commit yet — stage these changes and continue straight into Task 4, 5, and 6, then make one combined commit at the end of Task 6 (Step 7 there) covering Tasks 3-6.

---

### Task 4: Remove conversation-persistence methods from the MemoryStore interface

**Files:**
- Modify: `internal/provider/provider.go:77-82`

- [ ] **Step 1: Remove the interface methods**

In `internal/provider/provider.go`, delete lines 77-82:

```go
	// Conversation persistence
	CreateConversation(ctx context.Context, projectID, mode string) (string, error)
	AppendMessage(ctx context.Context, conversationID, role, content string) error
	GetRecentExchanges(ctx context.Context, projectID string, limit int) ([][2]string, error)
	GetLatestConversation(ctx context.Context, projectID string) (string, error)
	GetConversationMessages(ctx context.Context, conversationID string) ([]memory.ConversationMessage, error)
```

so that `MergeProject(...)` is immediately followed by the `// State` comment and `IncrementInteraction`.

- [ ] **Step 2: Check for a stray unused import**

Run: `grep -n '"github.com/wcatz/ghost/internal/memory"' internal/provider/provider.go`

If `memory.ConversationMessage` was the only remaining use of the `memory` package alias in this file, `go vet`/`go build` in the next step will report an unused import — check the grep output against other `memory.` usages in the file (e.g. `memory.Task`, `memory.Decision`, `memory.Project`) before assuming it needs removal. Given this interface has many other `memory.X` return types (`memory.Task`, `memory.Decision`, etc.), the import stays.

- [ ] **Step 3: Continue — do not commit yet**

Stage these changes and continue into Task 5 and Task 6; this is part of the combined Tasks 3-6 commit made at the end of Task 6.

---

### Task 5: Remove RecentExchanges wiring from main.go

**Files:**
- Modify: `cmd/ghost/main.go:281`, `cmd/ghost/main.go:286-291`

- [ ] **Step 1: Remove the GetRecentExchanges call and the RecentExchanges field**

In `cmd/ghost/main.go`, delete line 281:

```go
	exchanges, _ := store.GetRecentExchanges(ctx, projectID, 15)
```

(leaving a blank line between `currentContext, _ := store.GetLearnedContext(ctx, projectID)` and `fmt.Printf("Memories:     %d existing\n", len(existingMemories))` — collapse to a single blank line, don't leave two.)

Then change the `ReflectionInput{}` literal (lines 286-291) from:

```go
	input := reflection.ReflectionInput{
		RecentExchanges:  exchanges,
		ExistingMemories: existingMemories,
		CurrentContext:   currentContext,
		ProjectName:      projectName,
	}
```

to:

```go
	input := reflection.ReflectionInput{
		ExistingMemories: existingMemories,
		CurrentContext:   currentContext,
		ProjectName:      projectName,
	}
```

- [ ] **Step 2: Confirm this file now builds in isolation (still expect package-level failures elsewhere until Task 6)**

Run: `go build ./cmd/... > /tmp/ghost-build-cmd.log 2>&1; echo "exit: $?"; cat /tmp/ghost-build-cmd.log`
Expected: nonzero exit (the build as a whole still fails) — check the captured log for `cmd/ghost/main.go` specifically: it should have no errors referencing that file. Remaining errors come from `internal/reflection` still having the old struct shape until Task 6 — that's fine, Task 6 fixes it next. Checking the exit code first (rather than piping straight through `grep -v`) avoids masking a real `cmd/ghost/main.go` failure that happens to mention one of the filtered package names in its message text.

- [ ] **Step 3: Continue — do not commit yet**

Stage these changes and continue into Task 6; this is part of the combined Tasks 3-6 commit made at the end of Task 6.

---

### Task 6: Remove RecentExchanges/LastCommits/ProjectLanguage fields and ExtractionPrompt from prompt.go

**Files:**
- Modify: `internal/reflection/prompt.go:1-151`
- Modify: `internal/reflection/consolidator_test.go:288`

- [ ] **Step 1: Update the ReflectionInput struct**

In `internal/reflection/prompt.go`, change lines 10-18 from:

```go
// ReflectionInput holds all data fed into the reflection prompt.
type ReflectionInput struct {
	RecentExchanges  [][2]string     // [user, assistant] pairs
	ExistingMemories []memory.Memory // all memories (up to 200)
	CurrentContext   string          // learned_context from ghost_state
	LastCommits      []string        // recent commit messages
	ProjectLanguage  string
	ProjectName      string
}
```

to:

```go
// ReflectionInput holds all data fed into the reflection prompt.
type ReflectionInput struct {
	ExistingMemories []memory.Memory // all memories (up to 200)
	CurrentContext   string          // learned_context from ghost_state
	ProjectName      string
}
```

- [ ] **Step 2: Remove the Recent Exchanges and Recent Git Activity prompt sections**

In `BuildReflectionPrompt`, delete lines 45-67 (the two `if len(input.X) > 0` blocks for `RecentExchanges` and `LastCommits`):

```go
	// Recent code exchanges.
	if len(input.RecentExchanges) > 0 {
		_, _ = fmt.Fprintf(&sb, "## Recent Exchanges (last %d)\n", len(input.RecentExchanges))
		for _, e := range input.RecentExchanges {
			userMsg := e[0]
			if len(userMsg) > 500 {
				userMsg = userMsg[:500] + "..."
			}
			assistantMsg := e[1]
			if len(assistantMsg) > 500 {
				assistantMsg = assistantMsg[:500] + "..."
			}
			_, _ = fmt.Fprintf(&sb, "- User: %q -> Ghost: %q\n", userMsg, assistantMsg)
		}
	}

	// Recent git activity.
	if len(input.LastCommits) > 0 {
		_, _ = fmt.Fprintf(&sb, "\n## Recent Git Activity (%d commits)\n", len(input.LastCommits))
		for _, c := range input.LastCommits {
			_, _ = fmt.Fprintf(&sb, "- %s\n", c)
		}
	}

	// Project info.
	_, _ = fmt.Fprintf(&sb, "\n## Project\n- Name: %s\n- Language: %s\n", input.ProjectName, input.ProjectLanguage)
```

Replace that whole span with just:

```go
	// Project info.
	_, _ = fmt.Fprintf(&sb, "\n## Project\n- Name: %s\n", input.ProjectName)
```

- [ ] **Step 3: Remove the dead ExtractionPrompt const**

Delete lines 125-151 in full (the `// ExtractionPrompt is the system prompt...` comment through the closing backtick of the const), so the file ends at `quoteData`'s closing brace (formerly line 123).

- [ ] **Step 4: Fix the test that sets ProjectLanguage**

In `internal/reflection/consolidator_test.go`, in `TestBuildReflectionPrompt_IncludesTags`, remove line 288:

```go
		ProjectLanguage: "go",
```

so the literal becomes:

```go
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName: "ghost",
		ExistingMemories: []memory.Memory{
```

- [ ] **Step 5: Search for any other ExtractionPrompt/RecentExchanges/LastCommits/ProjectLanguage references before building**

Run this from the repo root: `grep -rn "ExtractionPrompt\|RecentExchanges\|LastCommits\|ProjectLanguage" --include=*.go .`
Expected: no output (all references removed). If anything remains outside the files already covered by this plan, stop and investigate before proceeding — it means a caller wasn't accounted for.

- [ ] **Step 6: Build, vet, and test the whole repo**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS — this is the first point since Task 3 where the whole repo compiles again, and it must be fully green before the commit in Step 7.

- [ ] **Step 7: Commit Tasks 3-6 together**

```bash
git add internal/memory/store.go internal/provider/provider.go cmd/ghost/main.go internal/reflection/prompt.go internal/reflection/consolidator_test.go
git commit -m "chore: remove dead conversation-persistence code and unused reflection-prompt fields"
```

This single commit covers Task 3 (Store methods), Task 4 (interface methods), Task 5 (main.go wiring), and Task 6 (prompt.go fields/const) — the repo builds, vets, and tests clean at this commit.

---

### Task 7: Remove the now-dead conversation tests from store_test.go

**Files:**
- Modify: `internal/memory/store_test.go` (three functions: `TestStoreConversations`, `TestStoreGetRecentExchanges`, `TestStoreGetLatestConversationNoRows`)

- [ ] **Step 1: Delete TestStoreConversations**

Delete the full function (currently spanning what was lines 1347-1403 before Task 1's addition shifted line numbers — locate by function name, not by line number, since Task 1 may have appended a new test above/below):

```go
func TestStoreConversations(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a conversation.
	convID, err := s.CreateConversation(ctx, testProject, "chat")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if convID == "" {
		t.Fatal("CreateConversation returned empty ID")
	}

	// Append messages.
	if err := s.AppendMessage(ctx, convID, "user", "Hello ghost"); err != nil {
		t.Fatalf("AppendMessage user: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "assistant", "Hello! How can I help?"); err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "user", "What is Go?"); err != nil {
		t.Fatalf("AppendMessage user 2: %v", err)
	}
	if err := s.AppendMessage(ctx, convID, "assistant", "Go is a programming language."); err != nil {
		t.Fatalf("AppendMessage assistant 2: %v", err)
	}

	// GetConversationMessages.
	msgs, err := s.GetConversationMessages(ctx, convID)
	if err != nil {
		t.Fatalf("GetConversationMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "Hello ghost" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content != "Go is a programming language." {
		t.Errorf("unexpected last message: %+v", msgs[3])
	}

	// GetLatestConversation.
	latestID, err := s.GetLatestConversation(ctx, testProject)
	if err != nil {
		t.Fatalf("GetLatestConversation: %v", err)
	}
	if latestID != convID {
		t.Errorf("expected latest conv %q, got %q", convID, latestID)
	}

	// GetLatestConversation for non-existent project.
	_, err = s.GetLatestConversation(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent project conversation")
	}
}
```

- [ ] **Step 2: Delete TestStoreGetRecentExchanges**

Delete the full function immediately following (same removal, entire body):

```go
func TestStoreGetRecentExchanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	convID, err := s.CreateConversation(ctx, testProject, "chat")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	// Insert 3 pairs of user/assistant messages with distinct timestamps.
	// SQLite datetime('now') has only second precision, so we insert with
	// explicit timestamps to guarantee ordering.
	msgs := []struct {
		role, content, ts string
	}{
		{"user", "first question", "2026-01-01 00:00:01"},
		{"assistant", "first answer", "2026-01-01 00:00:02"},
		{"user", "second question", "2026-01-01 00:00:03"},
		{"assistant", "second answer", "2026-01-01 00:00:04"},
		{"user", "third question", "2026-01-01 00:00:05"},
		{"assistant", "third answer", "2026-01-01 00:00:06"},
	}
	for _, m := range msgs {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`,
			convID, m.role, m.content, m.ts)
		if err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	// Get last 2 exchanges.
	pairs, err := s.GetRecentExchanges(ctx, testProject, 2)
	if err != nil {
		t.Fatalf("GetRecentExchanges: %v", err)
	}
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}

	// Should be the last 2, in chronological order.
	if pairs[0][0] != "second question" || pairs[0][1] != "second answer" {
		t.Errorf("expected second exchange first, got %v", pairs[0])
	}
	if pairs[1][0] != "third question" || pairs[1][1] != "third answer" {
		t.Errorf("expected third exchange second, got %v", pairs[1])
	}

	// Get 0 exchanges.
	pairs, err = s.GetRecentExchanges(ctx, testProject, 0)
	if err != nil {
		t.Fatalf("GetRecentExchanges(0): %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected 0 pairs, got %d", len(pairs))
	}
}
```

- [ ] **Step 3: Delete TestStoreGetLatestConversationNoRows**

Delete the full function:

```go
func TestStoreGetLatestConversationNoRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	_, err := s.GetLatestConversation(ctx, testProject)
	if err == nil {
		t.Error("expected error for no conversations")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}
```

- [ ] **Step 4: Confirm sql import is still used**

Run: `grep -n '\bsql\.' internal/memory/store_test.go`
Expected: at least one remaining match (e.g. `var resolvedAt sql.NullString` elsewhere in the file) — confirms the `database/sql` import doesn't go unused after this deletion. If it comes back with zero matches, remove the `"database/sql"` import from the file's import block.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS — all packages compile and all tests pass, with no references to the removed identifiers anywhere.

- [ ] **Step 6: Run go vet**

Run: `go vet ./...`
Expected: no output (clean).

- [ ] **Step 7: Commit**

```bash
git add internal/memory/store_test.go
git commit -m "test(memory): remove tests for deleted conversation-persistence methods"
```

---

### Task 8: Update docs/architecture.md

**Files:**
- Modify: `docs/architecture.md:108-121` (reflect data-flow diagram), `docs/architecture.md:148-167` (SQLite Schema table)

- [ ] **Step 1: Remove GetRecentExchanges from the reflect data-flow diagram**

In `docs/architecture.md`, change:

````text
### Memory Consolidation (ghost reflect)
```
ghost reflect <project> --apply
  → store.GetAll()           # existing memories
  → store.GetRecentExchanges() # recent conversation history
  → TieredConsolidator.Consolidate()
```
````

to:

````text
### Memory Consolidation (ghost reflect)
```
ghost reflect <project> --apply
  → store.GetAll()           # existing memories
  → TieredConsolidator.Consolidate()
```
````

(Leave the rest of that code block — `HaikuConsolidator`, `SQLiteConsolidator`, quality gate, `ReplaceNonManual`, `UpdateLearnedContext` lines — unchanged.)

- [ ] **Step 2: Remove conversations/messages from the SQLite Schema table**

Delete these two rows from the table at lines 148-167:

```markdown
| `conversations` | Conversation sessions (project, mode, timestamps) |
| `messages` | Conversation messages (role, content) |
```

- [ ] **Step 3: Verify no other stale references remain in the docs**

Run: `grep -n "GetRecentExchanges\|conversations\|messages" docs/architecture.md`
Expected: no matches (or only matches unrelated to this feature, e.g. if "messages" appears in unrelated prose — inspect any hits before assuming they're fine).

- [ ] **Step 4: Commit**

```bash
git add docs/architecture.md
git commit -m "docs: remove dead conversations/messages tables and reflect step from architecture.md"
```

---

### Task 9: Final full-repo verification

**Files:** none (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 2: Full vet**

Run: `go vet ./...`
Expected: no output

- [ ] **Step 3: Full test suite**

Run: `go test ./...`
Expected: PASS, all packages

- [ ] **Step 4: Confirm zero remaining references to every removed identifier**

Run this from the repo root: `grep -rn "ExtractionPrompt\|RecentExchanges\|LastCommits\|ProjectLanguage\|ConversationMessage\|GetLatestConversation\|GetConversationMessages\|CreateConversation\|AppendMessage\|GetRecentExchanges" --include=*.go .`
Expected: no output.

- [ ] **Step 5: Manually exercise ghost reflect against a scratch DB**

Run:
```bash
go build -o /tmp/ghost-plan-check ./cmd/ghost
GHOST_DATA_DIR=/tmp/ghost-plan-check-data /tmp/ghost-plan-check reflect ghost --apply
```
Expected: command runs to completion without error (it may report "0% of existing memories returned" or similar quality-gate messaging depending on the scratch DB's contents — the point is it doesn't crash or reference a missing table/column). Clean up afterward: `rm -rf /tmp/ghost-plan-check /tmp/ghost-plan-check-data`.

- [ ] **Step 6: Confirm no uncommitted changes remain**

Run: `git status`
Expected: working tree clean. History doesn't need one commit per task — Tasks 3-6 are deliberately squashed into a single commit (Task 6 Step 7) so every commit on the branch builds/vets/tests clean; what matters is that the PR's required CI checks pass on the final state, not a specific commit count.

---

## Self-Review Notes (for whoever executes this plan)

- Spec coverage: all 9 file-level changes from the spec's "Changes" section (1-9) map 1:1 to Tasks 1-8 here (Task 9 is pure verification, not in the spec's numbered list, but covered by the spec's "Testing" section).
- No data migration needed — confirmed both by the spec and by Task 1's test proving the tables are unconditionally empty-droppable.
- Tasks 3-6 touch interface/struct/caller in sequence and are squashed into one commit (made at the end of Task 6) so no commit on the branch has a broken build — see the note under "File Structure" above.
