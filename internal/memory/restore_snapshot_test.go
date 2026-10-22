package memory

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRestoreKeepsSavesMadeAfterSnapshot is the data-loss defect.
//
// RestoreSnapshot deleted every non-manual row in the project, without the
// consolidatedSince guard ReplaceNonManual has — so an `mcp` save written
// minutes after the snapshot was taken, which the snapshot never saw and
// cannot bring back, was permanently destroyed by a restore. A user saving a
// fact and then restoring would lose it silently, with no undo: the row is
// not in the snapshot, so it is simply gone.
func TestRestoreKeepsSavesMadeAfterSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	// Takes a snapshot of the project, then replaces it.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// Saved AFTER the snapshot: it is not in it and never can be.
	laterID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "saved after the snapshot was taken",
		Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("create post-snapshot: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range all {
		if m.ID == laterID {
			return // survived
		}
	}
	t.Errorf("a save made after the snapshot was destroyed by the restore; it is not in the snapshot, so it cannot be brought back (id %s)", laterID)
}

// TestRestorePreservesIdentityProvenanceAndCount: the snapshot did not carry
// the original id, created_at, access_count or v10 provenance, so every
// restored row came back as a brand-new memory. That reset decay, and because
// memory_embeddings and memory_links both reference memories(id) ON DELETE
// CASCADE, the delete-then-reinsert destroyed the embedding and the whole
// link graph around the row even though its text came back looking fine.
func TestRestorePreservesIdentityProvenanceAndCount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	agent := "opencode"
	session := "sess-abc"
	ref := "abc123"
	conf := 0.42
	origID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "a memory with history worth keeping",
		Source: "reflection", Importance: 0.5,
		Agent: agent, SessionID: session, SourceRef: ref, Confidence: &conf,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Give it decay history: access_count and last_accessed both feed decay,
	// and a restore that resets them makes an old memory look freshly written.
	if err := s.Touch(ctx, []string{origID}); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := s.Touch(ctx, []string{origID}); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	before, err := s.GetByIDs(ctx, []string{origID})
	if err != nil || len(before) != 1 {
		t.Fatalf("GetByIDs before: err=%v n=%d", err, len(before))
	}
	orig := before[0]
	if orig.AccessCount < 2 {
		t.Fatalf("precondition: access_count = %d, want >= 2", orig.AccessCount)
	}

	// Emit different content so the original row is really deleted, not
	// reused in place — that is the path that lost identity.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "gotcha", Content: "entirely different consolidated content", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	restored, err := s.GetByIDs(ctx, []string{origID})
	if err != nil {
		t.Fatalf("GetByIDs after: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored row is missing its original id %s — a fresh id silently severs embeddings and links (got %d rows)", origID, len(restored))
	}
	got := restored[0]

	if got.CreatedAt != orig.CreatedAt {
		t.Errorf("created_at = %s, want %s — a reset timestamp makes decay treat an old memory as new", got.CreatedAt, orig.CreatedAt)
	}
	if got.AccessCount != orig.AccessCount {
		t.Errorf("access_count = %d, want %d — decay history was discarded", got.AccessCount, orig.AccessCount)
	}
	if got.Agent != agent {
		t.Errorf("agent = %q, want %q — write-time provenance was not carried through the snapshot", got.Agent, agent)
	}
	if got.SessionID != session {
		t.Errorf("session_id = %q, want %q", got.SessionID, session)
	}
	if got.SourceRef != ref {
		t.Errorf("source_ref = %q, want %q", got.SourceRef, ref)
	}
	if got.Confidence == nil || *got.Confidence != conf {
		t.Errorf("confidence = %v, want %v", got.Confidence, conf)
	}
	if got.Content != orig.Content {
		t.Errorf("content = %q, want %q", got.Content, orig.Content)
	}
}

