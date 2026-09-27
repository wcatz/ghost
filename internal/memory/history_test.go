package memory

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// strPtr is the *string an UpdateMemory caller passes to mean "set this field".
func strPtr(s string) *string { return &s }

func itoa(i int) string { return strconv.Itoa(i) }

// phasesOf renders a history as "phase:agent" pairs so an assertion reads as the
// sequence of events rather than a field-by-field comparison.
func phasesOf(t *testing.T, entries []HistoryEntry) []string {
	t.Helper()
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Phase+":"+e.Agent)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMemoryHistoryRecordsSaveThenUpdateWithOldAndNewState: the reason the
// table exists. Before it, an update overwrote the row in place and the text
// Ghost used to believe was gone. Both states must be readable afterwards, in
// the order they were true, each naming the write that produced it.
func TestMemoryHistoryRecordsSaveThenUpdateWithOldAndNewState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const first = "the link worker re-embeds a memory whose content changed"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", first, "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}

	const second = "the link worker re-embeds and re-links a memory whose content changed"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(second), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:claude-code", "update:"}) {
		t.Fatalf("phases = %v, want [save:claude-code update:]", got)
	}
	if len(entries) != 2 {
		t.Fatalf("history has %d entries, want 2", len(entries))
	}
	if entries[0].Content != first {
		t.Errorf("save row content = %q, want the text as first saved (%q)", entries[0].Content, first)
	}
	if entries[1].Content != second {
		t.Errorf("update row content = %q, want the rewritten text (%q)", entries[1].Content, second)
	}
	if entries[0].Category != "gotcha" || entries[1].Category != "gotcha" {
		t.Errorf("categories = %q, %q; want both gotcha", entries[0].Category, entries[1].Category)
	}
	if entries[0].Source != "mcp" {
		t.Errorf("save row source = %q, want mcp", entries[0].Source)
	}
	if entries[1].SessionID != "" {
		t.Errorf("update row session = %q, want empty — UpdateMemory knows no session, and an invented one is fabricated provenance", entries[1].SessionID)
	}
	if entries[0].ResolvedAt != nil {
		t.Errorf("save row resolved_at = %v, want NULL", *entries[0].ResolvedAt)
	}
	if entries[0].Importance != 0.5 {
		t.Errorf("save row importance = %v, want 0.5", entries[0].Importance)
	}
	// recorded_at must be present even though it is second-precision: an
	// undated event cannot be ordered against another one.
	for i, e := range entries {
		if e.RecordedAt == "" {
			t.Errorf("row %d has no recorded_at", i)
		}
		if e.MemoryID != id {
			t.Errorf("row %d memory_id = %q, want %q", i, e.MemoryID, id)
		}
		if e.ProjectID != testProject {
			t.Errorf("row %d project_id = %q, want %q", i, e.ProjectID, testProject)
		}
	}
}

// TestMemoryHistoryRecordsResolveAndUnresolve: resolved_at is a column that is
// overwritten like any other, and "why is this not in my context any more" is
// exactly the question the audit has to answer.
func TestMemoryHistoryRecordsResolveAndUnresolve(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "the staging cluster was migrated to fly iad in March", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	n, err := s.SetResolved(ctx, []string{id})
	if err != nil || n != 1 {
		t.Fatalf("SetResolved = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.ClearResolved(ctx, testProject, []string{id}); err != nil {
		t.Fatalf("ClearResolved: %v", err)
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "resolve:", "unresolve:"}) {
		t.Fatalf("phases = %v, want [save resolve unresolve]", got)
	}
	if len(entries) != 3 {
		t.Fatalf("history has %d entries, want 3", len(entries))
	}
	if entries[1].ResolvedAt == nil {
		t.Error("the resolve row must record the resolved_at the resolve pass stamped")
	}
	if entries[2].ResolvedAt != nil {
		t.Errorf("the unresolve row resolved_at = %q, want NULL", *entries[2].ResolvedAt)
	}
}

