package memory

import (
	"context"
	"database/sql"
	"fmt"
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
	if purged.Rows != int64(len(entries)) {
		t.Errorf("purged %d rows, want the %d it had", purged.Rows, len(entries))
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
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category:   "gotcha",
		Content:    secret,
		Importance: 0.5,
		Source:     "reflection",
	}}, "", nil); err != nil {
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

// TestPurgeRedactsFoldedWordingCopiedOntoAnotherHistoryRow: the wording a
// FoldOnly fold discards lives in merged_content, and the same folded-in text
// can sit on a second memory's history row. When neither memory holds that
// wording as content, the purge used to collect nothing that could reach the
// other row: the texts to redact came only from the purged memory's content
// columns, so its own folded-in wording was never among them. The purged
// memory's merged_content is the wording the purge was asked to erase, and
// reading it is what reaches the copy another memory kept.
func TestPurgeRedactsFoldedWordingCopiedOntoAnotherHistoryRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The wording lives in merged_content alone, the shape a FoldOnly fold from
	// before the token guard leaves, so it is written straight into the table
	// and neither memory's own content holds it.
	const folded = "the pool database rotates its password on the first monday of each quarter"
	m1, _, _, err := s.Upsert(ctx, testProject, "convention",
		"a fact whose own text is not the folded wording", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert(m1): %v", err)
	}
	m2, _, _, err := s.Upsert(ctx, testProject, "convention",
		"a second fact carrying its own separate wording", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert(m2): %v", err)
	}
	for _, id := range []string{m1, m2} {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE memory_history SET merged_content = ? WHERE memory_id = ?`,
			folded, id); err != nil {
			t.Fatalf("seed the folded wording on %s: %v", id, err)
		}
	}
	// Assert the fixture landed: this test's claim is about a row the purge has
	// to reach, and one that never existed would satisfy the assertion below.
	var seeded int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE merged_content = ?`, folded,
	).Scan(&seeded); err != nil {
		t.Fatalf("count the seeded folded wording: %v", err)
	}
	if seeded != 2 {
		t.Fatalf("%d history rows carry the folded wording, want 2 — the fixture does not test the case", seeded)
	}

	if _, err := s.PurgeMemoryHistory(ctx, m1); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	// The other memory's event stays; only its text was asked for.
	var kept int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ?`, m2,
	).Scan(&kept); err != nil {
		t.Fatalf("count the other memory's history rows: %v", err)
	}
	if kept == 0 {
		t.Errorf("the purge removed the other memory's history row instead of redacting its cell")
	}
	leaked, err := countOccurrences(t, s.db, folded)
	if err != nil {
		t.Fatalf("scan for the folded wording: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the folded-in wording survives in %d row(s) after the purge", leaked)
	}
}

// TestPurgeKeepsAnotherMemoriesSnapshotWhoseContentIsTheFoldedWording: the
// wording a FoldOnly fold discarded is redacted in another memory's
// merged_content cell, but that reach must not widen the snapshot delete. The
// content list also keys the snapshot delete, and a snapshot is the rollback
// point for a whole project, so a live memory whose own content happens to
// equal the purged memory's folded-in wording must keep its snapshot.
func TestPurgeKeepsAnotherMemoriesSnapshotWhoseContentIsTheFoldedWording(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const folded = "the pool database rotates its access phrase on the first monday of each quarter"
	m1, _, _, err := s.Upsert(ctx, testProject, "convention",
		"a fact whose own text is not the folded wording", "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert(m1): %v", err)
	}
	// m2's own content IS the folded wording, and it carries a snapshot of
	// itself — the rollback point a purge of m1 must not touch.
	m2, _, _, err := s.Upsert(ctx, testProject, "convention", folded, "mcp", 0.7, nil)
	if err != nil {
		t.Fatalf("Upsert(m2): %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memory_history SET merged_content = ? WHERE memory_id = ?`,
		folded, m1); err != nil {
		t.Fatalf("seed the folded wording on m1: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, memory_id, category, content, importance, source)
		VALUES ('snap-m2', ?, ?, 'convention', ?, 0.7, 'mcp')`,
		testProject, m2, folded); err != nil {
		t.Fatalf("seed m2's snapshot: %v", err)
	}
	var seeded int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE memory_id = ?`, m2,
	).Scan(&seeded); err != nil {
		t.Fatalf("count m2's snapshots: %v", err)
	}
	if seeded != 1 {
		t.Fatalf("%d snapshots for m2, want 1 — the fixture does not test the case", seeded)
	}

	if _, err := s.PurgeMemoryHistory(ctx, m1); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_snapshots WHERE memory_id = ?`, m2,
	).Scan(&left); err != nil {
		t.Fatalf("count m2's snapshots after the purge: %v", err)
	}
	if left != 1 {
		t.Errorf("the purge removed an unrelated memory's snapshot (%d left, want 1); the folded-in wording must not key the snapshot delete", left)
	}
	// The other memory's history cell is still redacted.
	var leaked int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE merged_content = ?`, folded,
	).Scan(&leaked); err != nil {
		t.Fatalf("count the surviving folded wording: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the folded-in wording survives in %d history row(s)", leaked)
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
