package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The as_of stamps every test in this file writes its history at. They are in
// the FUTURE relative to the test run on purpose: `memories.created_at` is
// stamped by SQLite at write time and cannot be backdated through any public
// API, and the read decides "not yet created" by comparing it against the
// request's instant. A stamp in the past would therefore make every fixture
// read as a memory created after T, and the tests would be measuring the
// wrong thing.
const (
	asOfStampSave    = "2027-01-01 00:00:00"
	asOfStampRewrite = "2027-02-01 00:00:00"
	asOfStampLate    = "2027-03-01 00:00:00"
	// asOfStampFarFuture is past every stamp above, so an as_of read at it can
	// only see the newest version of everything: the state the store is in now.
	asOfStampFarFuture = "2030-01-01 00:00:00"
)

// asOfAt parses one of the stamps above into the instant a read is asked for.
func asOfAt(t *testing.T, stamp string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02 15:04:05", stamp)
	if err != nil {
		t.Fatalf("parse as_of stamp %q: %v", stamp, err)
	}
	return parsed
}

// stampHistory moves a memory's history rows onto the given instants, oldest
// row first. The append path stamps datetime('now') inside the writing
// transaction, which is second-precision, so every write a test makes shares one
// timestamp and "the state at T" cannot be placed between any two of them.
// Backdating the recorded rows is the only way to give a read two distinct
// versions to choose between, and it changes nothing else: the rows, their
// phases and their state columns are the ones the writers produced.
func stampHistory(t *testing.T, s *Store, memoryID string, stamps ...string) {
	t.Helper()
	if _, err := s.MemoryHistory(context.Background(), memoryID, 0); err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	for i, stamp := range stamps {
		res, err := s.db.Exec(`
			UPDATE memory_history SET recorded_at = ?
			WHERE rowid = (SELECT rowid FROM memory_history WHERE memory_id = ?
			               ORDER BY rowid LIMIT 1 OFFSET ?)`, stamp, memoryID, i)
		if err != nil {
			t.Fatalf("stamp history row %d of %s: %v", i, memoryID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("stamping history row %d of %s touched %d rows, want 1 (the fixture and the stamps disagree)", i, memoryID, n)
		}
	}
}

// dropHistory erases a memory's recorded history without touching the row,
// which is the state a memory written before schema v17 is in: live, with no
// version of itself recorded anywhere. PurgeMemoryHistory is the tree's own path
// to that state, so a fixture built with it is the real shape rather than a
// hand-built approximation of it.
func dropHistory(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	if _, err := s.PurgeMemoryHistory(context.Background(), memoryID); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}
}

// asOfContentByID indexes a read's rows for an assertion that names a memory.
func asOfContentByID(t *testing.T, set *AsOfSet) map[string]AsOfRow {
	t.Helper()
	out := make(map[string]AsOfRow, len(set.Rows))
	for _, r := range set.Rows {
		out[r.ID] = r
	}
	return out
}

// TestMemoriesAsOfReadsTheVersionAtOrBeforeT: an edit is the case the whole
// table exists for. The row holds only the newest text, so before this read
// existed there was no query that could return the older wording; the version
// row is what makes "what did Ghost believe then" answerable, and it has to
// answer with the text AND the category that version carried, because a
// relabelled memory is a different memory to every filter that reads category.
func TestMemoriesAsOfReadsTheVersionAtOrBeforeT(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const first = "the link worker re-embeds a memory whose content changed"
	const second = "the link worker re-embeds and re-links a memory whose content changed"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", first, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}
	category := "dependency"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(second), &category, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite)

	atSave, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, asOfStampSave))
	if err != nil {
		t.Fatalf("MemoriesAsOf at the save: %v", err)
	}
	row, ok := asOfContentByID(t, atSave)[id]
	if !ok {
		t.Fatalf("memory %s is absent at the save instant, want the version it was saved with", id)
	}
	if row.Content != first {
		t.Errorf("content at the save = %q, want %q", row.Content, first)
	}
	if row.Category != "gotcha" {
		t.Errorf("category at the save = %q, want gotcha (the category the version carried, not the current one)", row.Category)
	}

	atRewrite, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, asOfStampRewrite))
	if err != nil {
		t.Fatalf("MemoriesAsOf at the rewrite: %v", err)
	}
	row = asOfContentByID(t, atRewrite)[id]
	if row.Content != second {
		t.Errorf("content at the rewrite = %q, want %q", row.Content, second)
	}
	if row.Category != "dependency" {
		t.Errorf("category at the rewrite = %q, want dependency", row.Category)
	}

	// The instant BETWEEN the two writes is the assertion that matters: a read
	// that only ever returned the current row would pass the two reads above by
	// accident if the "before" instant were taken to be the first write's.
	mid, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, "2027-01-15 00:00:00"))
	if err != nil {
		t.Fatalf("MemoriesAsOf between the writes: %v", err)
	}
	row = asOfContentByID(t, mid)[id]
	if row.Content != first {
		t.Errorf("content between the writes = %q, want the older wording %q", row.Content, first)
	}
	if row.Category != "gotcha" {
		t.Errorf("category between the writes = %q, want gotcha", row.Category)
	}
}

