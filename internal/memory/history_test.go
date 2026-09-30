package memory

import (
	"context"
	"database/sql"
	"fmt"
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

	// The TABLE is what the cap bounds, so that is what is asserted. Asserting the
	// read instead would prove nothing: MemoryHistory's own default limit is
	// historyVersionsPerMemory, so a read returns exactly that many rows whether
	// the prune ran or not. This assertion was that mistake, and the mutation
	// that disabled the prune entirely — a memory's history grew without bound and
	// the test still passed.
	var stored int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ?`, id,
	).Scan(&stored); err != nil {
		t.Fatalf("count history rows: %v", err)
	}
	if stored != historyVersionsPerMemory {
		t.Fatalf("the table holds %d rows for this memory, want the cap of %d — the prune did not run",
			stored, historyVersionsPerMemory)
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != historyVersionsPerMemory {
		t.Fatalf("read back %d rows, want the newest %d", len(entries), historyVersionsPerMemory)
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
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history`).Scan(&total); err != nil {
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

	_, err = db.Exec(`INSERT INTO memory_history (memory_id, project_id, phase, content, category, importance, source)
		VALUES ('m1', 'p1', 'teleported', 'text', 'fact', 0.5, 'mcp')`)
	if err == nil {
		t.Fatal("an unknown phase was accepted")
	}
	var check string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='memory_history'`,
	).Scan(&check); err != nil && err != sql.ErrNoRows {
		t.Fatalf("read table DDL: %v", err)
	}
	if !strings.Contains(check, "CHECK (phase IN") {
		t.Errorf("memory_history has no phase CHECK constraint: %s", check)
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

	// Invalidating it withdraws the claim, which is a change in its own right, and
	// re-activating it asserts the claim again — so the pair is two events, not
	// one.
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
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "supersede:", "unsupersede:", "supersede:"}) {
		t.Fatalf("phases = %v, want [save supersede unsupersede supersede] — a withdrawal and a fresh claim are both changes", got)
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

// TestDeleteWithPurgeHistoryLeavesNothing: the redaction path. The history
// deliberately outlives the row, so a plain delete KEEPS the text a memory held
// — which is the whole point of the table and also how a credential that was
// "removed" by deleting its memory would survive in a store with a longer memory
// than the row. A purge is the one delete that erases both, in one transaction.
func TestDeleteWithPurgeHistoryLeavesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	secret := "the deploy key is ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	// preGuardRow, not UpsertWithProvenance: the premise of this test is a
	// credential that reached the history, and after this PR neither is reachable
	// through a writer. It is still reachable on any database written before the
	// guard, which is the population a purge is for. See preGuardRow.
	id := preGuardRow(t, s, Memory{
		Category: "gotcha", Content: secret, Source: "mcp", Importance: 0.5,
		Agent: "claude-code", SessionID: "ses_a",
	})
	if err := s.UpdateMemory(ctx, testProject, id, strPtr("the deploy key has been rotated"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	// Sanity: the credential really is in the history, or the purge proves nothing.
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) < 2 || !strings.Contains(entries[0].Content, "ghp_ABCDEF") {
		t.Fatalf("history does not hold the original text: %v", phasesOf(t, entries))
	}

	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	entries, err = s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory after purge: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a purge left %d history rows (%v)", len(entries), phasesOf(t, entries))
	}
	live, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(live) != 0 {
		t.Errorf("the memory row survived the purge: %v %v", live, err)
	}
	// Nothing anywhere in the table carries the text any more — not this memory's
	// rows, and not a row some other memory's writer copied.
	var leaked int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE content LIKE '%ghp_ABCDEF%'`,
	).Scan(&leaked); err != nil {
		t.Fatalf("scan for the credential: %v", err)
	}
	if leaked != 0 {
		t.Errorf("%d history row(s) still carry the purged credential", leaked)
	}
}

// TestDeleteWithoutPurgeKeepsTheTombstone: the default must stay a retirement,
// not a redaction, or the history would be unusable for its stated purpose.
func TestDeleteWithoutPurgeKeepsTheTombstone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a fact that outlives its row", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "delete:"}) {
		t.Errorf("phases = %v, want [save delete]", got)
	}
}

// TestRedactHistoryContentRewritesWhatIsStored: the seam a credential detector
// plugs into (internal/secret, #656 — not on main yet). It is a var so the
// plumbing is testable before the detector exists; without this test, #656 would
// land as a change of shape and a reader could not tell whether the rewrite
// happens on the way IN or only on the way out.
func TestRedactHistoryContentRewritesWhatIsStored(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	restore := setHistoryRedactor(func(content string) string {
		return strings.ReplaceAll(content, "ghp_SECRET", "[redacted]")
	})
	t.Cleanup(restore)

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "the deploy key is ghp_SECRET", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, id, strPtr("the deploy key was rotated"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no history recorded")
	}
	for i, e := range entries {
		if strings.Contains(e.Content, "ghp_SECRET") {
			t.Errorf("row %d (%s) stored the unredacted credential: %q", i, e.Phase, e.Content)
		}
	}
	// The redaction is recorded, not silent about it: the text is still there,
	// marked.
	if !strings.Contains(entries[0].Content, "[redacted]") {
		t.Errorf("row 0 = %q, want the redacted form", entries[0].Content)
	}
	// The live row is NOT rewritten: the filter is the history's, and a memory
	// that is merely about a credential is still a memory.
	live, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(live) != 1 || live[0].Content != "the deploy key was rotated" {
		t.Errorf("the live row was altered by a history filter: %v %v", live, err)
	}
}

