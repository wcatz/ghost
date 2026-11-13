package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// readGhostState reads the mergeable slice of a project's ghost_state row.
// A missing row reads as the zero value, which is what a project that never
// reflected has.
func readGhostState(t *testing.T, s *Store, projectID string) ghostStateRow {
	t.Helper()
	var row ghostStateRow
	err := s.db.QueryRow(
		`SELECT interaction_count, COALESCE(learned_context,''), last_reflection_at,
		        COALESCE(reflection_summary,''), COALESCE(reflect_input_sig,'')
		 FROM ghost_state WHERE project_id = ?`, projectID).
		Scan(&row.interactionCount, &row.learnedContext, &row.lastReflectionAt, &row.reflectionSummary, &row.reflectInputSig)
	if err != nil {
		t.Fatalf("read ghost_state %q: %v", projectID, err)
	}
	return row
}

// seedGhostState writes a ghost_state row directly, so a test can pin the
// reflection timestamp rather than accept whatever datetime('now') produced.
func seedGhostState(t *testing.T, s *Store, projectID string, row ghostStateRow) {
	t.Helper()
	if _, err := s.db.Exec(`
		INSERT INTO ghost_state (project_id, interaction_count, learned_context, last_reflection_at, reflection_summary, reflect_input_sig)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET
			interaction_count = excluded.interaction_count,
			learned_context = excluded.learned_context,
			last_reflection_at = excluded.last_reflection_at,
			reflection_summary = excluded.reflection_summary,
			reflect_input_sig = excluded.reflect_input_sig
	`, projectID, row.interactionCount, row.learnedContext, row.lastReflectionAt, row.reflectionSummary, row.reflectInputSig); err != nil {
		t.Fatalf("seed ghost_state %q: %v", projectID, err)
	}
}

func nullStr(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func assertGhostState(t *testing.T, got, want ghostStateRow, context string) {
	t.Helper()
	if got.interactionCount != want.interactionCount {
		t.Errorf("%s: interaction_count = %d, want %d", context, got.interactionCount, want.interactionCount)
	}
	if got.learnedContext != want.learnedContext {
		t.Errorf("%s: learned_context = %q, want %q", context, got.learnedContext, want.learnedContext)
	}
	if got.reflectionSummary != want.reflectionSummary {
		t.Errorf("%s: reflection_summary = %q, want %q", context, got.reflectionSummary, want.reflectionSummary)
	}
	if got.reflectInputSig != want.reflectInputSig {
		t.Errorf("%s: reflect_input_sig = %q, want %q", context, got.reflectInputSig, want.reflectInputSig)
	}
	if got.lastReflectionAt != want.lastReflectionAt {
		t.Errorf("%s: last_reflection_at = %+v, want %+v", context, got.lastReflectionAt, want.lastReflectionAt)
	}
}

// learnedSeed is a ghost_state row whose every column carries the tag.
func learnedSeed(tag string, count int) ghostStateRow {
	return ghostStateRow{
		interactionCount:  count,
		learnedContext:    "LEARNED " + tag,
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary " + tag,
		reflectInputSig:   "sig " + tag,
	}
}

func projectCount(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM projects WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n
}

// A merge used to delete the outgoing project's ghost_state without carrying its
// learned context over (#1045). Each of the merge's three entry points - the
// public MergeProject wrapper, EnsureProjectWithRepo's auto-merge of a remote
// another project owns, and ResolveOrCreateRepoProject's method copy - must
// leave the survivor holding the outgoing reflection whole.
func TestMergePreservesLearnedContext(t *testing.T) {
	ctx := context.Background()
	const remote = "git@github.com:wcatz/merge-learned.git"
	t.Run("MergeProject", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProject(ctx, "test", "test", "dup-project"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		if err := s.UpdateLearnedContext(ctx, "test", "LEARNED about old project", "summary of the old project"); err != nil {
			t.Fatalf("UpdateLearnedContext: %v", err)
		}
		for i := 0; i < 5; i++ {
			if _, err := s.IncrementInteraction(ctx, "test"); err != nil {
				t.Fatalf("IncrementInteraction: %v", err)
			}
		}
		old := readGhostState(t, s, "test")
		if err := s.MergeProject(ctx, "test", testProject); err != nil {
			t.Fatalf("MergeProject: %v", err)
		}
		got := readGhostState(t, s, testProject)
		assertGhostState(t, got, old, "MergeProject")
		if got.learnedContext == "" || got.interactionCount != 5 {
			t.Errorf("survivor lost the outgoing reflection: %+v", got)
		}
	})
	t.Run("EnsureProjectWithRepo", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
			t.Fatalf("ensure owner: %v", err)
		}
		if err := s.EnsureProject(ctx, "incoming", "/tmp/incoming", "incoming"); err != nil {
			t.Fatalf("ensure incoming: %v", err)
		}
		seedGhostState(t, s, "incoming", learnedSeed("incoming", 3))
		if err := s.EnsureProjectWithRepo(ctx, "incoming", "/tmp/incoming", "incoming", remote); err != nil {
			t.Fatalf("ensure incoming with owner's remote: %v", err)
		}
		if n := projectCount(t, s, "incoming"); n != 0 {
			t.Fatalf("incoming project survived the merge: %d row(s)", n)
		}
		assertGhostState(t, readGhostState(t, s, "owner"), learnedSeed("incoming", 3), "EnsureProjectWithRepo merge")
	})
	t.Run("ResolveOrCreateRepoProject", func(t *testing.T) {
		s := testStore(t)
		if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
			t.Fatalf("ensure owner: %v", err)
		}
		if err := s.EnsureProject(ctx, "outgoing", "/tmp/outgoing", "outgoing"); err != nil {
			t.Fatalf("ensure outgoing: %v", err)
		}
		seedGhostState(t, s, "outgoing", learnedSeed("outgoing", 2))
		if _, _, err := s.ResolveOrCreateRepoProject(ctx, "outgoing", "outgoing", "outgoing", "/tmp/outgoing", "outgoing", remote); err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		if n := projectCount(t, s, "outgoing"); n != 0 {
			t.Fatalf("outgoing project survived the merge: %d row(s)", n)
		}
		assertGhostState(t, readGhostState(t, s, "owner"), learnedSeed("outgoing", 2), "ResolveOrCreateRepoProject merge")
	})
}