// TestMemoriesAsOfDropsATombstoneAndAMemoryCreatedLater: liveness at T is two
// different facts. A delete leaves a tombstone naming the text the row held, so
// a read at T after the delete must drop the memory — a delete that merely
// removed the row would make a historical read answer "yes, Ghost knew that"
// about something it has never known. A memory that did not exist yet must be
// dropped the same way, and silently: it is not an unknown, it is a memory the
// store had not written.
func TestMemoriesAsOfDropsATombstoneAndAMemoryCreatedLater(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const kept = "a memory that outlives the read"
	keptID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", kept, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the kept memory: %v", err)
	}
	const doomed = "a memory deleted before the read"
	doomedID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", doomed, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the doomed memory: %v", err)
	}
	if err := s.DeleteWithOptions(ctx, doomedID, DeleteOptions{}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}
	const later = "a memory written after the read"
	laterID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", later, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the later memory: %v", err)
	}
	stampHistory(t, s, keptID, asOfStampSave)
	stampHistory(t, s, doomedID, asOfStampSave, asOfStampRewrite)
	stampHistory(t, s, laterID, asOfStampLate)

	rows := asOfContentByID(t, mustAsOf(t, s, asOfStampRewrite))
	if _, ok := rows[keptID]; !ok {
		t.Errorf("memory %s is absent at %s, want it live", keptID, asOfStampRewrite)
	}
	if _, ok := rows[doomedID]; ok {
		t.Errorf("memory %s is present at %s: its tombstone is recorded before the read, so it was already gone", doomedID, asOfStampRewrite)
	}
	if _, ok := rows[laterID]; ok {
		t.Errorf("memory %s is present at %s, want it dropped as not yet created", laterID, asOfStampRewrite)
	}
	// ...and the deleted memory IS there before its tombstone, which is what
	// makes the drop above a fact about the store rather than an absence of one.
	earlier := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))
	if row, ok := earlier[doomedID]; !ok || row.Content != doomed {
		t.Errorf("at %s the deleted memory reads %+v, want it live with %q", asOfStampSave, row, doomed)
	}
	if _, ok := earlier[laterID]; ok {
		t.Errorf("memory %s is present at %s, want it dropped as not yet created", laterID, asOfStampSave)
	}
}

// TestMemoriesAsOfTakesLivenessFromTheResolveSequence: resolved_at on the row
// is a single value, and the question a historical read asks is a sequence
// question — was it resolved at T, and if so had it been resolved back before
// that. A memory resolved and then unreleased is live again, and only the
// sequence says so.
func TestMemoriesAsOfTakesLivenessFromTheResolveSequence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", "a resolve pass judged this stale", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	if _, err := s.ClearResolved(ctx, testProject, []string{id}); err != nil {
		t.Fatalf("ClearResolved: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite, asOfStampLate)

	resolved := asOfContentByID(t, mustAsOf(t, s, asOfStampRewrite))[id]
	if resolved.ResolvedAt == nil {
		t.Error("at the resolve instant the memory reads live, want it resolved (a resolve pass had stamped it)")
	}
	live := asOfContentByID(t, mustAsOf(t, s, asOfStampLate))[id]
	if live.ResolvedAt != nil {
		t.Errorf("at the unresolve instant the memory reads resolved at %q, want it live again", *live.ResolvedAt)
	}
	unsaved := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))[id]
	if unsaved.ResolvedAt != nil {
		t.Errorf("at the save instant the memory reads resolved at %q, want it live", *unsaved.ResolvedAt)
	}
}