// TestMemoryHistoryRecordsSupersedeOnTheOlderMemory: a supersedes edge is the
// claim "this row is no longer current", and it lands on the target of the
// edge. Recording it on the source would file the event under the memory whose
// state did not change.
func TestMemoryHistoryRecordsSupersedeOnTheOlderMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Deliberately unrelated texts: a near-duplicate pair would fold on the
	// second Upsert, and this test is about the edge, not the fold.
	newer, _, _, err := s.Upsert(ctx, testProject, "fact", "the link worker skips a memory whose vector belongs to a retired model", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert newer: %v", err)
	}
	older, _, _, err := s.Upsert(ctx, testProject, "fact", "the regional fly.io deploy script was retired in favour of the compose stack", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert older: %v", err)
	}
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	olderHistory, err := s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(older): %v", err)
	}
	if got := phasesOf(t, olderHistory); !equalStrings(got, []string{"save:", "supersede:"}) {
		t.Errorf("older memory phases = %v, want [save supersede]", got)
	}
	newerHistory, err := s.MemoryHistory(ctx, newer, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(newer): %v", err)
	}
	if got := phasesOf(t, newerHistory); !equalStrings(got, []string{"save:"}) {
		t.Errorf("newer memory phases = %v, want [save] — its state did not change", got)
	}
}

// TestMemoryHistoryRecordsThePerformingAgentPerEvent: the issue's own
// acceptance case. One row, two agents, both events, chronological, with the
// value each one produced.
func TestMemoryHistoryRecordsThePerformingAgentPerEvent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	target, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"The production deploy target is Fly.io in region iad", "mcp", 0.7, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("UpsertWithProvenance (claude-code): %v", err)
	}
	// A near-duplicate from another agent folds into target: the fold
	// strengthens the existing row, so the row is changed by opencode too.
	linked, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"The production deploy target is Hetzner Compose in region fsn1", "mcp", 0.7, nil,
		Provenance{Agent: "opencode", SessionID: "ses_b"})
	if err != nil {
		t.Fatalf("UpsertWithProvenance (opencode): %v", err)
	}
	if linked == target {
		t.Fatal("the fold must keep the incoming wording as its own row, or this test proves nothing")
	}

	entries, err := s.MemoryHistory(ctx, target, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:claude-code", "merge:opencode"}) {
		t.Fatalf("phases = %v, want [save:claude-code merge:opencode]", got)
	}
	if len(entries) != 2 {
		t.Fatalf("history has %d entries, want 2", len(entries))
	}
	if entries[0].SessionID != "ses_a" {
		t.Errorf("save row session = %q, want ses_a", entries[0].SessionID)
	}
	if entries[1].SessionID != "ses_b" {
		t.Errorf("merge row session = %q, want ses_b", entries[1].SessionID)
	}
	// The strengthen is in the history: importance is one of the recorded
	// values, so the pre-fold and post-fold numbers are both readable. The
	// tolerance is float32 → REAL, not slack: the column stores 0.7 as the
	// nearest double to the float32.
	if math.Abs(entries[0].Importance-0.7) > 1e-6 {
		t.Errorf("pre-fold importance = %v, want 0.7", entries[0].Importance)
	}
	if entries[1].Importance <= entries[0].Importance {
		t.Errorf("post-fold importance = %v, want greater than the pre-fold %v",
			entries[1].Importance, entries[0].Importance)
	}
	if entries[1].Content != entries[0].Content {
		t.Errorf("the fold must not overwrite the target's text: %q became %q",
			entries[0].Content, entries[1].Content)
	}
}