// TestUpdateMemoryBaselinesAPreV17Memory: the case migrateV17 leaves open. A
// memory that already existed when the history table arrived has NO rows in it,
// and an edit is the one write that overwrites text nothing else keeps — so the
// first edit of such a memory has to file the state it replaced, or the old
// wording is gone from the database for good and #647's as_of read has no
// version to read before the edit.
func TestUpdateMemoryBaselinesAPreV17Memory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := NewStore(db, nil)
	ctx := context.Background()
	t.Cleanup(func() { _ = db.Close() })
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// A v16 store: the memories table without the history table, stamped v16.
	const original = "the link worker re-embeds a memory whose content changed"
	if _, err := db.Exec(`DROP TABLE memory_history`); err != nil {
		t.Fatalf("drop memory_history: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories (project_id, category, content, source, importance)
		VALUES (?, 'gotcha', ?, 'mcp', 0.5)`, testProject, original); err != nil {
		t.Fatalf("seed v16 memory: %v", err)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM memories WHERE content = ?`, original).Scan(&id); err != nil {
		t.Fatalf("read seeded id: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1)); err != nil {
		t.Fatalf("stamp v16: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the migration runs, and the memory now exists with no history.
	migrated, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (migrating): %v", err)
	}
	defer func() { _ = migrated.Close() }()
	s = NewStore(migrated, nil)
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the migrated memory already has %d history rows (%v); the test needs one with none", len(entries), phasesOf(t, entries))
	}

	const rewritten = "the link worker re-embeds and re-links a memory whose content changed"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(rewritten), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	entries, err = s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"baseline:", "update:"}) {
		t.Fatalf("phases = %v, want [baseline update]", got)
	}
	if entries[0].Content != original {
		t.Errorf("the baseline records %q, want the text the memory held before the edit (%q)", entries[0].Content, original)
	}
	if entries[0].Category != "gotcha" || entries[0].Source != "mcp" {
		t.Errorf("baseline state = %s/%s, want gotcha/mcp", entries[0].Category, entries[0].Source)
	}
	if entries[1].Content != rewritten {
		t.Errorf("the update row records %q, want %q", entries[1].Content, rewritten)
	}
}

// TestUpdateMemoryBaselinesOnlyOnce: the baseline is a statement about a memory
// this build had never seen. A memory it has been watching has a real history,
// and a second baseline row in it would be a claim that the memory's first write
// happened twice.
func TestUpdateMemoryBaselinesOnlyOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a memory this build wrote", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.UpdateMemory(ctx, testProject, id, strPtr("a memory this build wrote, edited"), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory %d: %v", i, err)
		}
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "update:", "update:", "update:"}) {
		t.Errorf("phases = %v, want one save and three updates — no baseline for a memory with history", got)
	}
}

// TestHistoryTrimKeepsTheNewestRowOfAMemoryOverTheCap: the trim's floor. A memory
// past the cap keeps its newest rows, and "newest" has to mean the row that says
// what it says NOW — an audit whose last word about a live memory is a version
// fifty edits old is worse than no history at all.
func TestHistoryTrimKeepsTheNewestRowOfAMemoryOverTheCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	restore := historyVersionsPerMemory
	historyVersionsPerMemory = 3
	t.Cleanup(func() { historyVersionsPerMemory = restore })

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a memory edited past the cap", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	const last = "revision twenty"
	for i := 0; i < 20; i++ {
		text := "revision " + itoa(i)
		if i == 19 {
			text = last
		}
		if err := s.UpdateMemory(ctx, testProject, id, strPtr(text), nil, nil, nil); err != nil {
			t.Fatalf("UpdateMemory %d: %v", i, err)
		}
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != historyVersionsPerMemory {
		t.Fatalf("kept %d rows, want %d", len(entries), historyVersionsPerMemory)
	}
	newest := entries[len(entries)-1]
	if newest.Content != last {
		t.Errorf("the newest surviving row says %q, want %q — the trim took the wrong end", newest.Content, last)
	}
	if newest.Phase != "update" {
		t.Errorf("the newest surviving row is a %q, want the update that produced it", newest.Phase)
	}
}

