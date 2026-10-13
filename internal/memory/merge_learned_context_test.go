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

// TestMergeProjectPreservesLearnedContext reproduces #1045: a merge used to
// delete the outgoing project's ghost_state without carrying its learned
// context over, so the survivor's learned context and interaction count were
// both lost. This is the public wrapper, the free function's path.
func TestMergeProjectPreservesLearnedContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "test", "test", "dup-project"); err != nil {
		t.Fatalf("EnsureProject dup: %v", err)
	}
	// The outgoing project reflects, then is interacted with. UpdateLearnedContext
	// writes learned_context, reflection_summary and last_reflection_at from one
	// pass, so the three travel together.
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
	if got.learnedContext == "" {
		t.Error("survivor's learned context is empty after the merge: the outgoing project's reflection was discarded")
	}
	if got.interactionCount != 5 {
		t.Errorf("survivor interaction_count = %d, want the outgoing project's 5", got.interactionCount)
	}
}

// TestEnsureProjectWithRepoMergePreservesLearnedContext exercises the free
// mergeProjectTx copy through EnsureProjectWithRepo's auto-merge: a second
// project for a remote that is already owned is folded into the owner.
func TestEnsureProjectWithRepoMergePreservesLearnedContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const remote = "git@github.com:wcatz/merge-learned.git"

	if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	// The outgoing project exists without a remote, reflects, and is then
	// presented with the remote the owner already claims.
	if err := s.EnsureProject(ctx, "incoming", "/tmp/incoming", "incoming"); err != nil {
		t.Fatalf("ensure incoming: %v", err)
	}
	seedGhostState(t, s, "incoming", ghostStateRow{
		interactionCount:  3,
		learnedContext:    "LEARNED incoming",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary incoming",
		reflectInputSig:   "sig incoming",
	})

	if err := s.EnsureProjectWithRepo(ctx, "incoming", "/tmp/incoming", "incoming", remote); err != nil {
		t.Fatalf("ensure incoming with owner's remote: %v", err)
	}

	// The incoming row is gone and the owner holds its reflection whole.
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM projects WHERE id = 'incoming'`).Scan(&count); err != nil {
		t.Fatalf("count incoming: %v", err)
	}
	if count != 0 {
		t.Fatalf("incoming project survived the merge: %d row(s)", count)
	}
	got := readGhostState(t, s, "owner")
	assertGhostState(t, got, ghostStateRow{
		interactionCount:  3,
		learnedContext:    "LEARNED incoming",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary incoming",
		reflectInputSig:   "sig incoming",
	}, "EnsureProjectWithRepo merge")
}

// TestResolveOrCreateRepoProjectMergePreservesLearnedContext exercises the
// (*Store).mergeProjectTx method copy, which the free function does not reach:
// its callers are the bind-recovery paths, and ResolveOrCreateRepoProject is
// the exported entry that reaches one. An explicit, unbound project that is
// presented with a remote another project already owns is merged into the
// owner.
func TestResolveOrCreateRepoProjectMergePreservesLearnedContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const remote = "git@github.com:wcatz/method-merge-learned.git"

	if err := s.EnsureProjectWithRepo(ctx, "owner", "/tmp/owner", "owner", remote); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	if err := s.EnsureProject(ctx, "outgoing", "/tmp/outgoing", "outgoing"); err != nil {
		t.Fatalf("ensure outgoing: %v", err)
	}
	seedGhostState(t, s, "outgoing", ghostStateRow{
		interactionCount:  2,
		learnedContext:    "LEARNED outgoing",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary outgoing",
		reflectInputSig:   "sig outgoing",
	})

	if _, _, err := s.ResolveOrCreateRepoProject(ctx, "outgoing", "outgoing", "outgoing", "/tmp/outgoing", "outgoing", remote); err != nil {
		t.Fatalf("ResolveOrCreateRepoProject: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM projects WHERE id = 'outgoing'`).Scan(&count); err != nil {
		t.Fatalf("count outgoing: %v", err)
	}
	if count != 0 {
		t.Fatalf("outgoing project survived the merge: %d row(s)", count)
	}
	got := readGhostState(t, s, "owner")
	assertGhostState(t, got, ghostStateRow{
		interactionCount:  2,
		learnedContext:    "LEARNED outgoing",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary outgoing",
		reflectInputSig:   "sig outgoing",
	}, "ResolveOrCreateRepoProject merge")
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