// TestRestoreDoesNotConsumeTheSnapshot: a restore used to delete the snapshot
// it read, so a second attempt failed with "no snapshots found" — and with
// only three kept, a handful of reflections spent the entire history. Because
// the restore is now expressed as update-in-place plus insert-what-is-missing,
// running it again yields the same result instead of a different one.
func TestRestoreDoesNotConsumeTheSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	first, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("first RestoreSnapshot: %v", err)
	}
	if first.Restored == 0 {
		t.Fatalf("first restore reported no work done")
	}
	state := func() string {
		t.Helper()
		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		var b strings.Builder
		for _, m := range all {
			b.WriteString(m.ID)
			b.WriteString("|")
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
		return b.String()
	}
	afterFirst := state()

	second, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("second RestoreSnapshot failed — the restore consumed the snapshot it read: %v", err)
	}
	// The count is expected to differ: the first run removes the replace's
	// output and re-inserts the old row, the second only rewrites what is
	// already correct. What must not differ is the resulting corpus — that is
	// what "repeatable" has to mean, or a second run could silently drift.
	if got := state(); got != afterFirst {
		t.Errorf("second restore changed the corpus:\nafter first:  %s\nafter second: %s", afterFirst, got)
	}
	if second.Restored != 0 {
		t.Errorf("second restore reported %d; it changed nothing, so it must report none", second.Restored)
	}
}

// TestSnapshotRetentionIsWidened: three snapshots meant three reflections of
// history, and each restore burned one. Retention now keeps ten.
func TestSnapshotRetentionIsWidened(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		if _, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "round of reflection content",
			Source: "reflection", Importance: 0.5,
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: "consolidated content", Importance: 0.6},
		}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual %d: %v", i, err)
		}
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = ?`,
		testProject,
	).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if n != rounds {
		t.Errorf("distinct snapshots = %d, want %d — retention still trims history (or lost some)", n, rounds)
	}
}

// TestMigrateV13AddsSnapshotColumns: the widened columns must exist on both a
// fresh database and an upgraded one, and an already-captured snapshot must
// keep NULL memory_id rather than a fabricated one — the ids were never
// recorded, so inventing them would attach restored memories to whatever row
// happened to collide.
func TestMigrateV13AddsSnapshotColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE memory_snapshots DROP COLUMN memory_id`); err != nil {
		t.Skipf("this SQLite build cannot drop a column to simulate v12: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/a-very-long-v12-path', 'p1')`,
		`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source)
		  VALUES ('p1-100', 'p1', 'fact', 'captured before v13', 0.5, 'reflection')`,
		`PRAGMA user_version = 12`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrate(db, 12); err != nil {
		t.Fatalf("migrate v12->v13: %v", err)
	}

	for _, col := range []string{"memory_id", "access_count", "last_accessed", "agent",
		"session_id", "source_ref", "confidence", "valid_from", "valid_until", "verified_at"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('memory_snapshots') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("memory_snapshots.%s missing after migrateV13", col)
		}
	}

	var memoryID sql.NullString
	var content string
	if err := db.QueryRow(`SELECT content, memory_id FROM memory_snapshots WHERE snapshot_id = 'p1-100'`).
		Scan(&content, &memoryID); err != nil {
		t.Fatalf("read migrated snapshot: %v", err)
	}
	if content != "captured before v13" {
		t.Errorf("content changed: %q", content)
	}
	if memoryID.Valid {
		t.Errorf("memory_id = %v, want NULL — the original id was never recorded, so it cannot be recovered", memoryID.String)
	}
}

// TestMigrateFreshDBHasSnapshotColumns is the fresh-database twin: initSQL
// must create the widened table directly, or a new install would depend on
// running migrations it never needs.
func TestMigrateFreshDBHasSnapshotColumns(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, col := range []string{"memory_id", "access_count", "agent", "confidence"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('memory_snapshots') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("memory_snapshots.%s missing on a fresh database", col)
		}
	}
}