// TestReplaceNonManualNamesTheSuccessorOfEveryReplacedRow: the chain. A rewrite
// or a merge gives the row a NEW id — the new text is not byte-identical, so the
// old row cannot be reused — which means the old id's history simply stops. A
// reader following one memory (what #648 will do with a usefulness verdict)
// would never learn that the memory it holds a verdict about is now a different
// row, so the delete row has to name it.
func TestReplaceNonManualNamesTheSuccessorOfEveryReplacedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old, _, _, err := s.Upsert(ctx, testProject, "fact", "search falls back to FTS when Ollama is down", "reflection", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// The consolidation rewrites it, declaring which input row it stands for —
	// the field the reflection operations (#659) fill in from their merge and
	// rewrite output.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:    "fact",
		Content:     "an unreachable Ollama degrades retrieval to lexical search only",
		Importance:  0.5,
		Source:      "reflection",
		ReplacesIDs: []string{old},
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("replace left %d rows, want 1: %v %v", len(all), all, err)
	}
	successor := all[0].ID
	if successor == old {
		t.Fatal("the rewrite reused the old row, so there is no successor to record and the test proves nothing")
	}

	entries, err := s.MemoryHistory(ctx, old, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(old): %v", err)
	}
	tombstone := entries[len(entries)-1]
	if tombstone.Phase != phaseDelete {
		t.Fatalf("last row for the replaced id is a %q, want a delete", tombstone.Phase)
	}
	if tombstone.RelatedID != successor {
		t.Errorf("the delete row names successor %q, want the row that now holds the content (%q)", tombstone.RelatedID, successor)
	}
	// And the successor's own history says what it is, so following the pointer
	// lands somewhere real.
	succHistory, err := s.MemoryHistory(ctx, successor, 0)
	if err != nil {
		t.Fatalf("MemoryHistory(successor): %v", err)
	}
	if len(succHistory) == 0 || succHistory[0].Content != "an unreachable Ollama degrades retrieval to lexical search only" {
		t.Errorf("following the successor landed on %v", phasesOf(t, succHistory))
	}
}

// TestReplaceNonManualLeavesAnUnrelatedDeleteWithoutASuccessor: only rows the
// caller declared were replaced get a pointer. A row this pass deleted for any
// other reason — a drop, a concurrent save it outran — has no successor, and
// naming one would be a fabricated claim about where its knowledge went.
func TestReplaceNonManualLeavesAnUnrelatedDeleteWithoutASuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	kept, _, _, err := s.Upsert(ctx, testProject, "fact", "a memory nobody mentions this pass", "reflection", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A verbatim keep survives, so something else has to be dropped: the emitted
	// memory is a different fact entirely.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:   "fact",
		Content:    "an entirely different consolidated fact about the pipeline",
		Importance: 0.5,
		Source:     "reflection",
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, kept, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	last := entries[len(entries)-1]
	if last.Phase != phaseDelete {
		t.Fatalf("last row is a %q, want a delete", last.Phase)
	}
	if last.RelatedID != "" {
		t.Errorf("a dropped row names successor %q; nothing replaced it", last.RelatedID)
	}
}

// TestUnsupersedeIsItsOwnEvent: withdrawing a claim is a change in the target's
// standing exactly as asserting one was. A history that shows a supersede and no
// withdrawal reads as though the stale claim is still live, and the superseding
// id is the half of the sentence an audit is asking for.
func TestUnsupersedeIsItsOwnEvent(t *testing.T) {
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

	if err := s.CreateLink(ctx, newer, older, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if entries[len(entries)-1].RelatedID != newer {
		t.Errorf("the supersede row names %q, want the superseding memory %q", entries[len(entries)-1].RelatedID, newer)
	}

	if _, err := s.InvalidateLink(ctx, newer, older, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	entries, err = s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, entries); !equalStrings(got, []string{"save:", "supersede:", "unsupersede:"}) {
		t.Fatalf("phases = %v, want [save supersede unsupersede]", got)
	}
	withdrawn := entries[len(entries)-1]
	if withdrawn.RelatedID != newer {
		t.Errorf("the unsupersede row names %q, want the memory whose claim was withdrawn (%q)", withdrawn.RelatedID, newer)
	}

	// A second invalidation changes nothing — the edge was already dead — so it
	// must not append a row that repeats the last one.
	if _, err := s.InvalidateLink(ctx, newer, older, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink (repeat): %v", err)
	}
	entries, err = s.MemoryHistory(ctx, older, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("a repeat invalidation appended a row: phases = %v", phasesOf(t, entries))
	}
}

// TestFoldOnlyMergeKeepsTheFoldedText: the one fold that throws the incoming
// wording away. The ordinary fold stores it as a linked copy with its own save
// row; FoldOnly returns the target and stores nothing, so without this the only
// record of a promotion's discarded paraphrase was the database itself, before
// this table existed.
func TestFoldOnlyMergeKeepsTheFoldedText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// FoldOnly folds only what foldOnlyEquivalent calls the same memory — equal
	// after normalization — because it drops one of the two texts, so the pair
	// here differs in case and in a trailing full stop, and the dropped wording
	// is the only record Ghost will have of what the caller actually sent.
	target, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"The production deploy target is Fly.io in region iad.", "mcp", 0.7, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}
	const folded = "the production deploy target is fly.io in region iad"
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", folded, "mcp", 0.7, nil, UpsertOptions{
		Provenance: Provenance{Agent: "opencode", SessionID: "ses_b"},
		FoldOnly:   true,
	}); err != nil {
		t.Fatalf("UpsertWithOptions (FoldOnly): %v", err)
	}

	entries, err := s.MemoryHistory(ctx, target, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	last := entries[len(entries)-1]
	if last.Phase != phaseMerge {
		t.Fatalf("last row is a %q, want the merge", last.Phase)
	}
	if last.MergedContent != folded {
		t.Errorf("the merge row kept %q, want the folded-in text %q", last.MergedContent, folded)
	}
	// And it is the merge row only: the default fold keeps the wording as a row
	// of its own, so a second copy there would be duplication.
	if entries[0].MergedContent != "" {
		t.Errorf("the save row carries folded text %q; the default fold has no folded text to record", entries[0].MergedContent)
	}
}

