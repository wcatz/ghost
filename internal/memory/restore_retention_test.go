package memory

// #1341's schema, from the restore side: a snapshot records the retention tier
// it captured, and both restore paths put it back with the expiry its tier
// derives.
//
// Before this, memory_snapshots recorded `scope` and its marker (v14, #572) but
// no tier — so `ghost reflect --restore`, the only undo history a reflection
// replace has, put neither the tier nor the expiry back. A restored session row
// came back as the column DEFAULT: project, no expiry, and therefore a row
// `ghost prune` could never reach. A tier raise made inside the replace
// (raiseReusedRetentionTx) outlived the undo that was supposed to reverse it.
//
// The scope half of this same boundary is the template the two tests here
// follow, and it lives in scope_replace_test.go: mark the snapshot's rows as
// uncaptured and prove restore leaves the live value alone rather than
// believing what it never read.

import (
	"context"
	"testing"
	"time"
)

// TestARestorePutsTheTierAndItsExpiryBack is the positive half: a snapshot this
// build wrote recorded a tier, and both restore paths put it back.
//
// The three subtests are the three shapes a restore is asked to undo, and they
// are different statements rather than one repeated: a row the replace DELETED
// comes back through the INSERT, a row it kept is rewritten through the UPDATE,
// and a tier the replace's own reuse branch raised is undone on that same
// in-place row.
func TestARestorePutsTheTierAndItsExpiryBack(t *testing.T) {
	t.Run("a session row the replace deleted comes back prunable", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const body = "a session fact the replace drops and the restore must bring back"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		// Unrelated text, so the replace cannot reuse the row: it is a candidate
		// with no matching emission and the delete set takes it. That is the
		// shape that reaches the INSERT path on restore.
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: "the consolidated replacement", Importance: 0.6,
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if rowExists(t, s, live) {
			t.Fatalf("precondition: the replace did not consume the session row %s, so the INSERT path is not under test", live)
		}

		res, err := s.RestoreSnapshot(ctx, testProject)
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if res.Restored == 0 {
			t.Fatalf("the restore reported no work done")
		}
		if !rowExists(t, s, live) {
			t.Fatalf("the snapshot did not bring the row back under its own id")
		}
		after := getOne(t, s, live)
		if after.Retention != RetentionSession {
			t.Errorf("the restored row is %q, want %q: a session row that comes back project is a row nothing can prune",
				after.Retention, RetentionSession)
		}
		// The expiry is RE-DERIVED, not restored: the snapshot stores no expiry,
		// and sessionExpiry is its only source everywhere else too.
		wantFreshSessionExpiry(t, live, after.ExpiresAt, from, time.Now().UTC())

		// And the consequence the tier exists for: a prune far enough past the
		// expiry reaches the row. Before the fix this row was retention=project
		// with a NULL expiry, so the run selected nothing and the memory was
		// permanent.
		report, err := s.PruneSessionMemories(ctx, PruneOptions{Now: from.Add(400 * 24 * time.Hour)})
		if err != nil {
			t.Fatalf("PruneSessionMemories: %v", err)
		}
		found := false
		for _, c := range report.Candidates {
			if c.ID == live {
				found = true
			}
		}
		if !found {
			t.Errorf("the restored session row is not a prune candidate %d days past its expiry; a restored memory nobody can remove is the defect",
				400)
		}
	})

	t.Run("a tier a later save raised is lowered by the restore", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const body = "the staging cluster answers on port 8443"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		// The snapshot, and a byte-identical re-emission so the row is REUSED and
		// keeps the id the snapshot records (the UPDATE path, not the INSERT).
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if !rowExists(t, s, live) {
			t.Fatal("the row was not reused, so the snapshot does not name its id")
		}

		// The edit and the raise the restore has to undo: a row whose text has not
		// moved would never have been rewritten before the fix either, because the
		// UPDATE's history set is gated on the recorded columns. A PROJECT save
		// raises the tier and clears the expiry.
		edited := "the staging cluster answers on port 8443, kept for the record"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit the row after the snapshot: %v", err)
		}
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			edited+" and restated as durable", "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionProject}); err != nil {
			t.Fatalf("raise the row: %v", err)
		} else if dup != live {
			t.Fatalf("the restatement folded into %q, want %q", dup, live)
		}
		if raised := getOne(t, s, live); raised.Retention != RetentionProject {
			t.Fatalf("the fixture did not raise the row (retention %q); the restore would have nothing to undo", raised.Retention)
		}

		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, live)
		if after.Content != body {
			t.Fatalf("the restore did not rewrite the row (content %q, want the snapshot's %q): the UPDATE path did not fire", after.Content, body)
		}
		if after.Retention != RetentionSession {
			t.Errorf("after the restore the row is %q, want %q: the snapshot recorded the tier, so the undo puts it back",
				after.Retention, RetentionSession)
		}
		wantFreshSessionExpiry(t, live, after.ExpiresAt, from, time.Now().UTC())
	})

	t.Run("a raise the replace made is undone by the restore", func(t *testing.T) {
		// Case B of #1341, and not the same as the subtest above: the raise here
		// happens INSIDE ReplaceNonManual, through raiseReusedRetentionTx, on a row
		// whose id is in the snapshot — so the undo is tested against a tier the
		// consolidation itself established rather than one a later save did.
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		// Unrelated text on purpose: near-duplicate content lets the second save
		// re-tier the first before the replace even runs, and the fixture would
		// then be recording a tier the raise never changed (a trap this test hit).
		const reused = "the staging cluster answers on port 8443"
		const other = "deploys go out through the bastion tunnel on port 2222"
		kept, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", reused, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save the session row: %v", err)
		}
		absorbed, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", other, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("save the project row: %v", err)
		}
		if before := getOne(t, s, kept); before.Retention != RetentionSession {
			t.Fatalf("precondition: the row to be reused is already %q before the replace, so there is no raise to undo",
				before.Retention)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: reused, Importance: 0.5,
			ReplacesIDs: []string{kept, absorbed},
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if !rowExists(t, s, kept) {
			t.Fatalf("the reused row %s is gone; the fixture did not exercise a reuse", kept)
		}
		raised := getOne(t, s, kept)
		if raised.Retention != RetentionProject {
			t.Fatalf("the replace did not raise the reused row (retention %q); the reuse branch did not fire and the restore has nothing to undo",
				raised.Retention)
		}

		res, err := s.RestoreSnapshot(ctx, testProject)
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if res.Restored == 0 {
			t.Fatalf("the restore reported no rows restored, so nothing below is about the restore")
		}
		after := getOne(t, s, kept)
		if after.Retention != RetentionSession {
			t.Errorf("after the restore the reused row is %q, want %q: the raise survived the undo that exists to reverse it",
				after.Retention, RetentionSession)
		}
		wantFreshSessionExpiry(t, kept, after.ExpiresAt, from, time.Now().UTC())
	})

	t.Run("a durable row restored gains no expiry", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const body = "the retention tiers are three and this row is the middle one"

		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
			UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("save the project row: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
			Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
		}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		edited := body + ", restated"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit the row after the snapshot: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, live)
		if after.Retention != RetentionProject {
			t.Errorf("after the restore the row is %q, want %q", after.Retention, RetentionProject)
		}
		if after.ExpiresAt != nil {
			t.Errorf("after the restore the durable row carries expires_at %q; only a session row is scheduled for expiry, because a caller cannot state one",
				*after.ExpiresAt)
		}
	})
}