// TestMemoriesAsOfTakesTheLiveSupersedeClaimFromHistory: a supersede row records
// a claim about a memory rather than a change to it, so the newest version row
// says nothing about whether the claim was live at T — and a memory can be
// superseded, withdrawn and superseded again, so only the sequence of
// supersede/unsupersede rows says which claim stood. An unsupersede must win
// over the supersede before it, or a withdrawn claim would survive in a
// historical read forever.
func TestMemoriesAsOfTakesTheLiveSupersedeClaimFromHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The two texts share no tokens on purpose. A save that folds into an
	// existing near-duplicate merges rather than inserting, which would put a
	// merge row in the middle of the sequence this test is about.
	oldID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "helm upgrade --install blocks until the CRD is established", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the old claim: %v", err)
	}
	newID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "WAL growth on a long-lived session is bounded by the checkpoint interval", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the replacement: %v", err)
	}
	if newID == oldID {
		t.Fatal("the second save folded into the first, so this fixture has one memory rather than a pair")
	}
	if err := s.CreateLink(ctx, newID, oldID, "supersedes", 1.0, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if _, err := s.InvalidateLink(ctx, newID, oldID, "supersedes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	// The edge is live again, so the claim is live again.
	if err := s.CreateLink(ctx, newID, oldID, "supersedes", 1.0, "auto"); err != nil {
		t.Fatalf("CreateLink (again): %v", err)
	}
	// oldID's rows in order: save, supersede, unsupersede, supersede.
	stampHistory(t, s, oldID, asOfStampSave, asOfStampRewrite, asOfStampLate, asOfStampFarFuture)

	claimed := asOfContentByID(t, mustAsOf(t, s, asOfStampRewrite))[oldID]
	if claimed.SupersededBy != newID {
		t.Errorf("at the supersede instant SupersededBy = %q, want %q", claimed.SupersededBy, newID)
	}
	withdrawn := asOfContentByID(t, mustAsOf(t, s, asOfStampLate))[oldID]
	if withdrawn.SupersededBy != "" {
		t.Errorf("at the unsupersede instant SupersededBy = %q, want it withdrawn", withdrawn.SupersededBy)
	}
	relive := asOfContentByID(t, mustAsOf(t, s, asOfStampFarFuture))[oldID]
	if relive.SupersededBy != newID {
		t.Errorf("at the second supersede instant SupersededBy = %q, want %q", relive.SupersededBy, newID)
	}
}

// TestMemoriesAsOfReportsAPreV17MemoryAsUnknown: migrateV17 backfills nothing, so
// a memory that predates the table has no version of itself recorded anywhere.
// The honest answer for an instant before its first recorded version is that
// Ghost does not know what it said then — and the alternative, returning the
// current text, is a guess presented as a record. It must be reported as its
// own fact rather than dropped silently, because a reader who cannot see the
// gap will read the shorter set as the whole truth.
func TestMemoriesAsOfReportsAPreV17MemoryAsUnknown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	known, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a memory this build wrote", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the known memory: %v", err)
	}
	legacy, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", "a memory from before the history table", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the legacy memory: %v", err)
	}
	stampHistory(t, s, known, asOfStampSave)
	dropHistory(t, s, legacy)

	set := mustAsOf(t, s, asOfStampSave)
	rows := asOfContentByID(t, set)
	if _, ok := rows[known]; !ok {
		t.Errorf("memory %s is absent, want its recorded version", known)
	}
	if row, ok := rows[legacy]; ok {
		t.Errorf("memory %s is in the set with content %q, want it reported as unknown rather than guessed", legacy, row.Content)
	}
	if len(set.Unknown) != 1 || set.Unknown[0].ID != legacy {
		t.Fatalf("Unknown = %+v, want exactly the pre-v17 memory %s reported", set.Unknown, legacy)
	}
	if set.Unknown[0].Content != "" {
		t.Errorf("the unknown row carries content %q, want no content at all: the store recorded none for that instant", set.Unknown[0].Content)
	}
	if !strings.Contains(set.UnknownNote(), "before its first recorded version") {
		t.Errorf("the unknown note = %q, want it to name the gap a reader has to know about", set.UnknownNote())
	}

	// The gap is a property of the INSTANT, not of the memory: once the memory
	// has a recorded version, the read after it is complete and the note goes
	// away. A disclosure that outlived its fact would train a reader to ignore
	// it. The edit is what files it, and it is the writers' baseline row that
	// does the filing — a pre-v17 memory's first write records what it held
	// before that write destroyed the text.
	if err := s.UpdateMemory(ctx, testProject, legacy, strPtr("a memory from before the history table, edited"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	stampHistory(t, s, legacy, asOfStampSave, asOfStampRewrite)
	later := mustAsOf(t, s, asOfStampRewrite)
	if len(later.Unknown) != 0 {
		t.Errorf("Unknown at %s = %+v, want none: the memory has a recorded version by then", asOfStampRewrite, later.Unknown)
	}
	if later.UnknownNote() != "" {
		t.Errorf("the unknown note at %s = %q, want none", asOfStampRewrite, later.UnknownNote())
	}
	rows = asOfContentByID(t, later)
	if row, ok := rows[legacy]; !ok || row.Content != "a memory from before the history table, edited" {
		t.Errorf("the legacy memory at %s reads %+v, want the edited text", asOfStampRewrite, row)
	}
}

// TestMemoriesAsOfInTheFutureEqualsNow: the read must not invent a state. An
// instant past every recorded write is the state the store is in now, so the
// historical set and the current set have to be the same set — a divergence is
// the signature of a read that reconstructs rather than replays.
func TestMemoriesAsOfInTheFutureEqualsNow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", "a memory edited twice", "mcp", 0.6, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, id, strPtr("a memory edited once"), nil, nil, nil); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, id, strPtr("a memory edited twice, again"), nil, nil, nil); err != nil {
		t.Fatalf("second update: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite, asOfStampLate)

	current, err := s.GetByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(current) != 1 {
		t.Fatalf("GetByIDs returned %d rows, want 1", len(current))
	}
	row := asOfContentByID(t, mustAsOf(t, s, asOfStampFarFuture))[id]
	if row.Content != current[0].Content {
		t.Errorf("an as_of past every write reads %q, want the current text %q", row.Content, current[0].Content)
	}
	if row.Category != current[0].Category || row.Importance != float32(current[0].Importance) {
		t.Errorf("an as_of past every write reads category %q importance %v, want %q / %v",
			row.Category, row.Importance, current[0].Category, current[0].Importance)
	}
}

// TestMemoriesAsOfScopesOnTheProjectTheVersionRowRecords: which project a memory
// belonged to at T is read from the version row, not from the live row. The
// version row is the one place the change log says anything about the bucket at
// all, and a memory moved between projects after T was in the other one at T — so
// reading the live project_id would place a memory in a bucket it was not in at
// the instant asked about, which is the class of error a historical read exists
// to avoid.
func TestMemoriesAsOfScopesOnTheProjectTheVersionRowRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, "", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const other = "other-project"
	if err := s.EnsureProject(ctx, other, "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject(%s): %v", other, err)
	}

	mine, _, _, err := s.UpsertWithProvenance(ctx, testProject, "preference", "prefers tabs over spaces in this repository", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the project's memory: %v", err)
	}
	theirs, _, _, err := s.UpsertWithProvenance(ctx, other, "preference", "the deploy checklist ends with a smoke test", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the other project's memory: %v", err)
	}
	global, _, _, err := s.UpsertWithProvenance(ctx, GlobalProjectID, "convention", "commits are signed", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save the global memory: %v", err)
	}
	stampHistory(t, s, mine, asOfStampSave)
	stampHistory(t, s, theirs, asOfStampSave)
	stampHistory(t, s, global, asOfStampSave)

	// A project read is the project's own rows plus _global, which is what every
	// project read has always been, and not another project's.
	rows := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))
	if _, ok := rows[mine]; !ok {
		t.Errorf("a project read at %s does not find %s, want it: its version row records the project", asOfStampSave, mine)
	}
	if _, ok := rows[global]; !ok {
		t.Errorf("a project read at %s does not find the _global memory, want it: a project read admits _global", asOfStampSave)
	}
	if _, ok := rows[theirs]; ok {
		t.Errorf("a project read at %s finds another project's memory %s", asOfStampSave, theirs)
	}
	// A _global read is _global alone, and the version row's project is what
	// puts a memory in it.
	globals, err := ReadMemoriesAsOf(ctx, s.db, GlobalOnly, GlobalProjectID, asOfAt(t, asOfStampSave))
	if err != nil {
		t.Fatalf("ReadMemoriesAsOf(_global): %v", err)
	}
	globalRows := asOfContentByID(t, globals)
	if _, ok := globalRows[global]; !ok {
		t.Errorf("a _global read at %s does not find the global memory", asOfStampSave)
	}
	for _, id := range []string{mine, theirs} {
		if _, ok := globalRows[id]; ok {
			t.Errorf("a _global read at %s finds project memory %s", asOfStampSave, id)
		}
	}
}