// TestAFailedWriteLeavesNoHistoryRow: the claim every other test here rests on —
// the history and the state it describes commit or roll back together. A trigger
// is the injection point: it aborts a statement mid-transaction without any
// production hook, and the assertion is that NOTHING of the write survives, the
// memory row included.
func TestAFailedWriteLeavesNoHistoryRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a fact recorded before the failure", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}

	// The history append fails, so the edit that was about to be recorded must
	// not be half-applied either.
	if _, err := s.db.Exec(`
		CREATE TRIGGER fail_history BEFORE INSERT ON memory_history
		BEGIN SELECT RAISE(ABORT, 'injected history failure'); END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	newText := "a fact that must not survive its own history failure"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(newText), nil, nil, nil); err == nil {
		t.Fatal("UpdateMemory succeeded; the injected failure did not fire, so this test proves nothing")
	}

	live, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(live) != 1 {
		t.Fatalf("the memory row is unreadable after the failure: %v %v", live, err)
	}
	if live[0].Content == newText {
		t.Error("the edit was applied even though its history row failed — the two did not roll back together")
	}
	after, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("history has %d rows after the failure, want the %d it had — a failed write left a row", len(after), len(before))
	}
}

// TestAFailedDeleteLeavesNoTombstone: the same claim for the delete path, whose
// tombstone is written BEFORE the DELETE and must roll back with it. A tombstone
// for a memory that still exists is worse than a missing one: it tells a reader
// the memory is gone.
func TestAFailedDeleteLeavesNoTombstone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.Upsert(ctx, testProject, "fact", "a fact whose delete will fail", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER fail_delete BEFORE DELETE ON memories
		BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	if err := s.Delete(ctx, id); err == nil {
		t.Fatal("Delete succeeded; the injected failure did not fire, so this test proves nothing")
	}

	live, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(live) != 1 {
		t.Fatalf("the memory is gone after a failed delete: %v %v", live, err)
	}
	after, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("history has %d rows after the failed delete, want %d — a tombstone was committed for a memory that still exists", len(after), len(before))
	}
}

// TestImportRefusesAnIDThatStillHasHistory: ids are not reused by Ghost, but a
// portable artifact carries them verbatim. A memory deleted here leaves its
// history behind — that is the feature — so importing into the same id would
// splice two unrelated memories' records under one id, with the old text under
// its save/delete pair and then an import row for text that memory never held.
// The import refuses instead, and names the operation that makes the id reusable.
func TestImportRefusesAnIDThatStillHasHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := "AAAAAAAABBBBBBBBCCCCCCCCDDDDDDDD"
	// Seeded under the id the artifact will carry, by a direct INSERT rather than
	// an Upsert and a rename: memory_provenance cascades FROM memories with no ON
	// UPDATE, so a row that carries evidence can no longer be renamed. That is
	// right — the id IS the memory, and its evidence names it — and it means a
	// fixture wanting a chosen id inserts it. The delete then frees the id while
	// its history stays.
	if _, err := s.db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source)
		 VALUES (?, ?, 'fact', 'a fact that was deleted here', 'mcp')`, id, testProject,
	); err != nil {
		t.Fatalf("seed the row to be deleted: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the deleted memory left no history; the test needs an id that is free but remembered")
	}

	_, _, _, err = s.ImportMemory(ctx, PortableMemory{
		ID:        id,
		ProjectID: testProject,
		Category:  "convention",
		Content:   "an unrelated fact from an artifact",
		Source:    "manual",
	}, ImportOptions{Apply: true, TrustProvenance: true})
	if err == nil {
		t.Fatal("the import spliced two memories' histories under one id")
	}
	if !strings.Contains(err.Error(), "ghost history purge") {
		t.Errorf("error = %q, want it to name the operation that frees the id", err)
	}
	live, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(live) != 0 {
		t.Errorf("the refused import still wrote the row: %v %v", live, err)
	}

	// And the way out works. The row is already gone, so this is the SECOND
	// stage of a redaction — the one a delete-time purge cannot cover — and it
	// still has to work, or the id stays unimportable and the text stays on disk.
	purged, err := s.PurgeMemoryHistory(ctx, id)
	if err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
	if purged != int64(len(entries)) {
		t.Errorf("purged %d rows, want the %d it had", purged, len(entries))
	}
	after, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("history rows survived: %v", phasesOf(t, after))
	}
	// A live memory is untouched by a history-only purge: "erase the history" and
	// "delete the memory" are different requests.
	other, _, _, err := s.Upsert(ctx, testProject, "fact", "an unrelated live memory", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.PurgeMemoryHistory(ctx, other); err != nil {
		t.Fatalf("PurgeMemoryHistory(live): %v", err)
	}
	if live, err := s.GetByIDs(ctx, []string{other}); err != nil || len(live) != 1 {
		t.Errorf("a history purge deleted a live memory: %v %v", live, err)
	}
}

