package memory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreKeepsSavesMadeAfterSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

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
	if got := state(); got != afterFirst {
		t.Errorf("second restore changed the corpus:\nafter first:  %s\nafter second: %s", afterFirst, got)
	}
	if second.Restored != 0 {
		t.Errorf("second restore reported %d; it changed nothing, so it must report none", second.Restored)
	}
}

func TestRestoreInvalidatesTheEmbeddingOfRevertedContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "alpha content", Importance: 0.5},
		{Category: "fact", Content: "psi content", Importance: 0.5},
		{Category: "fact", Content: "chi content", Importance: 0.5},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if err := s.UpdateMemoryWithOptions(ctx, testProject, alpha, UpdateOptions{
		Content: strPtr("beta content"),
	}); err != nil {
		t.Fatalf("update alpha: %v", err)
	}
	if err := s.StoreEmbedding(ctx, alpha, []float32{0, 1, 0}, "test"); err != nil {
		t.Fatalf("re-embed alpha: %v", err)
	}
	if err := s.UpdateMemoryWithOptions(ctx, testProject, chi, UpdateOptions{
		Category: strPtr("pattern"),
	}); err != nil {
		t.Fatalf("update chi: %v", err)
	}

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

	for _, id := range []string{psi, chi} {
		vec, err := s.GetEmbedding(ctx, id)
		if err != nil {
			t.Fatalf("GetEmbedding(%s): %v", id, err)
		}
		if vec == nil {
			t.Errorf("row %s whose content the restore left alone lost its embedding", id)
		}
	}

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

func TestRestoreLegacySnapshotMatchesByContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

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

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("after a second restore the legacy row appears %d times, want 1 — content match is not idempotent", n)
	}
}

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