// TestMemoryHistoryRecordsReflectRewriteAndDelete: the reflection replace is
// the only writer that deletes rows in bulk, and it is the one whose decisions
// "which reflection run changed this" is asked about.
func TestMemoryHistoryRecordsReflectRewriteAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old, _, _, err := s.Upsert(ctx, testProject, "fact", "vector search is disabled when Ollama is unreachable", "reflection", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:   "fact",
		Content:    "search falls back to FTS5 when Ollama is unreachable",
		Importance: 0.6,
		Source:     "reflection",
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	oldHistory, err := s.MemoryHistory(ctx, old, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(old): %v", err)
	}
	if got := phasesOf(t, oldHistory); !equalStrings(got, []string{"save:", "delete:"}) {
		t.Fatalf("replaced memory phases = %v, want [save delete]", got)
	}
	if oldHistory[1].Content != "vector search is disabled when Ollama is unreachable" {
		t.Errorf("the delete row must carry the text the row held when it was dropped, got %q", oldHistory[1].Content)
	}

	all, err := s.GetAll(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("replace left %d rows, want 1", len(all))
	}
	fresh, err := s.MemoryHistory(ctx, all[0].ID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(new): %v", err)
	}
	if got := phasesOf(t, fresh); !equalStrings(got, []string{"reflect:"}) {
		t.Fatalf("reflected memory phases = %v, want [reflect]", got)
	}
	if fresh[0].Source != "reflection" {
		t.Errorf("reflected row source = %q, want reflection", fresh[0].Source)
	}
}

// TestMemoryHistoryDeleteTombstoneOutlivesTheRow: a hard delete takes the row
// with it, so the history is the only remaining record of what it said. If the
// rows cascaded away with it, the audit would be empty exactly when it is
// needed.
func TestMemoryHistoryDeleteTombstoneOutlivesTheRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const text = "the old deploy script assumes a single region"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", text, "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory after delete: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:claude-code", "delete:"}) {
		t.Fatalf("phases = %v, want [save:claude-code delete:]", got)
	}
	if len(entries) != 2 {
		t.Fatalf("history has %d entries, want 2", len(entries))
	}
	if entries[1].Content != text {
		t.Errorf("tombstone content = %q, want %q", entries[1].Content, text)
	}
	if entries[1].Category != "gotcha" {
		t.Errorf("tombstone category = %q, want gotcha", entries[1].Category)
	}
}

// TestMemoryHistoryProjectDeleteCascades: the project is the row of record for
// the whole corpus. Deleting it must not leave the corpus's history behind,
// which is why the table carries a project_id foreign key even though
// memory_id deliberately does not.
func TestMemoryHistoryProjectDeleteCascades(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a fact that only the doomed project knows", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("history rows after deleting the project = %d, want 0", len(entries))
	}
}

// TestMemoryHistoryCapsRowsPerMemory: the growth policy, per-memory half. A
// memory rewritten thousands of times must not grow the table without bound,
// and the rows that go must be the oldest.
func TestMemoryHistoryCapsRowsPerMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	restore := historyVersionsPerMemory
	historyVersionsPerMemory = 4
	t.Cleanup(func() { historyVersionsPerMemory = restore })

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a memory that keeps being edited", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := s.UpdateMemory(ctx, testProject, id, strPtr("a memory that keeps being edited, revision"), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory %d: %v", i, err)
		}
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != historyVersionsPerMemory {
		t.Fatalf("kept %d rows, want the newest %d", len(entries), historyVersionsPerMemory)
	}
	// The survivors are the newest: the insert is the row that was culled.
	for i, e := range entries {
		if e.Phase == "save" {
			t.Errorf("row %d is the original save, so the cap kept the oldest rows and dropped the newest", i)
		}
	}
}