// TestPurgeReachesTheSnapshotThatCouldRestoreTheRow: the copy that makes a
// purge's own report a lie. Every applied reflection copies each non-manual
// memory's full content into memory_snapshots, the column has no foreign key, and
// RestoreSnapshot re-inserts the row from it under the memory's ORIGINAL id — so
// a purge that left the snapshot behind erases the text from the history and
// leaves it one `ghost reflect --restore` away from being readable again, with no
// history to say it came back.
func TestPurgeReachesTheSnapshotThatCouldRestoreTheRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const secret = "the deploy key is ghp_SNAPSHOTKEY0123456789ABCDEFGHIJ"
	// preGuardRow, not Upsert — the same reason as the purge test above: the state
	// under test is a credential already on disk, which no writer will produce now.
	id := preGuardRow(t, s, Memory{
		Category: "gotcha", Content: secret, Source: "reflection", Importance: 0.5,
	})
	// A reflection pass snapshots the corpus before replacing it. The
	// replacement here is a no-op shape: the emitted text is the same memory, so
	// the row is reused and the snapshot of the pre-replace text is what stands
	// between the purge and a restore.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:   "gotcha",
		Content:    secret,
		Importance: 0.5,
		Source:     "reflection",
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	var snapshots int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE memory_id = ? AND content = ?`, id, secret,
	).Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots == 0 {
		t.Fatal("the reflection pass left no snapshot of this memory; the fixture does not test the case")
	}

	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE memory_id = ?`, id,
	).Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots after purge: %v", err)
	}
	if snapshots != 0 {
		t.Errorf("%d snapshot row(s) survive the purge and could restore the memory", snapshots)
	}
	// And nothing anywhere in this database still holds the text.
	leaked, err := countOccurrences(t, s.db, secret)
	if err != nil {
		t.Fatalf("scan for the text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the purged text survives in %d row(s) of this database", leaked)
	}
}