// TestMemoriesAsOfReportsAPromotionInTheRecordedPast: a promotion REWRITES the
// history rows' project_id, because the project-delete cascade would otherwise
// take a memory's recorded past with it when the project it left is deleted. So
// the recorded past of a promoted memory names _global for every version it
// ever had, and a read at an instant BEFORE the promotion reports it there.
//
// This test exists to pin that imprecision rather than to argue for it. The
// honest alternatives were to report the live project_id — which puts a memory
// in the bucket it holds NOW, the exact error a historical read must not make —
// or to reconstruct a project the database no longer holds. What the read does
// is report the record as it stands, and say in its doc that this is why.
func TestMemoriesAsOfReportsAPromotionInTheRecordedPast(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, "", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	promoted, _, _, err := s.UpsertWithProvenance(ctx, testProject, "preference", "the review gate is green before a merge is opened", "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, testProject, promoted); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, promoted, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the promoted memory has %d history rows, want 1: a promotion rewrites the rows rather than appending", len(entries))
	}
	if entries[0].ProjectID != GlobalProjectID {
		t.Errorf("the save row records project %q, want the promotion's %q", entries[0].ProjectID, GlobalProjectID)
	}
	stampHistory(t, s, promoted, asOfStampSave)
	if row := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))[promoted]; row.ProjectID != GlobalProjectID {
		t.Errorf("a read before the promotion reports project %q, want the recorded %q", row.ProjectID, GlobalProjectID)
	}
}