// TestMemoryHistoryCapsTheTable: the growth policy, global half. The per-memory
// cap cannot bound a corpus that keeps creating and dropping memories — every
// one of them is a memory id with a handful of rows — so the table itself is
// capped.
func TestMemoryHistoryCapsTheTable(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	restore := historyRowsCap
	historyRowsCap = 10
	t.Cleanup(func() { historyRowsCap = restore })

	// The last id is captured as it is returned, not recovered from a listing:
	// GetAll orders by importance and created_at, and 25 rows written in the same
	// second with the same importance have no defined order among themselves, so
	// "the most recent" read back out of it is a guess that changes with timing.
	// (It did: this assertion only failed under -race.)
	var lastID string
	for i := 0; i < 25; i++ {
		id, _, _, err := s.Upsert(ctx, testProject, "fact",
			"a durable fact numbered "+itoa(i)+" about subsystem "+itoa(i%7), "mcp", 0.5, nil)
		if err != nil {
			t.Fatalf("Upsert %d: %v", i, err)
		}
		lastID = id
	}

	var total int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance`).Scan(&total); err != nil {
		t.Fatalf("count history rows: %v", err)
	}
	if total > historyRowsCap {
		t.Errorf("table holds %d rows, want at most the cap of %d", total, historyRowsCap)
	}
	// The newest rows are the ones kept: the last memory written is still
	// readable.
	entries, err := s.MemoryHistory(ctx, lastID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the most recently written memory lost its history — the cap kept the oldest rows")
	}
}

// TestMemoryHistoryLimitKeepsTheNewestRows: a limit must not turn a changelog
// into a truncated head. The returned slice is still chronological.
func TestMemoryHistoryLimitKeepsTheNewestRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a memory edited many times", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.UpdateMemory(ctx, testProject, id, strPtr("a memory edited many times, revision"), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory %d: %v", i, err)
		}
	}
	all, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	limited, err := s.MemoryHistory(ctx, id, 2)
	if err != nil {
		t.Fatalf("MemoryHistory(limit): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited history = %d rows, want 2", len(limited))
	}
	if limited[0].Phase != "update" || limited[1].Phase != "update" {
		t.Errorf("limited history = %v, want the two newest updates", phasesOf(t, limited))
	}
	if len(all) != 6 {
		t.Errorf("unlimited history = %d rows, want 6", len(all))
	}
}

// TestMemoryHistoryUnknownMemoryIsEmpty: a reader must be able to tell "no
// history" from "an error" without a special case at every call site.
func TestMemoryHistoryUnknownMemoryIsEmpty(t *testing.T) {
	s := testStore(t)
	entries, err := s.MemoryHistory(context.Background(), "no-such-memory", 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("history of an unknown memory = %d rows, want 0", len(entries))
	}
}

// TestMemoryHistoryRejectsAnUnknownPhase: the vocabulary is a CHECK, not a
// convention. A phase nobody reads — or a typo that would silently create a
// new one — has to be refused by the schema.
func TestMemoryHistoryRejectsAnUnknownPhase(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec(`INSERT INTO memory_provenance (memory_id, project_id, phase, content, category, importance, source)
		VALUES ('m1', 'p1', 'teleported', 'text', 'fact', 0.5, 'mcp')`)
	if err == nil {
		t.Fatal("an unknown phase was accepted")
	}
	var check string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='memory_provenance'`,
	).Scan(&check); err != nil && err != sql.ErrNoRows {
		t.Fatalf("read table DDL: %v", err)
	}
	if !strings.Contains(check, "CHECK (phase IN") {
		t.Errorf("memory_provenance has no phase CHECK constraint: %s", check)
	}
}