// TestPurgeRedactsAnotherMemoriesFoldedCopy: a FoldOnly fold records the wording
// it discarded in merged_content on the TARGET's row, so a text can sit on a
// history row that names a different memory. Deleting that row would throw away
// the event; leaving the cell would keep the text. The cell is redacted, and the
// event stays.
func TestPurgeRedactsAnotherMemoriesFoldedCopy(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A fact stored twice: first as the stored wording, then as a near-identical
	// paraphrase a FoldOnly fold throws away. The discarded paraphrase is what the
	// target's merge row keeps, and the same words are then saved as a memory of
	// their own — which is how a text ends up on a row naming one memory while a
	// second memory holds the very same string.
	const stored = "The staging database is reached over the bastion at port 2222 not 22"
	const discarded = "the staging database is reached over the bastion at port 2222 not 22"
	target, _, _, err := s.Upsert(ctx, testProject, "convention", stored, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "convention", discarded, "mcp", 0.7, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("UpsertWithOptions (FoldOnly): %v", err)
	}
	// Now save the same words for real. The default fold keeps the incoming text
	// as a linked copy of its own, so this is a second memory whose content is
	// byte-identical to the wording the merge row recorded.
	other, _, _, err := s.Upsert(ctx, testProject, "convention", discarded, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert (the same words again): %v", err)
	}
	if other == target {
		t.Fatal("the second save folded into the first; the fixture needs two memories")
	}

	// The second save also folded, and that fold records no folded-in text (the
	// incoming wording became a row of its own), so the row carrying the
	// discarded wording is not the last one. It is looked up by what it holds.
	var carried int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ? AND merged_content = ?`,
		target, phaseMerge, discarded,
	).Scan(&carried); err != nil {
		t.Fatalf("count merge rows carrying the wording: %v", err)
	}
	if carried != 1 {
		t.Fatalf("%d merge rows carry the discarded wording, want 1 — the fixture does not test the case", carried)
	}

	// Purge the SECOND memory. The target's merge row is not its own, so it
	// survives with the event and loses the text.
	if err := s.DeleteWithOptions(ctx, other, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	var phase, merged string
	if err := s.db.QueryRow(
		`SELECT phase, merged_content FROM memory_history
		 WHERE memory_id = ? AND phase = ?`, target, phaseMerge,
	).Scan(&phase, &merged); err != nil {
		t.Fatalf("read a merge row after the purge: %v", err)
	}
	if phase != phaseMerge {
		t.Errorf("the surviving event is %q, want it kept as a merge", phase)
	}
	if merged != purgedTextMarker {
		t.Errorf("a folded-in cell reads %q, want the purge marker — the text is still in the database", merged)
	}
	leaked, err := countOccurrences(t, s.db, discarded)
	if err != nil {
		t.Fatalf("scan for the text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the discarded wording survives in %d row(s)", leaked)
	}
}

// countOccurrences reports how many rows in any table hold text, across the
// tables that can hold a memory's text: the row itself, its history, and a
// snapshot. It is the assertion a redaction needs and a per-table assertion does
// not give: a purge that empties the history while a snapshot keeps the text has
// erased nothing an operator can observe.
func countOccurrences(t *testing.T, db *sql.DB, text string) (int, error) {
	t.Helper()
	var total int
	for _, q := range []struct {
		table string
		query string
	}{
		{"memories", `SELECT count(*) FROM memories WHERE content = ?`},
		{"memory_history.content", `SELECT count(*) FROM memory_history WHERE content = ?`},
		{"memory_history.merged_content", `SELECT count(*) FROM memory_history WHERE merged_content = ?`},
		{"memory_snapshots", `SELECT count(*) FROM memory_snapshots WHERE content = ?`},
		// The evidence tables, because a purge that empties the history while a
		// snapshot still holds the text has erased nothing an operator can observe —
		// and the same is true of an agent or a reference a redaction was asked to
		// remove. Scanned by their identifying fields, not by content, since they
		// hold no content.
		{"memory_provenance.source_ref", `SELECT count(*) FROM memory_provenance WHERE source_ref = ?`},
		{"memory_snapshot_evidence.source_ref", `SELECT count(*) FROM memory_snapshot_evidence WHERE source_ref = ?`},
		// The retrieval record (#646), EVERY column, matched as a literal
		// SUBSTRING rather than by equality. A struct check is a claim about this
		// build's type; this is a claim about what is on the disk, which is what a
		// purge, an export and a reader actually meet.
		//
		// Substring is the sensitivity that matters, and equality would have made
		// this list decorative: a leak that stored the question inside a longer
		// value, or a memory's content inside a verdict object, would never equal
		// the text searched for and the scan would report zero. instr rather than
		// LIKE, so a % or _ in the text is not a wildcard — a false positive here
		// is harmless (it fails a test), a false negative is the whole point.
		//
		// Every column, not just the ones the INSERT names, so a column added later
		// without thought is caught in review rather than in production.
		{"retrieval_record.project_id", `SELECT count(*) FROM retrieval_record WHERE instr(project_id, ?) > 0`},
		{"retrieval_record.source", `SELECT count(*) FROM retrieval_record WHERE instr(source, ?) > 0`},
		{"retrieval_record.session_id", `SELECT count(*) FROM retrieval_record WHERE instr(session_id, ?) > 0`},
		{"retrieval_record.query_hash", `SELECT count(*) FROM retrieval_record WHERE instr(query_hash, ?) > 0`},
		{"retrieval_record.as_of", `SELECT count(*) FROM retrieval_record WHERE instr(as_of, ?) > 0`},
		{"retrieval_record.reason", `SELECT count(*) FROM retrieval_record WHERE instr(reason, ?) > 0`},
		{"retrieval_record.verdicts", `SELECT count(*) FROM retrieval_record WHERE instr(verdicts, ?) > 0`},
		{"retrieval_record.outcome", `SELECT count(*) FROM retrieval_record WHERE instr(outcome, ?) > 0`},
		{"retrieval_record.recorded_at", `SELECT count(*) FROM retrieval_record WHERE instr(recorded_at, ?) > 0`},
	} {
		var n int
		if err := db.QueryRow(q.query, text).Scan(&n); err != nil {
			return 0, fmt.Errorf("count in %s: %w", q.table, err)
		}
		total += n
	}
	return total, nil
}

// TestRestoreAppendsOnlyForRowsItChanged: restore is repeatable by design — the
// snapshot is deliberately kept and a second run must leave the corpus alone — so
// the id set it appends cannot be re-derived after the write. By then every row
// matches the snapshot, and a row an earlier run restored looks exactly like one
// this run restores, so a second `ghost reflect --restore` appended a byte-
// identical `restore` row for every memory. Each repeat spent one of the 50
// per-memory version slots the growth policy keeps, which is how pushing the
// rollback button repeatedly pushes the real save/update/reflect versions out.
func TestRestoreAppendsOnlyForRowsItChanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the pre-snapshot wording of a fact",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create the pre-snapshot row: %v", err)
	}
	// The reflection that snapshots and replaces it.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the consolidated wording", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	restored, err := s.GetAll(ctx, testProject, 10)
	if err != nil || len(restored) != 1 {
		t.Fatalf("the first restore left %d rows, want 1: %v %v", len(restored), restored, err)
	}
	id := restored[0].ID
	afterFirst, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	// The reflection's wording differs, so the replace dropped the row and the
	// restore brought it back under its ORIGINAL id — the reinsert is what makes
	// that id live again, and its delete row is the state the restore reverted
	// from.
	if got := phasesOf(t, afterFirst); !equalStrings(got, []string{"save:", "delete:", "restore:"}) {
		t.Fatalf("phases = %v, want [save delete restore]", got)
	}

	// The second run changes nothing, so it records nothing.
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	afterSecond, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if got := phasesOf(t, afterSecond); !equalStrings(got, []string{"save:", "delete:", "restore:"}) {
		t.Errorf("phases = %v, want [save delete restore] — a repeat restore appended a row for a row it changed nothing in", got)
	}
}

// TestPurgeReachesAPreV13Snapshot: the snapshot that has no id. A pre-v13
// snapshot recorded no memory_id, and `ghost reflect --restore` matches those by
// content — so an id-keyed delete left the one row that can resurrect the memory,
// and the purge reported success on a secret one restore away.
func TestPurgeReachesAPreV13Snapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const secret = "the bastion accepts only key-based logins since the cutover"
	id, _, _, err := s.Upsert(ctx, testProject, "gotcha", secret, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A pre-v13 snapshot of that memory: every column the old table had, and no
	// memory_id.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id)
		VALUES (?, ?, 'gotcha', ?, 0.5, 'mcp', NULL)`, "snap-pre13", testProject, secret); err != nil {
		t.Fatalf("seed a pre-v13 snapshot: %v", err)
	}
	// Assert the fixture landed. This test's whole claim is that a row SURVIVED a
	// purge, and a fixture that never existed would satisfy it — which is exactly
	// what happened the first time: PurgeMemoryHistory had already removed the
	// seeded row, so the assertion below was true of a database that never had it.
	if seeded := snapshotCount(t, s, "snap-pre13"); seeded != 1 {
		t.Fatalf("the pre-v13 snapshot was not seeded (%d rows); the fixture does not test the case", seeded)
	}
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE snapshot_id = 'snap-pre13'`,
	).Scan(&left); err != nil {
		t.Fatalf("count the pre-v13 snapshot: %v", err)
	}
	if left != 0 {
		t.Errorf("the id-less snapshot survived, and ghost reflect --restore matches it by content")
	}
	leaked, err := countOccurrences(t, s.db, secret)
	if err != nil {
		t.Fatalf("scan for the text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the purged text survives in %d row(s)", leaked)
	}
}

// TestPurgeInTheDeletePathStillCollectsTheLiveText: the text a redaction has to
// erase is the text the row holds, and `DeleteWithOptions` deletes the row before
// it purges — so the collection that finds that text came back empty and the
// redaction silently ran with nothing to redact. Ordered before the DELETE for
// exactly that reason, and this is the test that says so.
func TestPurgeInTheDeletePathStillCollectsTheLiveText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A FoldOnly fold puts the wording it discarded on the target's merge row, so
	// the same words can exist as a memory of their own.
	const stored = "The cutover moved logins to key-based authentication."
	const discarded = "the cutover moved logins to key-based authentication"
	target, _, _, err := s.Upsert(ctx, testProject, "convention", stored, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "convention", discarded, "mcp", 0.7, nil,
		UpsertOptions{FoldOnly: true}); err != nil {
		t.Fatalf("UpsertWithOptions (FoldOnly): %v", err)
	}
	if _, _, _, err := s.Upsert(ctx, testProject, "convention", discarded, "mcp", 0.7, nil); err != nil {
		t.Fatalf("Upsert (the same words again): %v", err)
	}
	var carried int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ? AND merged_content = ?`,
		target, discarded,
	).Scan(&carried); err != nil {
		t.Fatalf("count merge rows carrying the wording: %v", err)
	}
	if carried != 1 {
		t.Fatalf("%d merge rows carry the wording, want 1 — the fixture does not test the case", carried)
	}

	// Find that second memory and delete it, which is the path under test.
	mems, err := s.GetByIDs(ctx, []string{firstIDWithContent(t, s, testProject, discarded)})
	if err != nil || len(mems) != 1 {
		t.Fatalf("look the second memory up: %v %v", mems, err)
	}
	other := mems[0].ID
	if other == target {
		t.Fatal("the second save folded into the first; the fixture needs two memories")
	}
	if err := s.DeleteWithOptions(ctx, other, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	leaked, err := countOccurrences(t, s.db, discarded)
	if err != nil {
		t.Fatalf("scan for the text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the text of the deleted memory survives in %d row(s): the delete path purged without collecting it", leaked)
	}
}