// TestRestoreInvalidatesTheEmbeddingOfRevertedContent is #1085.
//
// RestoreSnapshot rewrote a reverted row's CONTENT in place but left the
// embedding and link-scan rows of the text it had just reverted away. The
// row then scored for bytes it no longer held — semantic search returned the
// restored row at cosine 1.000 for a query built from the superseded text —
// and UnembeddedMemoryIDs never re-queued it, because it still had a vector
// under the configured identity: the row stayed wrong until the embedding
// model changed. The restore now deletes both rows in the same transaction
// it writes content in, the rule UpdateMemoryWithOptions applies to an edit.
// A row whose content the restore leaves alone keeps both, and so does a row
// whose METADATA the restore reverts — the delete is scoped to the content
// difference alone, never the UPDATE's four-column difference.
func TestRestoreInvalidatesTheEmbeddingOfRevertedContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Three reflection rows the replace below re-emits verbatim, so all of
	// them keep their ids, their embeddings and their link scans, and all
	// three land in the snapshot.
	alpha, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "alpha content", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	psi, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "psi content", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create psi: %v", err)
	}
	chi, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "chi content", Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create chi: %v", err)
	}
	for _, id := range []string{alpha, psi, chi} {
		if err := s.StoreEmbedding(ctx, id, []float32{1, 0, 0}, "test"); err != nil {
			t.Fatalf("embed %s: %v", id, err)
		}
	}

	// A verbatim replace takes the snapshot. Nothing about the three rows
	// changes, so restoration comes back to exactly this state.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "alpha content", Importance: 0.5},
		{Category: "fact", Content: "psi content", Importance: 0.5},
		{Category: "fact", Content: "chi content", Importance: 0.5},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// Alpha is edited to the text the restore will revert, and re-embedded
	// under that text — the stale-vector state the bug leaves behind.
	if err := s.UpdateMemoryWithOptions(ctx, testProject, alpha, UpdateOptions{
		Content: strPtr("beta content"),
	}); err != nil {
		t.Fatalf("update alpha: %v", err)
	}
	if err := s.StoreEmbedding(ctx, alpha, []float32{0, 1, 0}, "test"); err != nil {
		t.Fatalf("re-embed alpha: %v", err)
	}
	// Chi is edited in METADATA ONLY — its category diverges from the
	// snapshot while its content stays the same, so the restore rewrites chi
	// without reverting any text.
	if err := s.UpdateMemoryWithOptions(ctx, testProject, chi, UpdateOptions{
		Category: strPtr("pattern"),
	}); err != nil {
		t.Fatalf("update chi: %v", err)
	}

	// Every row holds a link-scan slot describing the vectors above. The
	// restore must clear alpha's — its slot described the reverted text —
	// and leave psi's and chi's exactly where they were.
	for _, id := range []string{alpha, psi, chi} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO link_scans (memory_id) VALUES (?)`, id); err != nil {
			t.Fatalf("seed link scan %s: %v", id, err)
		}
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	got, err := s.GetByIDs(ctx, []string{alpha, chi})
	if err != nil || len(got) != 2 {
		t.Fatalf("GetByIDs after restore: err=%v n=%d", err, len(got))
	}
	var alphaGot, chiGot Memory
	for _, m := range got {
		if m.ID == alpha {
			alphaGot = m
		}
		if m.ID == chi {
			chiGot = m
		}
	}
	if alphaGot.Content != "alpha content" {
		t.Fatalf("restored content = %q, want %q", alphaGot.Content, "alpha content")
	}
	if chiGot.Category != "fact" {
		t.Fatalf("restored category = %q, want %q (the metadata revert did land)", chiGot.Category, "fact")
	}

	// The vector of the reverted-away text must be gone, not stale: the row
	// is queued for a re-embed, and a semantic query for the beta text no
	// longer scores the restored row at 1.000.
	unembedded, err := s.UnembeddedMemoryIDs(ctx, testProject, "test", 10)
	if err != nil {
		t.Fatalf("UnembeddedMemoryIDs: %v", err)
	}
	found := false
	for _, id := range unembedded {
		if id == alpha {
			found = true
		}
	}
	if !found {
		t.Errorf("restored row %s is not queued for a re-embed: UnembeddedMemoryIDs=%v", alpha, unembedded)
	}
	results, err := s.SearchVector(ctx, testProject, []float32{0, 1, 0}, 10)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	for _, r := range results {
		if r.MemoryID == alpha {
			t.Errorf("SearchVector(beta) returned the restored row %s at score %f (content %q)",
				r.MemoryID, r.Score, alphaGot.Content)
		}
	}

	// Its own content never changed, so psi keeps its vector across the
	// restore — only reverted text re-queues a re-embed.
	for _, id := range []string{psi, chi} {
		vec, err := s.GetEmbedding(ctx, id)
		if err != nil {
			t.Fatalf("GetEmbedding(%s): %v", id, err)
		}
		if vec == nil {
			t.Errorf("row %s whose content the restore left alone lost its embedding", id)
		}
	}

	// The link-scan slots follow the same rule as the embeddings: alpha's
	// is dropped with the text it described, psi's and chi's survive — psi
	// because nothing about it changed, chi because a metadata-only revert
	// restores no text and must not re-queue a re-link it does not need.
	linkScanCount := func(id string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM link_scans WHERE memory_id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("count link_scans for %s: %v", id, err)
		}
		return n
	}
	if n := linkScanCount(alpha); n != 0 {
		t.Errorf("reverted row %s kept its link scan (1 row, want 0)", alpha)
	}
	if n := linkScanCount(psi); n != 1 {
		t.Errorf("unchanged row %s lost its link scan (%d rows, want 1)", psi, n)
	}
	if n := linkScanCount(chi); n != 1 {
		t.Errorf("metadata-only-reverted row %s lost its link scan (%d rows, want 1)", chi, n)
	}
}

// TestRestoreLegacySnapshotMatchesByContent covers snapshots written before
// schema v13, whose memory_id is NULL and cannot be recovered.
//
// They restore by content instead: that can only add a row that is missing
// and never overwrites a live one, so the property that matters is that a
// second restore does not duplicate what the first just wrote.
func TestRestoreLegacySnapshotMatchesByContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A pre-v13 snapshot: everything recorded except the original id.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_snapshots
		    (snapshot_id, project_id, category, content, importance, source, tags, memory_id)
		VALUES (?, ?, 'fact', 'legacy snapshot content', 0.5, 'reflection', '[]', NULL)
	`, testProject+"-legacy", testProject); err != nil {
		t.Fatalf("seed legacy snapshot: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM memories WHERE project_id = ? AND content = ?`,
			testProject, "legacy snapshot content").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("legacy snapshot content restored %d times, want 1", count())
	}

	// Restoring again must not duplicate it: with no id to key on, an
	// unconditional insert would stack a copy on every run.
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("after a second restore the legacy row appears %d times, want 1 — content match is not idempotent", n)
	}
}