// TestMemoryHistoryRecordsCreateAndDecisionCompanion: the two writers that
// insert a memory outside Upsert. A memory with no recorded origin is the exact
// hole this table closes, so both are covered — a decision's companion memory in
// particular is the row a reader meets when auditing "why is this decision
// stored as a memory at all".
func TestMemoryHistoryRecordsCreateAndDecisionCompanion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, testProject, Memory{
		Category:   "architecture",
		Content:    "the store pins one connection per handle",
		Source:     "manual",
		Importance: 0.6,
		Agent:      "claude-code",
		SessionID:  "ses_a",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, companion, _, err := s.RecordDecision(ctx, testProject, "Single connection per store", "MaxOpenConns(1)", "SQLite is single-writer", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if companion == "" {
		t.Fatal("RecordDecision returned no companion memory id")
	}

	for _, tc := range []struct{ id, wantPhase, wantSource string }{
		{created, "save", "manual"},
		{companion, "save", "decision_log"},
	} {
		entries, err := s.MemoryHistory(ctx, tc.id, 0)
		if err != nil {
			t.Fatalf("MemoryHistory(%s): %v", tc.id, err)
		}
		if len(entries) != 1 {
			t.Fatalf("memory %s has %d history rows, want 1 (the insert)", tc.id, len(entries))
		}
		if entries[0].Phase != tc.wantPhase {
			t.Errorf("memory %s phase = %q, want %q", tc.id, entries[0].Phase, tc.wantPhase)
		}
		if entries[0].Source != tc.wantSource {
			t.Errorf("memory %s history source = %q, want %q", tc.id, entries[0].Source, tc.wantSource)
		}
	}
	// The agent is the one that performed the save, because Create carries one.
	entries, err := s.MemoryHistory(ctx, created, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if entries[0].Agent != "claude-code" || entries[0].SessionID != "ses_a" {
		t.Errorf("Create history agent/session = %q/%q, want claude-code/ses_a", entries[0].Agent, entries[0].SessionID)
	}
}

// TestMemoryHistoryRecordsSeedAndImport: the two remaining insert paths —
// Ghost's own shipped seeds, and a portable artifact. Both land in the corpus
// and neither goes through Upsert, so both have to append or they are the two
// kinds of row the audit cannot account for.
func TestMemoryHistoryRecordsSeedAndImport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.SeedGlobalMemories(ctx); err != nil {
		t.Fatalf("SeedGlobalMemories: %v", err)
	}
	seeds, err := s.GetAll(ctx, GlobalProjectID, 100)
	if err != nil {
		t.Fatalf("GetAll(_global): %v", err)
	}
	if len(seeds) == 0 {
		t.Fatal("no seeds were written")
	}
	seedHistory, err := s.MemoryHistory(ctx, seeds[0].ID, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(seed): %v", err)
	}
	if len(seedHistory) != 1 || seedHistory[0].Phase != "save" {
		t.Fatalf("seed history = %v, want one save row", phasesOf(t, seedHistory))
	}
	if seedHistory[0].Source != "builtin" {
		t.Errorf("seed history source = %q, want builtin", seedHistory[0].Source)
	}

	imported, _, _, err := s.ImportMemory(ctx, PortableMemory{
		ID:        "IMPORTED1",
		ProjectID: testProject,
		Category:  "convention",
		Content:   "run gofmt before committing",
		Source:    "manual",
		Agent:     "claude-code",
		SessionID: "ses_import",
	}, ImportOptions{Apply: true, TrustProvenance: true})
	if err != nil {
		t.Fatalf("ImportMemory: %v", err)
	}
	if !imported {
		t.Fatal("ImportMemory reported no row created")
	}
	importHistory, err := s.MemoryHistory(ctx, "IMPORTED1", 0)
	if err != nil {
		t.Fatalf("MemoryHistory(imported): %v", err)
	}
	if len(importHistory) != 1 || importHistory[0].Phase != "import" {
		t.Fatalf("import history = %v, want one import row", phasesOf(t, importHistory))
	}
	// The artifact's own agent is the performer of the import, so the audit can
	// tell an imported row from one Ghost wrote.
	if importHistory[0].Agent != "claude-code" || importHistory[0].SessionID != "ses_import" {
		t.Errorf("import history agent/session = %q/%q, want claude-code/ses_import",
			importHistory[0].Agent, importHistory[0].SessionID)
	}
}

// TestMemoryHistoryRecordsRestore: a restore is the one write that puts a
// memory's text back. Without a history row the corpus reads as though the
// replace it reverted never happened, and the state a restore reverted FROM is
// unrecoverable from the history.
func TestMemoryHistoryRecordsRestore(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	original, _, _, err := s.Upsert(ctx, testProject, "fact", "search falls back to FTS when Ollama is down", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:   "fact",
		Content:    "ollama being unreachable degrades retrieval to lexical only",
		Importance: 0.5,
		Source:     "reflection",
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	live, err := s.GetByIDs(ctx, []string{original})
	if err != nil || len(live) != 1 {
		t.Fatalf("the snapshot did not bring the row back: %v %v", live, err)
	}
	entries, err := s.MemoryHistory(ctx, original, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	// The replace dropped the row outright (its text was not re-emitted, so there
	// was nothing to reuse), and the restore brought it back under the same id —
	// so the whole episode is one memory's history: saved, deleted by the
	// consolidation, restored by the rollback.
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "delete:", "restore:"}) {
		t.Fatalf("phases = %v, want [save delete restore]", got)
	}
	if entries[1].Content != "search falls back to FTS when Ollama is down" {
		t.Errorf("the delete row records %q, want the text the row held when the replace dropped it", entries[1].Content)
	}
	if entries[2].Content != "search falls back to FTS when Ollama is down" {
		t.Errorf("the restore row records %q, want the text the snapshot held", entries[2].Content)
	}
}

// TestMemoryHistorySurvivesAProjectMerge: a merge KEEPS the corpus — it
// reassigns every child row and then deletes only the projects row. A history
// table whose project_id cascades would therefore lose the whole recorded past
// of every memory the merge moved, leaving live rows whose `ghost history` says
// they were never written. The merge has to carry the history with the memories.
func TestMemoryHistorySurvivesAProjectMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "old-project", "/tmp/old", "old-project"); err != nil {
		t.Fatalf("EnsureProject(old): %v", err)
	}
	if err := s.EnsureProject(ctx, "new-project", "/tmp/new", "new-project"); err != nil {
		t.Fatalf("EnsureProject(new): %v", err)
	}
	id, _, _, err := s.Upsert(ctx, "old-project", "fact", "a fact the old project owned", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.MergeProject(ctx, "old-project", "new-project"); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	moved, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(moved) != 1 {
		t.Fatalf("the merge did not keep the memory: %v %v", moved, err)
	}
	if moved[0].ProjectID != "new-project" {
		t.Errorf("memory project = %q, want new-project", moved[0].ProjectID)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the merge destroyed the memory's history — the projects-row cascade took rows the merge itself was meant to carry")
	}
	if entries[0].Content != "a fact the old project owned" {
		t.Errorf("the surviving history lost the text: %q", entries[0].Content)
	}
}