// snapshotCount reports how many rows a snapshot id has, so a test whose claim is
// about a row SURVIVING something can prove its fixture was there to begin with.
func snapshotCount(t *testing.T, s *Store, snapshotID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE snapshot_id = ?`, snapshotID,
	).Scan(&n); err != nil {
		t.Fatalf("count snapshots for %s: %v", snapshotID, err)
	}
	return n
}

func firstIDWithContent(t *testing.T, s *Store, projectID, content string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(
		`SELECT id FROM memories WHERE project_id = ? AND content = ? LIMIT 1`, projectID, content,
	).Scan(&id); err != nil {
		t.Fatalf("find a memory holding %q: %v", content, err)
	}
	return id
}

// TestTheBaselineRowGoesThroughTheRedactionFilter: the filter's plumbing has two
// statement shapes — the batched append and this NOT EXISTS insert — and one of
// them missing the filter is exactly how an unredacted credential lands in the
// table while the append path claims to redact it. This one caught the baseline
// insert bypassing it while the batched append did not.
//
// The memory is pre-v17 by construction here: the only way to reach the baseline
// is a row with no history, so a saved-then-edited memory would never get one.
func TestTheBaselineRowGoesThroughTheRedactionFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A pre-v17 row, straight into the table, and with no history row.
	const secret = "the deploy key is ghp_BASELINEKEY0123456789ABCDEFG"
	if _, err := s.db.Exec(`DROP TABLE memory_history`); err != nil {
		t.Fatalf("drop the history table: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO memories (project_id, category, content, source, importance) VALUES (?, 'gotcha', ?, 'mcp', 0.5)`,
		testProject, secret); err != nil {
		t.Fatalf("seed the pre-v17 memory: %v", err)
	}
	id := firstIDWithContent(t, s, testProject, secret)
	if _, err := s.db.Exec(initSQL); err != nil {
		t.Fatalf("recreate the history table: %v", err)
	}

	restore := setHistoryRedactor(func(content string) string {
		return strings.ReplaceAll(content, "ghp_BASELINEKEY", "[redacted]")
	})
	t.Cleanup(restore)

	if err := s.UpdateMemory(ctx, testProject, id, strPtr("the deploy key has been rotated"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("phases = %v, want [baseline update]", phasesOf(t, entries))
	}
	if strings.Contains(entries[0].Content, "ghp_BASELINEKEY") {
		t.Errorf("the baseline row stored the unredacted credential: %q", entries[0].Content)
	}
	if !strings.Contains(entries[0].Content, "[redacted]") {
		t.Errorf("the baseline row = %q, want the redacted form", entries[0].Content)
	}
}