// TestRestorePicksTheSnapshotInsideItsWriteTransaction is the cross-process
// race: RestoreSnapshot read the latest snapshot id on the pool and only then
// waited for the write lock, so a reflection another handle committed while the
// restore waited in BEGIN IMMEDIATE was serialised BEFORE the restore and still
// skipped by it. The restore applied the older snapshot, and a memory saved
// between the two reflections and consumed by the second was in neither the
// live corpus nor the snapshot it used — gone, with the restore reporting
// success.
//
// Handle b holds the write lock with the second reflection written but not
// committed; handle a's restore is started and observed holding its one
// connection (waiting for the lock, after the old code's pool read); then b
// commits. A restore that picks its snapshot inside its own transaction sees
// b's snapshot, so the consumed memory comes back and b's output goes.
func TestRestorePicksTheSnapshotInsideItsWriteTransaction(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ghost.db")
	open := func() *Store {
		db, err := OpenDB(path)
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	a, b := open(), open()
	live := func(content string) bool {
		t.Helper()
		var n int
		if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE project_id = ? AND content = ?`, testProject, content).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n > 0
	}

	if err := a.EnsureProject(ctx, testProject, "/src/"+testProject, testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, _, _, err := a.Upsert(ctx, testProject, "fact", "alpha deploys run on tuesdays only", "mcp", 0.6, nil); err != nil {
		t.Fatalf("save X: %v", err)
	}
	if _, _, _, _, err := a.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "reflect one output about deploy cadence", Importance: 0.6}}, "", nil); err != nil {
		t.Fatalf("reflect1: %v", err)
	}
	const mContent = "bravo the staging cluster needs a manual cert rotation"
	if _, _, _, err := a.Upsert(ctx, testProject, "gotcha", mContent, "mcp", 0.7, nil); err != nil {
		t.Fatalf("save M: %v", err)
	}
	time.Sleep(5 * time.Millisecond) // a distinct UnixNano snapshot id
	const reflect2Out = "reflect two output merging everything"

	// b takes the write lock (BEGIN IMMEDIATE) and writes reflection 2 in it.
	txB, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin b: %v", err)
	}
	defer txB.Rollback() //nolint:errcheck
	if _, _, _, _, err := b.ReplaceNonManual(withStoreTx(ctx, txB), testProject, []Memory{{Category: "fact", Content: reflect2Out, Importance: 0.6}}, "", nil); err != nil {
		t.Fatalf("reflect2 in tx: %v", err)
	}

	done := make(chan error, 1)
	go func() { _, err := a.RestoreSnapshot(ctx, testProject); done <- err }()

	// a is one connection: once the restore holds it continuously, the
	// restore is in BEGIN waiting for b's lock (the old code's snapshot read
	// on the pool is a single fast query that releases it before that).
	deadline := time.Now().Add(5 * time.Second)
	held := time.Time{}
	for {
		if a.db.Stats().InUse == 1 {
			if held.IsZero() {
				held = time.Now()
			} else if time.Since(held) > 100*time.Millisecond {
				break
			}
		} else {
			held = time.Time{}
		}
		select {
		case err := <-done:
			t.Fatalf("restore finished while b held the write lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("restore never started waiting for the write lock")
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := txB.Commit(); err != nil {
		t.Fatalf("commit b: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !live(mContent) {
		t.Errorf("memory saved between the two reflections is gone after the restore: the restore applied the snapshot taken before reflection 1, not the one reflection 2 committed while it waited")
	}
	if live(reflect2Out) {
		t.Errorf("reflection 2's output survived the restore: it was serialised before the restore and should have been undone")
	}
}

// TestRestoreCountsOnlyRowsItChanged: Restored is what the operator is told, so
// it must count rows the restore removed, rewrote or re-inserted — not rows it
// matched and found already equal to the snapshot. A repeat run changes no
// column and appends no history row, so it has to report zero.
func TestRestoreCountsOnlyRowsItChanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, c := range []string{"first merged source fact", "second merged source fact", "verbatim kept fact", "dropped fact"} {
		if _, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: c, Source: "reflection", Importance: 0.5,
		}); err != nil {
			t.Fatalf("create %q: %v", c, err)
		}
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "first and second merged into one", Importance: 0.6},
		{Category: "fact", Content: "verbatim kept fact", Importance: 0.5},
		{Category: "fact", Content: "brand new fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	historyRows := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_history`).Scan(&n); err != nil {
			t.Fatalf("count history: %v", err)
		}
		return n
	}

	first, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("first RestoreSnapshot: %v", err)
	}
	// Two removed (merge output, new row) + three re-inserted (both merge
	// sources, dropped row); the verbatim row matched and changed nothing.
	if first.Restored != 5 {
		t.Errorf("first restore reported %d, want 5 (2 removed + 3 re-inserted; the unchanged verbatim row is not a change)", first.Restored)
	}

	h := historyRows()
	second, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("second RestoreSnapshot: %v", err)
	}
	if second.Restored != 0 {
		t.Errorf("second restore reported %d, want 0: it changed no column of any memory", second.Restored)
	}
	if got := historyRows(); got != h {
		t.Errorf("second restore appended %d history rows, want 0", got-h)
	}
}