// TestMemoryHistorySkipsAnAlreadyActiveSupersedeEdge: `ghost supersede` re-links
// a pair whenever an endpoint changed since the edge was written, and the
// upsert usually changes nothing. A row repeating the previous state would
// burn one of the per-memory version slots on every pass until the real
// save/update/reflect versions are pruned away, so a changelog would fill with
// identical entries.
func TestMemoryHistorySkipsAnAlreadyActiveSupersedeEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	newer, _, _, err := s.Upsert(ctx, testProject, "fact", "the retry budget is three attempts with jitter", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert newer: %v", err)
	}
	older, _, _, err := s.Upsert(ctx, testProject, "fact", "the deploy pipeline runs on a weekly schedule", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert older: %v", err)
	}

	// First link: the edge becomes active, so the target's currency changed.
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// The same edge again, as a re-classified pass writes it — already active.
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink (re-link): %v", err)
	}

	entries, err := s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "supersede:"}) {
		t.Fatalf("phases = %v, want [save supersede] — a re-link of a live edge changes nothing about the memory", got)
	}

	// Re-activating an invalidated edge IS a state change, so it is recorded.
	if _, err := s.InvalidateLink(ctx, newer, older, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink (re-activate): %v", err)
	}
	entries, err = s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "supersede:", "supersede:"}) {
		t.Fatalf("phases = %v, want [save supersede supersede] — re-activating the edge is a change", got)
	}
}