// TestMemoriesAsOfFindsAMemoryTheStoreHasSinceDeleted: the driving table has to
// be the history, not the live rows. A consolidation's delete takes the row and
// leaves a tombstone, and a read that started from `memories` would find nothing
// at all for a memory the store held for months — the answer would be an empty
// set that reads as "Ghost never knew this", which is the opposite of the truth.
func TestMemoriesAsOfFindsAMemoryTheStoreHasSinceDeleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const rewritten = "helm upgrade --install blocks until the CRD is established"
	const replacement = "a long-lived session grows its WAL file until the next checkpoint truncates it"
	oldID, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", rewritten, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// Save, then the rewrite's tombstone. Both are stamped, because the second
	// row is the fact the whole test turns on: a read that only ever saw the
	// save row would report the rewritten memory as live forever.
	stampHistory(t, s, oldID, asOfStampSave)

	// A rewrite: the new text is not byte-identical, so the replace deletes the
	// old row (recording the tombstone and the successor) and inserts a new id.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "gotcha", Content: replacement, Source: "mcp", Importance: 0.5,
	}}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	newID := theOnlyMemoryID(t, s, testProject)
	if newID == oldID {
		t.Fatalf("the replace reused the id %s, so this fixture is not a rewrite", oldID)
	}
	stampHistory(t, s, newID, asOfStampRewrite)
	stampHistory(t, s, oldID, asOfStampSave, asOfStampRewrite)

	// Before the rewrite the old id is the live memory and carries the old text.
	before := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))
	row, ok := before[oldID]
	if !ok {
		t.Fatalf("at %s the pre-rewrite memory is absent, want it live", asOfStampSave)
	}
	if row.Content != rewritten {
		t.Errorf("at %s the pre-rewrite memory reads %q, want %q", asOfStampSave, row.Content, rewritten)
	}
	if _, ok := before[newID]; ok {
		t.Errorf("at %s the rewrite's successor is already present, want it not yet created", asOfStampSave)
	}

	// After it, the old id is a tombstone and the successor holds the new text.
	after := asOfContentByID(t, mustAsOf(t, s, asOfStampRewrite))
	if _, ok := after[oldID]; ok {
		t.Errorf("at %s the rewritten row is still in the set, want it dropped as tombstoned", asOfStampRewrite)
	}
	row, ok = after[newID]
	if !ok {
		t.Fatalf("at %s the rewrite's successor is absent, want it live", asOfStampRewrite)
	}
	if row.Content != replacement {
		t.Errorf("at %s the successor reads %q, want %q", asOfStampRewrite, row.Content, replacement)
	}
}

// TestMemoriesAsOfReportsAMemoryWhoseFirstVersionIsAfterT: a memory can be live,
// predate the history table, and still have a recorded version — and every one of
// those versions can be dated after T. It is then in neither half of the read: the
// version set needs a row at or before T, and the gap half asked only whether the
// memory had history AT ALL, so "yes" answered it and the memory fell out of both.
//
// The gap is the common case, not an edge case. recordBaselineHistoryTx runs only
// from UpdateMemory, and every other first write on a pre-v17 memory appends a row
// dated to that write — so a resolve pass, a supersede, a delete or a reflection
// rewrite shortly after the upgrade is enough. A memory that vanishes from the
// answer with nothing said is worse than one reported as a gap: the reader has no
// way to tell a short set from a complete one.
//
// It is a reflection REWRITE rather than a reuse, and that is #727's doing: a
// verbatim re-emission writes nothing, so a pre-v17 memory the lifecycle only ever
// carries through keeps no version and stays in Unknown for as long as nothing
// changes it. That is the honest answer — the store cannot say what it said at T
// because nothing ever wrote a version of it, and the content it does report comes
// from the live row, which the re-emission left alone. It is still a change from
// before #727, where such a memory acquired a version purely by being looked at,
// and it is the one cost this trades against the flood.
func TestMemoriesAsOfReportsAMemoryWhoseFirstVersionIsAfterT(t *testing.T) {
	const content = "a memory written before the history table existed"

	// Each case performs ONE first write on a memory that has no history, which is
	// the shape a pre-v17 memory has when the lifecycle touches it after the
	// upgrade.
	for _, tc := range []struct {
		name  string
		first func(t *testing.T, s *Store, id string)
	}{
		{name: "resolve", first: func(t *testing.T, s *Store, id string) {
			if _, err := s.SetResolved(context.Background(), []string{id}); err != nil {
				t.Fatalf("SetResolved: %v", err)
			}
		}},
		{name: "unresolve", first: func(t *testing.T, s *Store, id string) {
			if _, err := s.SetResolved(context.Background(), []string{id}); err != nil {
				t.Fatalf("SetResolved: %v", err)
			}
			if _, err := s.ClearResolved(context.Background(), testProject, []string{id}); err != nil {
				t.Fatalf("ClearResolved: %v", err)
			}
		}},
		{name: "delete", first: func(t *testing.T, s *Store, id string) {
			if err := s.DeleteWithOptions(context.Background(), id, DeleteOptions{}); err != nil {
				t.Fatalf("DeleteWithOptions: %v", err)
			}
		}},
		{name: "supersede", first: func(t *testing.T, s *Store, id string) {
			partner := mustSave(t, s, "the memory whose edge claims it")
			if err := s.CreateLink(context.Background(), partner, id, "supersedes", 1.0, "auto"); err != nil {
				t.Fatalf("CreateLink: %v", err)
			}
		}},
		{name: "unsupersede", first: func(t *testing.T, s *Store, id string) {
			partner := mustSave(t, s, "the memory whose edge claims it")
			if err := s.CreateLink(context.Background(), partner, id, "supersedes", 1.0, "auto"); err != nil {
				t.Fatalf("CreateLink: %v", err)
			}
			if _, err := s.InvalidateLink(context.Background(), partner, id, "supersedes"); err != nil {
				t.Fatalf("InvalidateLink: %v", err)
			}
		}},
		{name: "reflect rewrite", first: func(t *testing.T, s *Store, id string) {
			if _, err := s.ReplaceNonManual(context.Background(), testProject, []Memory{{
				Category: "fact", Content: content + ", restated", Source: "mcp", Importance: 0.5,
			}}, ""); err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			id := mustSave(t, s, content)
			dropHistory(t, s, id)
			tc.first(t, s, id)
			entries, err := s.MemoryHistory(context.Background(), id, 0)
			if err != nil {
				t.Fatalf("MemoryHistory: %v", err)
			}
			if len(entries) == 0 {
				t.Fatalf("the first write recorded nothing, so this fixture is not the case under test")
			}
			stamps := make([]string, len(entries))
			for i := range stamps {
				stamps[i] = asOfStampLate
			}
			stampHistory(t, s, id, stamps...)

			set := mustAsOf(t, s, asOfStampSave)
			if _, ok := asOfContentByID(t, set)[id]; ok {
				t.Errorf("the memory reads as known at %s, want it reported unknown: its only version is dated %s", asOfStampSave, asOfStampLate)
			}
			found := false
			for _, row := range set.Unknown {
				if row.ID == id {
					found = true
				}
			}
			if !found {
				t.Fatalf("the memory is in neither the set (%d rows) nor the gap (%d rows), so it VANISHED from a read it belongs to:\n%s",
					len(set.Rows), len(set.Unknown), set.UnknownNote())
			}
		})
	}
}