// TestRestoreCountsATagsOnlyRevert: the history gate compares four recorded
// columns, but the restore rewrites more than those. A row whose only
// difference from the snapshot is its tags was changed by the restore, so the
// count includes it, while the history (which records no tags-only change)
// stays untouched.
func TestRestoreCountsATagsOnlyRevert(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact whose tags get edited", Source: "reflection",
		Importance: 0.5, Tags: []string{"a"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact whose tags get edited", Importance: 0.5, Tags: []string{"a"}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET tags = '["zzz"]' WHERE id = ?`, id); err != nil {
		t.Fatalf("edit tags: %v", err)
	}
	var h int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_history`).Scan(&h); err != nil {
		t.Fatalf("count history: %v", err)
	}

	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if res.Restored != 1 {
		t.Errorf("restore reported %d, want 1: it put the tags back", res.Restored)
	}
	var tags string
	if err := s.db.QueryRowContext(ctx, `SELECT tags FROM memories WHERE id = ?`, id).Scan(&tags); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if tags != `["a"]` {
		t.Fatalf("tags = %s, want [\"a\"]: the restore did not revert them", tags)
	}
	var h2 int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_history`).Scan(&h2); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if h2 != h {
		t.Errorf("a tags-only revert appended %d history rows, want 0", h2-h)
	}

	again, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("second RestoreSnapshot: %v", err)
	}
	if again.Restored != 0 {
		t.Errorf("second restore reported %d, want 0", again.Restored)
	}
}