// TestARestoreLeavesTheTierAloneForALegacySnapshot is #1341's other boundary,
// and it is scope's (#572/v14) restated for the tier: migrateV26 adds
// memory_snapshots.retention to a table whose earlier rows have none, and
// retention_captured = 0 is how those rows say so. Restore must not read that
// NULL as "the memory was a project memory" — the DEFAULT would be a claim the
// snapshot never made — so it leaves the live tier and expiry as they stand and
// the result says the undo was incomplete.
func TestARestoreLeavesTheTierAloneForALegacySnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const body = "a fact whose snapshot tier was never actually recorded"

	live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the session row: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{
		Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live},
	}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// Stand in for a snapshot taken before schema v26: the marker is what says
	// "no tier was recorded", so the value beside it must not be believed even
	// when a hand backfill left one.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE memory_snapshots SET retention = ?, retention_captured = 0 WHERE memory_id = ?
	`, RetentionSession, live); err != nil {
		t.Fatalf("mark the snapshot uncaptured: %v", err)
	}
	// The live row is raised after the snapshot with a PROJECT save, so a restore
	// that believed the uncaptured value would look like it had done something.
	edited := body + ", raised while the snapshot was uncaptured"
	if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
		t.Fatalf("edit the row after the snapshot: %v", err)
	}
	if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact", edited+" and restated", "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionProject}); err != nil {
		t.Fatalf("raise the row: %v", err)
	} else if dup != live {
		t.Fatalf("the restatement folded into %q, want %q", dup, live)
	}

	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	after := getOne(t, s, live)
	if after.Content != body {
		t.Fatalf("the restore did not rewrite the row (content %q, want the snapshot's %q): the UPDATE path did not fire", after.Content, body)
	}
	if after.Retention != RetentionProject || after.ExpiresAt != nil {
		t.Errorf("the restore wrote retention=%q expires_at=%s over a snapshot that recorded no tier; the live row must keep what it had",
			after.Retention, expiryText(after.ExpiresAt))
	}
	if res.UncapturedRetention == 0 {
		t.Errorf("the restore reported no uncaptured retention for a snapshot whose every row predates the column; the incomplete undo must be visible")
	}
}

// TestARestoreReportsTheLegacySnapshotOnTheInsertPath is the other half of the
// uncaptured boundary: the row is missing rather than present, so it comes back
// through the INSERT ... SELECT, which has no live tier to leave alone. The
// column DEFAULT is the honest answer for a row arriving from nowhere, and the
// result still has to say the tier was not restored rather than present the
// DEFAULT as the snapshot's.
func TestARestoreReportsTheLegacySnapshotOnTheInsertPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const body = "a session fact whose snapshot predates the retention column"

	live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil,
		UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save the session row: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if rowExists(t, s, live) {
		t.Fatalf("precondition: the replace did not consume the session row %s", live)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE memory_snapshots SET retention = NULL, retention_captured = 0 WHERE memory_id = ?
	`, live); err != nil {
		t.Fatalf("mark the snapshot uncaptured: %v", err)
	}

	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !rowExists(t, s, live) {
		t.Fatalf("the snapshot did not bring the row back")
	}
	back := getOne(t, s, live)
	if back.Retention != RetentionProject {
		t.Errorf("the re-created row is %q, want %q: an uncaptured snapshot states no tier, so the column DEFAULT is all this build can honestly write",
			back.Retention, RetentionProject)
	}
	if back.ExpiresAt != nil {
		t.Errorf("the re-created row carries expires_at %q; nothing may schedule an expiry for a row whose tier was never recorded", *back.ExpiresAt)
	}
	if res.UncapturedRetention == 0 {
		t.Errorf("the restore reported no uncaptured retention for a snapshot whose every row predates the column")
	}
}