// TestLegacyFirstWriteRecordsTheStateItReplaced: why no writer has to file a
// baseline on these paths. Every write that touches a pre-v17 memory records the
// state it read out of the memories row in the SAME transaction, so the text a
// first write is about to stop holding is in the history either way — which is
// why recordBaselineHistoryTx is needed only on UpdateMemory, the one write that
// overwrites the text in place. A writer added to these paths later does not need
// a baseline, and this test is what says so.
func TestLegacyFirstWriteRecordsTheStateItReplaced(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "the retention sweep runs at four in the morning"

	id := mustSave(t, s, content)
	dropHistory(t, s, id)
	if _, err := s.SetResolved(ctx, []string{id}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the resolve recorded %d rows, want 1", len(entries))
	}
	if entries[0].Content != content {
		t.Errorf("the resolve row reads %q, want the text it replaced (%q): a first write that did not record the state it read would need a baseline instead",
			entries[0].Content, content)
	}
}

// TestMemoriesAsOfMeasuresAgeFromTheVersionWhenTheRowIsGone: a delete takes the
// row and leaves the tombstone, so every current column is NULL — and an
// unreadable created_at reads as ANCIENT. A memory deleted yesterday was therefore
// pushed to the decay floor in every listing covering the past year, on the
// strength of a column that says nothing. The version's own timestamp is a fact
// about the past by construction: it is at or before T by definition.
func TestMemoriesAsOfMeasuresAgeFromTheVersionWhenTheRowIsGone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "a memory that is deleted after the read"
	id := mustSave(t, s, content)
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite)

	row, ok := asOfContentByID(t, mustAsOf(t, s, asOfStampSave))[id]
	if !ok {
		t.Fatalf("at %s the memory is absent, want it live with %q", asOfStampSave, content)
	}
	if row.CreatedAt == "" {
		t.Fatalf("CreatedAt is empty for a tombstoned row, so its age reads as ancient and its decay is at the floor")
	}
	if got, want := row.CreatedAt, asOfStampSave; got != want {
		t.Errorf("CreatedAt = %q, want the version's own timestamp %q: the row is gone, so the current column cannot say when the memory existed", got, want)
	}
}

