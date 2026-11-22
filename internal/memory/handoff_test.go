package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveHandoffStoresAnExpiringRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	h := SessionHandoff{
		ProjectID:          testProject,
		SessionID:          "s1",
		TranscriptPath:     "/t/x.jsonl",
		TranscriptPosition: 12,
		Content:            "task: finish the relay\nquestions: port?\nfiles: a.go\npointer: s1 @ 12",
	}
	if err := s.SaveHandoff(ctx, h); err != nil {
		t.Fatalf("SaveHandoff: %v", err)
	}

	got, ok, err := s.HandoffForSession(ctx, testProject, "s1", time.Now())
	if err != nil {
		t.Fatalf("HandoffForSession: %v", err)
	}
	if !ok {
		t.Fatal("no handoff stored")
	}
	if got.SessionID != "s1" || got.TranscriptPath != "/t/x.jsonl" || got.TranscriptPosition != 12 {
		t.Errorf("pointer wrong: %+v", got)
	}
	if got.Content != h.Content {
		t.Errorf("content = %q, want %q", got.Content, h.Content)
	}
	if got.ExpiresAt == nil {
		t.Fatal("a session-retention handoff has no expiry")
	}
	exp, err := time.Parse(StoredStampLayout, *got.ExpiresAt)
	if err != nil {
		t.Fatalf("parse expiry %q: %v", *got.ExpiresAt, err)
	}
	if before, after := time.Now().Add(SessionTTL-time.Minute), time.Now().Add(SessionTTL+time.Minute); exp.Before(before) || exp.After(after) {
		t.Errorf("expiry = %v, want about %v after the save", exp, before.Add(time.Minute))
	}
}

func TestHandoffForSessionIsPerSessionAndUnexpired(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "old", Content: "old handoff"}); err != nil {
		t.Fatalf("save old: %v", err)
	}
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "new", Content: "new handoff"}); err != nil {
		t.Fatalf("save new: %v", err)
	}
	if err := s.EnsureProject(ctx, "other", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject other: %v", err)
	}
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: "other", SessionID: "elsewhere", Content: "other project"}); err != nil {
		t.Fatalf("save other: %v", err)
	}
	// Expire one by hand: an expired handoff is not a handoff a compacting
	// session should be handed.
	if _, err := s.db.ExecContext(ctx, `UPDATE session_handoff SET expires_at = ? WHERE session_id = 'new'`,
		time.Now().Add(-time.Hour).UTC().Format(StoredStampLayout)); err != nil {
		t.Fatalf("expire new: %v", err)
	}

	got, ok, err := s.HandoffForSession(ctx, testProject, "old", time.Now())
	if err != nil {
		t.Fatalf("HandoffForSession old: %v", err)
	}
	if !ok || got.SessionID != "old" || got.Content != "old handoff" {
		t.Errorf("HandoffForSession(old) = %+v,%v, want its own handoff", got, ok)
	}
	if _, ok, err := s.HandoffForSession(ctx, testProject, "new", time.Now()); err != nil || ok {
		t.Errorf("HandoffForSession(new) = %v,%v, want none for an expired handoff", ok, err)
	}
	if _, ok, err := s.HandoffForSession(ctx, testProject, "elsewhere", time.Now()); err != nil || ok {
		t.Errorf("HandoffForSession(elsewhere) = %v,%v, want none: another project's session", ok, err)
	}
}

func TestHandoffIsCascadedWithItsProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "s1", Content: "x"}); err != nil {
		t.Fatalf("SaveHandoff: %v", err)
	}
	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM session_handoff`).Scan(&n); err != nil {
		t.Fatalf("count handoffs: %v", err)
	}
	if n != 0 {
		t.Errorf("%d handoff rows survived their project's delete", n)
	}
}

func TestMigrateV32MatchesTheFreshDatabaseSchema(t *testing.T) {
	newDB := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.Exec(initSQL); err != nil {
			t.Fatalf("initSQL: %v", err)
		}
		return db
	}
	fresh, migrated := newDB(t), newDB(t)
	if _, err := migrated.Exec(`DROP TABLE session_handoff`); err != nil {
		t.Fatalf("drop the table so the step has work to do: %v", err)
	}
	tx, err := migrated.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV32(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migrateV32: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	freshCols, err := columnShapes(t, fresh, "session_handoff")
	if err != nil || len(freshCols) == 0 {
		t.Fatalf("a fresh database has no session_handoff (%v, %v)", freshCols, err)
	}
	migratedCols, err := columnShapes(t, migrated, "session_handoff")
	if err != nil {
		t.Fatalf("read migrated table: %v", err)
	}
	if diff := columnShapeDiff(freshCols, migratedCols); diff != "" {
		t.Errorf("the migration path and initSQL disagree on the table: %s", diff)
	}
}

func TestHandoffForSessionPrefersTheExactSession(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "other", Content: "other session's task"}); err != nil {
		t.Fatalf("save other: %v", err)
	}
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "mine", Content: "my task"}); err != nil {
		t.Fatalf("save mine: %v", err)
	}
	got, ok, err := s.HandoffForSession(ctx, testProject, "mine", time.Now())
	if err != nil || !ok || got.SessionID != "mine" || got.Content != "my task" {
		t.Errorf("HandoffForSession(mine) = %+v,%v,%v, want my task", got, ok, err)
	}
	if _, ok, err := s.HandoffForSession(ctx, testProject, "absent", time.Now()); err != nil || ok {
		t.Errorf("an absent session got a handoff: %v,%v", ok, err)
	}
}

func TestSaveHandoffSweepsExpiredRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "stale", Content: "expired task"}); err != nil {
		t.Fatalf("save stale: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE session_handoff SET expires_at = ? WHERE session_id = 'stale'`,
		time.Now().Add(-time.Hour).UTC().Format(StoredStampLayout)); err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if err := s.SaveHandoff(ctx, SessionHandoff{ProjectID: testProject, SessionID: "fresh", Content: "fresh task"}); err != nil {
		t.Fatalf("save fresh: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM session_handoff WHERE session_id = 'stale'`).Scan(&n); err != nil {
		t.Fatalf("count stale: %v", err)
	}
	if n != 0 {
		t.Errorf("%d expired handoff rows survived the next save", n)
	}
}

// TestMigrateFromV31CreatesTheHandoffTableAndStampsV32 drives the real migrate()
// chain from a database left at the previous version, rather than replaying the
// single step against a fresh schema: the upgrade path is what users take.
func TestMigrateFromV31CreatesTheHandoffTableAndStampsV32(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	// Strip the v32 object and stamp the store as v31, the shape a user's
	// database has before this build opens it.
	if _, err := db.Exec(`DROP TABLE session_handoff`); err != nil {
		t.Fatalf("drop the table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 31`); err != nil {
		t.Fatalf("stamp v31: %v", err)
	}
	if err := migrate(db, 31); err != nil {
		t.Fatalf("migrate from 31: %v", err)
	}
	if v, err := dbUserVersion(db); err != nil || v != schemaVersion {
		t.Errorf("user_version after migrate = %d,%v, want %d", v, err, schemaVersion)
	}
	cols, err := columnShapes(t, db, "session_handoff")
	if err != nil || len(cols) == 0 {
		t.Fatalf("the v31 -> v32 migrate did not create session_handoff (%v, %v)", cols, err)
	}
}