// TestMigrateV16MergePreservesLearnedContext models
// TestMigrateV14MergesDuplicateRepositoryIdentities on the ghost_state half:
// the migration's merge of two projects that share a remote must carry the
// outgoing project's learned context to the survivor.
func TestMigrateV16MergePreservesLearnedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duplicate-remote-state.sqlite")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	if _, err := db.Exec(`DROP INDEX idx_projects_repo_remote`); err != nil {
		_ = db.Close()
		t.Fatalf("drop seed index: %v", err)
	}
	const remote = "github.com/me/merge-state"
	if _, err := db.Exec(`
		INSERT INTO projects (id, path, name, repo_remote) VALUES
		('remote-a', '/remote/a', 'a', ?),
		('remote-b', '/remote/b', 'b', ?)
	`, remote, remote); err != nil {
		_ = db.Close()
		t.Fatalf("insert duplicate projects: %v", err)
	}
	// remote-b is the outgoing project (ids are merged in order, so remote-a
	// survives). It carries a reflection; remote-a has none.
	if _, err := db.Exec(`
		INSERT INTO ghost_state (project_id, interaction_count, learned_context, last_reflection_at, reflection_summary, reflect_input_sig)
		VALUES ('remote-b', 4, 'LEARNED remote-b', '2026-06-01 10:00:00', 'summary remote-b', 'sig remote-b')
	`); err != nil {
		_ = db.Close()
		t.Fatalf("insert outgoing ghost_state: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
		_ = db.Close()
		t.Fatalf("stamp pre-v14 schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB migrated: %v", err)
	}
	defer migrated.Close() //nolint:errcheck

	got := readGhostState(t, NewStore(migrated, nil), "remote-a")
	assertGhostState(t, got, ghostStateRow{
		interactionCount:  4,
		learnedContext:    "LEARNED remote-b",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary remote-b",
		reflectInputSig:   "sig remote-b",
	}, "migrateV16 merge")

	var remaining int
	if err := migrated.QueryRow(`SELECT count(*) FROM ghost_state WHERE project_id = 'remote-b'`).Scan(&remaining); err != nil {
		t.Fatalf("count outgoing ghost_state: %v", err)
	}
	if remaining != 0 {
		t.Errorf("outgoing ghost_state row survived the migration: %d row(s)", remaining)
	}
}

// TestMergeGhostStateRowsUnitRule pins the rule itself: the newer reflection
// wins whole, learned_context is never taken from one project while the summary,
// signature and timestamp come from the other, and the interaction count is the
// larger of the two.
func TestMergeGhostStateRowsUnitRule(t *testing.T) {
	unit := func(tag string, count int, at string) ghostStateRow {
		return ghostStateRow{
			interactionCount: count, learnedContext: tag + "-LC", lastReflectionAt: nullStr(at),
			reflectionSummary: tag + "-SUM", reflectInputSig: tag + "-SIG",
		}
	}
	surv, old := unit("SURV", 1, "2026-01-01 00:00:00"), unit("OLD", 9, "2026-06-01 00:00:00")
	survNoStamp, oldNoStamp := unit("SURV", 1, ""), unit("OLD", 9, "")
	withCount := func(r ghostStateRow, n int) ghostStateRow { r.interactionCount = n; return r }
	for _, tc := range []struct {
		name                     string
		survivor, outgoing, want ghostStateRow
	}{
		{"outgoing newer wins whole", surv, old, old},
		{"survivor newer keeps whole", old, surv, old},
		{"survivor has no reflection takes outgoing whole", ghostStateRow{interactionCount: 1}, old, old},
		{"both untimestamped keep survivor whole", survNoStamp, oldNoStamp, withCount(survNoStamp, 9)},
		{"survivor learned context without timestamp keeps its own unit", survNoStamp, old, withCount(survNoStamp, 9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertGhostState(t, mergeGhostStateRows(tc.survivor, tc.outgoing), tc.want, tc.name)
		})
	}
}