// TestBothMergePathsPreserveLearnedContextIdentically is the guard the review
// asked for: the free function and the method are two copies of the merge, and
// they must leave the survivor's ghost_state identical. The method copy is
// reached through ResolveOrCreateRepoProject, the free one through
// MergeProject; both are fed the same shape and their results compared.
func TestBothMergePathsPreserveLearnedContextIdentically(t *testing.T) {
	const remote = "git@github.com:wcatz/identical-merge.git"
	seed := ghostStateRow{
		interactionCount:  7,
		learnedContext:    "LEARNED shared shape",
		lastReflectionAt:  nullStr("2026-06-01 10:00:00"),
		reflectionSummary: "summary shared shape",
		reflectInputSig:   "sig shared shape",
	}

	t.Run("free", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProject(ctx, "outgoing", "/tmp/outgoing", "outgoing"); err != nil {
			t.Fatalf("ensure outgoing: %v", err)
		}
		seedGhostState(t, s, "outgoing", seed)
		if err := s.MergeProject(ctx, "outgoing", testProject); err != nil {
			t.Fatalf("MergeProject: %v", err)
		}
		assertGhostState(t, readGhostState(t, s, testProject), seed, "free merge path")
	})

	t.Run("method", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		if err := s.EnsureProjectWithRepo(ctx, testProject, "/tmp/test", "test", remote); err != nil {
			t.Fatalf("bind survivor remote: %v", err)
		}
		if err := s.EnsureProject(ctx, "outgoing", "/tmp/outgoing", "outgoing"); err != nil {
			t.Fatalf("ensure outgoing: %v", err)
		}
		seedGhostState(t, s, "outgoing", seed)
		if _, _, err := s.ResolveOrCreateRepoProject(ctx, "outgoing", "outgoing", "outgoing", "/tmp/outgoing", "outgoing", remote); err != nil {
			t.Fatalf("ResolveOrCreateRepoProject: %v", err)
		}
		assertGhostState(t, readGhostState(t, s, testProject), seed, "method merge path")
	})
}

// TestMergeGhostStateRowsUnitRule pins the rule itself, including the edges the
// review called out: learned_context is never taken from one project while the
// summary, signature and timestamp come from the other.
func TestMergeGhostStateRowsUnitRule(t *testing.T) {
	surv := ghostStateRow{
		interactionCount:  1,
		learnedContext:    "SURV-LC",
		lastReflectionAt:  nullStr("2026-01-01 00:00:00"),
		reflectionSummary: "SURV-SUM",
		reflectInputSig:   "SURV-SIG",
	}
	old := ghostStateRow{
		interactionCount:  9,
		learnedContext:    "OLD-LC",
		lastReflectionAt:  nullStr("2026-06-01 00:00:00"),
		reflectionSummary: "OLD-SUM",
		reflectInputSig:   "OLD-SIG",
	}

	for _, tc := range []struct {
		name     string
		survivor ghostStateRow
		outgoing ghostStateRow
		want     ghostStateRow
	}{
		{
			name:     "outgoing newer wins whole",
			survivor: surv,
			outgoing: old,
			want: ghostStateRow{
				interactionCount: 9, learnedContext: "OLD-LC",
				lastReflectionAt:  nullStr("2026-06-01 00:00:00"),
				reflectionSummary: "OLD-SUM", reflectInputSig: "OLD-SIG",
			},
		},
		{
			name:     "survivor newer keeps whole",
			survivor: old,
			outgoing: surv,
			want: ghostStateRow{
				interactionCount: 9, learnedContext: "OLD-LC",
				lastReflectionAt:  nullStr("2026-06-01 00:00:00"),
				reflectionSummary: "OLD-SUM", reflectInputSig: "OLD-SIG",
			},
		},
		{
			name:     "survivor has no reflection takes outgoing whole",
			survivor: ghostStateRow{interactionCount: 1},
			outgoing: old,
			want: ghostStateRow{
				interactionCount: 9, learnedContext: "OLD-LC",
				lastReflectionAt:  nullStr("2026-06-01 00:00:00"),
				reflectionSummary: "OLD-SUM", reflectInputSig: "OLD-SIG",
			},
		},
		{
			name: "both untimestamped keep survivor whole",
			survivor: ghostStateRow{
				interactionCount: 1, learnedContext: "SURV-LC",
				reflectionSummary: "SURV-SUM", reflectInputSig: "SURV-SIG",
			},
			outgoing: ghostStateRow{
				interactionCount: 9, learnedContext: "OLD-LC",
				reflectionSummary: "OLD-SUM", reflectInputSig: "OLD-SIG",
			},
			want: ghostStateRow{
				interactionCount: 9, learnedContext: "SURV-LC",
				reflectionSummary: "SURV-SUM", reflectInputSig: "SURV-SIG",
			},
		},
		{
			name: "survivor learned context without timestamp keeps its own unit",
			survivor: ghostStateRow{
				interactionCount: 1, learnedContext: "SURV-LC",
				reflectionSummary: "SURV-SUM", reflectInputSig: "SURV-SIG",
			},
			outgoing: old,
			want: ghostStateRow{
				interactionCount: 9, learnedContext: "SURV-LC",
				reflectionSummary: "SURV-SUM", reflectInputSig: "SURV-SIG",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertGhostState(t, mergeGhostStateRows(tc.survivor, tc.outgoing), tc.want, tc.name)
		})
	}
}