// TestMemoryHistorySurvivesARepositoryAutoMerge: the same cascade, reached
// without a command. A save that names a repository another project already
// owns merges the named project into that owner, and the reassignment runs
// through the OTHER of main's two merge implementations — so this is the case a
// test that only calls MergeProject cannot see.
func TestMemoryHistorySurvivesARepositoryAutoMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const remote = "git@github.com:acme/widgets.git"
	if err := s.EnsureProjectWithRepo(ctx, "alpha", "/tmp/alpha", "alpha", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo(alpha): %v", err)
	}
	if err := s.EnsureProject(ctx, "beta", "/tmp/beta", "beta"); err != nil {
		t.Fatalf("EnsureProject(beta): %v", err)
	}
	id, _, _, err := s.Upsert(ctx, "beta", "fact", "a fact only the beta checkout knew", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A second project claiming the same remote is folded into the owner.
	if err := s.EnsureProjectWithRepo(ctx, "beta", "/tmp/beta", "beta", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo(beta): %v", err)
	}

	moved, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(moved) != 1 {
		t.Fatalf("the auto-merge did not keep the memory: %v %v", moved, err)
	}
	if moved[0].ProjectID != "alpha" {
		t.Fatalf("memory project = %q, want alpha — the merge this test needs did not happen", moved[0].ProjectID)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the repository auto-merge destroyed the memory's history")
	}
}

// TestMemoryHistorySurvivesARepoClaimMerge: the third route into a merge, and
// the only one that reaches main's SECOND reassignment implementation. A save
// that names an existing project with no recorded repository, from a directory
// whose remote another project already owns, folds the named project into that
// owner — with no command, no MergeProject, and no project-merge list this test
// could mistake for the one under test.
func TestMemoryHistorySurvivesARepoClaimMerge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const remote = "git@github.com:acme/gadgets.git"
	if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
		t.Fatalf("EnsureProjectWithRepo(owner): %v", err)
	}
	if err := s.EnsureProject(ctx, "named", "/tmp/named", "named"); err != nil {
		t.Fatalf("EnsureProject(named): %v", err)
	}
	id, _, _, err := s.Upsert(ctx, "named", "fact", "a fact the named project recorded", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// The save path: the named project exists, records no repository, and the
	// saving directory's remote is already claimed by "owner".
	resolved, _, err := s.ResolveOrCreateRepoProject(ctx, "named", "named", "named", "/tmp/named", "named", remote)
	if err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}
	if resolved != "owner" {
		t.Fatalf("resolved project = %q, want owner — the merge this test needs did not happen", resolved)
	}
	moved, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(moved) != 1 {
		t.Fatalf("the merge did not keep the memory: %v %v", moved, err)
	}
	if moved[0].ProjectID != "owner" {
		t.Fatalf("memory project = %q, want owner", moved[0].ProjectID)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the repository-claim merge destroyed the memory's history")
	}
}

// TestMemoryHistoryFollowsAMemoryPromotedToGlobal: the same cascade hazard as
// the merge, reached by promotion. A promoted memory leaves its project but
// keeps its id, and its history rows name the project it left — so deleting
// that project afterwards takes the recorded past of a memory that is still
// live in _global. Promotion has to move the history with the memory.
func TestMemoryHistoryFollowsAMemoryPromotedToGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a rule worth injecting everywhere", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, testProject, id); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}

	promoted, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(promoted) != 1 {
		t.Fatalf("the promotion did not keep the memory: %v %v", promoted, err)
	}
	if promoted[0].ProjectID != GlobalProjectID {
		t.Fatalf("memory project = %q, want %s", promoted[0].ProjectID, GlobalProjectID)
	}

	// The ordinary follow-up: the project it left is deleted.
	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	stillThere, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(stillThere) != 1 {
		t.Fatalf("deleting the old project took the promoted memory: %v %v", stillThere, err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("deleting the project a memory was promoted out of destroyed its history — the rows still named the project it left")
	}
	if entries[0].Content != "a rule worth injecting everywhere" {
		t.Errorf("the surviving history lost the text: %q", entries[0].Content)
	}
}