// TestAsOfCreatedAtFallsBackWhenTheColumnCannotAnswer pins the rule itself, for
// the case no current writer produces: a created_at LATER than T. ageDays clamps a
// negative age at 0, so a future-dated column is the single most favourable value a
// row can carry into a past ranking, and it has to be refused rather than believed.
func TestAsOfCreatedAtFallsBackWhenTheColumnCannotAnswer(t *testing.T) {
	at := asOfAt(t, asOfStampRewrite)
	for _, tc := range []struct {
		name      string
		createdAt string
		version   string
		want      string
	}{
		{name: "usable column", createdAt: asOfStampSave, version: asOfStampRewrite, want: asOfStampSave},
		{name: "unreadable column", createdAt: "", version: asOfStampSave, want: asOfStampSave},
		{name: "unparseable column", createdAt: "not a timestamp", version: asOfStampSave, want: asOfStampSave},
		{name: "column later than the instant", createdAt: asOfStampLate, version: asOfStampSave, want: asOfStampSave},
		{name: "no version either", createdAt: "", version: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := asOfCreatedAt(tc.createdAt, tc.version, at); got != tc.want {
				t.Errorf("asOfCreatedAt(%q, %q) = %q, want %q", tc.createdAt, tc.version, got, tc.want)
			}
		})
	}
}

// TestMemoriesAsOfCountsARestoredMemoryOnce: "tombstoned" does not imply "not
// live". A snapshot restore reinstates the row under the id it recorded, and
// appends its own `restore` row, so a memory can be live now and still carry the
// delete that took it — which put it in BOTH halves of the gap read, and one
// memory was disclosed as two. The count is not cosmetic: it reaches
// CandidateSet.Unrecorded and every surface's "N memories are unknown" sentence.
func TestMemoriesAsOfCountsARestoredMemoryOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "a memory deleted and then restored under its own id"

	id := mustSave(t, s, content)
	dropHistory(t, s, id)
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}
	// A snapshot of the memory as it stood before the delete, which is what a
	// restore replays. The restore is the real path on purpose: it is the only
	// writer that brings a deleted id back, and the point of the test is that
	// "has a delete row" and "is live" are different questions.
	if _, err := s.db.Exec(`
		INSERT INTO memory_snapshots
		    (snapshot_id, project_id, category, content, importance, source, created_at, memory_id, scope_captured)
		VALUES ('snap1', ?, 'fact', ?, 0.5, 'mcp', ?, ?, 1)`,
		testProject, content, asOfStampSave, id); err != nil {
		t.Fatalf("insert the snapshot: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the restore recorded no history for the reinstated id, so this fixture is not the case under test")
	}
	stamps := make([]string, len(entries))
	for i := range stamps {
		stamps[i] = asOfStampLate
	}
	stampHistory(t, s, id, stamps...)

	set := mustAsOf(t, s, asOfStampSave)
	times := 0
	for _, row := range set.Unknown {
		if row.ID == id {
			times++
		}
	}
	if times != 1 {
		t.Errorf("the restored memory is reported as a gap %d times, want 1 — a live row and its own tombstone are the same memory", times)
	}
	if ids := set.UnknownIDs(); len(ids) != len(set.Unknown) {
		t.Errorf("UnknownIDs returns %v for %d unknown rows, want one id per row", ids, len(set.Unknown))
	}
	// The set must still be right for the memory's other half: with every version
	// dated after T it is a gap, not a known row.
	if _, ok := asOfContentByID(t, set)[id]; ok {
		t.Errorf("the memory reads as known at %s, want a reported gap: every one of its versions is dated %s", asOfStampSave, asOfStampLate)
	}
}