// TestPurgeInTheDeletePathFindsATextNoHistoryRowHolds: why the ordering is the
// contract rather than a detail. A memory's last history row normally repeats its
// current text, so collecting texts from the history alone appears to work — and
// that is why reversing the two statements survives every other test. It stops
// working the moment a live memory's text is in no history row at all, which this
// tree can do on purpose: a history-only purge (PurgeMemoryHistory) erases the
// rows and leaves the row. Then the text lives in exactly one place, the
// memories row the DELETE is about to remove, and a redaction that collects after
// the DELETE collects nothing — and the id-less snapshot it could not have matched
// by id survives.
func TestPurgeInTheDeletePathFindsATextNoHistoryRowHolds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const secret = "rotation is announced in #ops before the key is replaced"
	id, _, _, err := s.Upsert(ctx, testProject, "convention", secret, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A pre-v13 snapshot: no memory_id, so only a content match reaches it.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id)
		VALUES (?, ?, 'convention', ?, 0.5, 'mcp', NULL)`, "snap-order", testProject, secret); err != nil {
		t.Fatalf("seed the id-less snapshot: %v", err)
	}
	// The row lives; no history row holds its text. Stripped with SQL rather than
	// through PurgeMemoryHistory, because that call would ALSO remove the id-less
	// snapshot below — the fixture would be gone before the purge under test, and
	// the test would pass against a database that never had the problem.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_history WHERE memory_id = ?`, id); err != nil {
		t.Fatalf("strip the history: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the fixture needs a live memory with no history; it has %d rows", len(entries))
	}
	if seeded := snapshotCount(t, s, "snap-order"); seeded != 1 {
		t.Fatalf("the id-less snapshot was not seeded (%d rows); the fixture does not test the case", seeded)
	}

	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{PurgeHistory: true}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}

	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE snapshot_id = 'snap-order'`,
	).Scan(&left); err != nil {
		t.Fatalf("count the snapshot: %v", err)
	}
	if left != 0 {
		t.Errorf("the id-less snapshot survived a purge whose only record of the text was the row it deleted first")
	}
	leaked, err := countOccurrences(t, s.db, secret)
	if err != nil {
		t.Fatalf("scan for the text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the purged text survives in %d row(s)", leaked)
	}
}
