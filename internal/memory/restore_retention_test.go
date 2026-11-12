package memory

import (
	"context"
	"testing"
	"time"
)

func TestARestorePutsTheTierAndItsExpiryBack(t *testing.T) {
	t.Run("a session row the replace deleted comes back prunable", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const body = "a session fact the replace drops and the restore must bring back"
		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "the consolidated replacement", Importance: 0.6}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if rowExists(t, s, live) {
			t.Fatal("precondition: row not consumed")
		}
		res, err := s.RestoreSnapshot(ctx, testProject)
		if err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if res.Restored == 0 || !rowExists(t, s, live) {
			t.Fatalf("restored=%d exists=%v", res.Restored, rowExists(t, s, live))
		}
		after := getOne(t, s, live)
		if after.Retention != RetentionSession {
			t.Errorf("retention=%q want %q", after.Retention, RetentionSession)
		}
		wantFreshSessionExpiry(t, live, after.ExpiresAt, from, time.Now().UTC())
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
			t.Error("restored session row not a prune candidate")
		}
	})
	t.Run("a tier a later save raised is lowered by the restore", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const body = "the staging cluster answers on port 8443"
		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		edited := "the staging cluster answers on port 8443, kept for the record"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact", edited+" and restated as durable", "mcp", 0.5, nil, UpsertOptions{Retention: RetentionProject}); err != nil || dup != live {
			t.Fatalf("raise: dup=%q err=%v", dup, err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, live)
		if after.Content != body || after.Retention != RetentionSession {
			t.Fatalf("after restore content=%q retention=%q", after.Content, after.Retention)
		}
		wantFreshSessionExpiry(t, live, after.ExpiresAt, from, time.Now().UTC())
	})
	t.Run("a raise the replace made is undone by the restore", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		from := time.Now().UTC()
		const reused = "the staging cluster answers on port 8443"
		const other = "deploys go out through the bastion tunnel on port 2222"
		kept, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", reused, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save kept: %v", err)
		}
		absorbed, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", other, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("save absorbed: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: reused, Importance: 0.5, ReplacesIDs: []string{kept, absorbed}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if !rowExists(t, s, kept) || getOne(t, s, kept).Retention != RetentionProject {
			t.Fatalf("precondition not met")
		}
		res, err := s.RestoreSnapshot(ctx, testProject)
		if err != nil || res.Restored == 0 {
			t.Fatalf("RestoreSnapshot: res=%+v err=%v", res, err)
		}
		after := getOne(t, s, kept)
		if after.Retention != RetentionSession {
			t.Errorf("retention=%q want %q", after.Retention, RetentionSession)
		}
		wantFreshSessionExpiry(t, kept, after.ExpiresAt, from, time.Now().UTC())
	})
	t.Run("a durable row restored gains no expiry", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const body = "the retention tiers are three and this row is the middle one"
		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionProject})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		edited := body + ", restated"
		if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
			t.Fatalf("edit: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		after := getOne(t, s, live)
		if after.Retention != RetentionProject || after.ExpiresAt != nil {
			t.Errorf("retention=%q expires=%v", after.Retention, after.ExpiresAt)
		}
	})
}

func TestARestoreLeavesTheTierAloneForALegacySnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const body = "a fact whose snapshot tier was never actually recorded"
	live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live}}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET retention = ?, retention_captured = 0 WHERE memory_id = ?`, RetentionSession, live); err != nil {
		t.Fatalf("mark uncaptured: %v", err)
	}
	edited := body + ", raised while the snapshot was uncaptured"
	if err := s.UpdateMemory(ctx, testProject, live, &edited, nil, nil, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, dup, _, err := s.UpsertWithOptions(ctx, testProject, "fact", edited+" and restated", "mcp", 0.5, nil, UpsertOptions{Retention: RetentionProject}); err != nil || dup != live {
		t.Fatalf("raise: %v", err)
	}
	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	after := getOne(t, s, live)
	if after.Content != body {
		t.Fatalf("content=%q", after.Content)
	}
	if after.Retention != RetentionProject || after.ExpiresAt != nil {
		t.Errorf("retention=%q expires=%v", after.Retention, expiryText(after.ExpiresAt))
	}
	if res.UncapturedRetention == 0 {
		t.Error("UncapturedRetention==0")
	}
}

func TestARestoreReportsTheLegacySnapshotOnTheInsertPath(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const body = "a session fact whose snapshot predates the retention column"
	live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6}}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_snapshots SET retention = NULL, retention_captured = 0 WHERE memory_id = ?`, live); err != nil {
		t.Fatalf("mark uncaptured: %v", err)
	}
	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !rowExists(t, s, live) {
		t.Fatal("row not restored")
	}
	back := getOne(t, s, live)
	if back.Retention != RetentionProject || back.ExpiresAt != nil {
		t.Errorf("retention=%q expires=%v", back.Retention, back.ExpiresAt)
	}
	if res.UncapturedRetention == 0 {
		t.Error("UncapturedRetention==0")
	}
}

func TestARestoreIdempotenceForAnUnchangedSessionRow(t *testing.T) {
	t.Run("a session row reused verbatim keeps its expiry across two restores", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const body = "the staging cluster answers on port 8443"
		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		ageSessionRow(t, s, live, 400*24*time.Hour)
		pre := expiryText(getOne(t, s, live).ExpiresAt)
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: body, Importance: 0.5, ReplacesIDs: []string{live}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if expiryText(getOne(t, s, live).ExpiresAt) != pre {
			t.Fatal("precondition broken")
		}
		for i := 0; i < 2; i++ {
			if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
				t.Fatalf("restore %d: %v", i+1, err)
			}
			if got := expiryText(getOne(t, s, live).ExpiresAt); got != pre {
				t.Errorf("run %d expiry=%q want %q", i+1, got, pre)
			}
		}
	})
	t.Run("an expiry a fold refreshed survives when the tier did not move", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		const body = "the staging cluster answers on port 8443"
		live, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", body, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{{Category: "fact", Content: body, Importance: 0.6, ReplacesIDs: []string{live}}}, "", nil); err != nil {
			t.Fatalf("ReplaceNonManual: %v", err)
		}
		if getOne(t, s, live).Retention != RetentionSession || expiryText(getOne(t, s, live).ExpiresAt) == "" {
			t.Fatal("precondition broken")
		}
		ageSessionRow(t, s, live, 400*24*time.Hour)
		aged := expiryText(getOne(t, s, live).ExpiresAt)
		if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if got := expiryText(getOne(t, s, live).ExpiresAt); got != aged {
			t.Errorf("expiry=%q want %q", got, aged)
		}
	})
}