// TestMemoriesAsOfCountsAMemoryWithTwoTombstonesOnce: the id test above makes the
// two halves disjoint; it does not make the tombstone half one row per memory. A
// memory can hold more than one delete row — delete, restore under the same id,
// delete again — and every one of them satisfies the half's two tests, so without a
// ranking the same id was appended to Unknown once per tombstone.
func TestMemoriesAsOfCountsAMemoryWithTwoTombstonesOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "a memory deleted, restored, and deleted again"

	id := mustSave(t, s, content)
	dropHistory(t, s, id)
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{}); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO memory_snapshots
		    (snapshot_id, project_id, category, content, importance, source, created_at, memory_id, scope_captured)
		VALUES ('snap2', ?, 'fact', ?, 0.5, 'mcp', ?, ?, 1)`,
		testProject, content, asOfStampSave, id); err != nil {
		t.Fatalf("insert the snapshot: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// The id is live again, so the second delete is a second tombstone rather than
	// a no-op — which is what makes this shape reachable at all.
	if err := s.DeleteWithOptions(ctx, id, DeleteOptions{}); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	entries, err := s.MemoryHistory(ctx, id, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	tombstones := 0
	for _, e := range entries {
		if e.Phase == phaseDelete {
			tombstones++
		}
	}
	if tombstones < 2 {
		t.Fatalf("the fixture holds %d tombstone(s), want 2: this is the case under test", tombstones)
	}
	stamps := make([]string, len(entries))
	for i := range stamps {
		stamps[i] = asOfStampLate
	}
	stampHistory(t, s, id, stamps...)

	set := mustAsOf(t, s, asOfStampSave)
	times := 0
	for _, row := range set.Unknown {
		if row.ID == id {
			times++
		}
	}
	if times != 1 {
		t.Errorf("a memory with %d tombstones is reported as a gap %d times, want 1", tombstones, times)
	}
}

// mustSave is the fixture's save, returning the id.
func mustSave(t *testing.T, s *Store, content string) string {
	t.Helper()
	id, _, _, err := s.UpsertWithProvenance(context.Background(), testProject, "fact", content, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}
	return id
}

// mustAsOf fails the test rather than returning an unusable set, so every
// assertion below reads the set it asked for instead of re-checking the error.
func mustAsOf(t *testing.T, s *Store, stamp string) *AsOfSet {
	t.Helper()
	set, err := s.MemoriesAsOf(context.Background(), testProject, asOfAt(t, stamp))
	if err != nil {
		t.Fatalf("MemoriesAsOf at %s: %v", stamp, err)
	}
	return set
}

// theOnlyMemoryID is the fixture's way of naming the row a replace left behind.
func theOnlyMemoryID(t *testing.T, s *Store, projectID string) string {
	t.Helper()
	all, err := s.GetAll(context.Background(), projectID, 10)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("the project holds %d memories, want 1", len(all))
	}
	return all[0].ID
}

// asOfStampSaveOrLater is a compile-time reminder that the stamps above are the
// only ones these fixtures may use: they are all in the future relative to a test
// run, because created_at is stamped by SQLite at write time and cannot be
// backdated through any public API.
var _ = []string{asOfStampSave, asOfStampRewrite, asOfStampLate, asOfStampFarFuture}

// TestMemoriesAsOfOnAStoreBehindTheTierColumnStillReads: the historical reader
// runs on handles that cannot migrate. `ghost context --as-of` opens the store
// with memory.OpenReadDB — read-only, refusing to create — so a store written by
// a Ghost from before schema v19 is a store this statement must still run
// against. A column named in it fails the WHOLE read, and the consequence is
// not one missing field: `ghost context --as-of` prints the header it built and
// then no context at all, for a question the user asked about their own past.
//
// The tier was named here for a while, read from the live row the way tags, pin
// and scope are, and removed: memory_history records the state a memory HELD and
// a tier is not part of that state, so the only value available is the one the
// row carries NOW. Nothing consumed it either — the as_of decay passes
// RetentionProject, and assemble.validateRequest refuses a tier filter over an
// as_of read — so a version gate would have been machinery guarding a value
// nobody reads. See AsOfRow.Retention.
func TestMemoriesAsOfOnAStoreBehindTheTierColumnStillReads(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const first = "the link worker re-embeds a memory whose content changed"
	const second = "the link worker re-embeds and re-links a memory whose content changed"
	id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "gotcha", first, "mcp", 0.5, nil, Provenance{})
	if err != nil {
		t.Fatalf("UpsertWithProvenance: %v", err)
	}
	category := "dependency"
	if err := s.UpdateMemory(ctx, testProject, id, strPtr(second), &category, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	stampHistory(t, s, id, asOfStampSave, asOfStampRewrite)

	// The control, read BEFORE the columns go: this is the answer the read owes.
	want, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, asOfStampSave))
	if err != nil {
		t.Fatalf("MemoriesAsOf control: %v", err)
	}
	if _, ok := asOfContentByID(t, want)[id]; !ok {
		t.Fatalf("the control read does not contain %s, so the comparison below proves nothing", id)
	}

	// Back to v18: the corpus, the history and the category all stay, because a
	// fixture that dropped the rows too would be testing an empty store.
	if _, err := s.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_memories_session_expiry`); err != nil {
		t.Fatalf("drop the v19 index: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE memories DROP COLUMN retention`); err != nil {
		t.Fatalf("drop the tier column: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE memories DROP COLUMN expires_at`); err != nil {
		t.Fatalf("drop the expiry column: %v", err)
	}

	got, err := s.MemoriesAsOf(ctx, testProject, asOfAt(t, asOfStampSave))
	if err != nil {
		t.Fatalf("MemoriesAsOf on a store behind the tier column: %v", err)
	}
	row, ok := asOfContentByID(t, got)[id]
	if !ok {
		t.Fatalf("memory %s is absent from a historical read on a v18 store: the read answered empty", id)
	}
	if row.Content != first || row.Category != "gotcha" {
		t.Errorf("the version read on a v18 store = (%q, %q), want (%q, gotcha): the text a version carried", row.Content, row.Category, first)
	}
	// And the tier is empty here, deliberately: the change log holds no tier, so
	// there is no historical value for this field to carry.
	if row.Retention != "" {
		t.Errorf("AsOfRow.Retention = %q on a historical row, want empty: no version ever recorded a tier", row.Retention)
	}
}
