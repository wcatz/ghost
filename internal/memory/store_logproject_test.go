package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestStoreLogsRenderAStoredProjectIDThroughTheTokenGuard(t *testing.T) {
	tokenID := "gh" + "p_" + strings.Repeat("a", 36)
	const withheld = "<project_id withheld: it holds a GitHub personal access token>"
	run := func(t *testing.T, projectID string) string {
		t.Helper()
		var logged bytes.Buffer
		db, err := OpenDB(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := NewStore(db, slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
		ctx := context.Background()
		if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, projectID, "/nonexistent/"+projectID, "logproject"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, created_at, memory_id, scope_captured)
			VALUES ('snap1', ?, 'fact', 'a snapshotted memory', 0.5, 'mcp', ?, 'snap-mem', 1)`, projectID, asOfStampSave); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RestoreSnapshot(ctx, projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteProject(ctx, projectID, true); err != nil {
			t.Fatal(err)
		}
		return logged.String()
	}
	for _, msg := range []string{"memories restored from snapshot", "deleted project"} {
		line := run(t, tokenID)
		if !strings.Contains(line, msg) || strings.Contains(line, tokenID) || !strings.Contains(line, withheld) {
			t.Errorf("%q: %s", msg, line)
		}
		ordinary := run(t, "proj-ordinary")
		if !strings.Contains(ordinary, msg) || !strings.Contains(ordinary, "proj-ordinary") {
			t.Errorf("ordinary %q: %s", msg, ordinary)
		}
	}
}

func TestStoreMergeLogsRenderStoredProjectIDsThroughTheTokenGuard(t *testing.T) {
	tokenOld := "gh" + "p_" + strings.Repeat("a", 36)
	tokenNew := "gh" + "p_" + strings.Repeat("b", 36)
	const withheld = "<project_id withheld: it holds a GitHub personal access token>"
	const remote = "git@github.com:wcatz/merge-log.git"
	setup := func(t *testing.T, oldID, newID string) (*Store, *bytes.Buffer) {
		t.Helper()
		var logged bytes.Buffer
		db, err := OpenDB(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := NewStore(db, slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
		for _, id := range []string{oldID, newID} {
			if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, "/nonexistent/"+id, id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, id); err != nil {
				t.Fatal(err)
			}
		}
		return s, &logged
	}
	paths := map[string]func(*Store, string, string) error{
		"MergeProject": func(s *Store, oldID, newID string) error { return s.MergeProject(context.Background(), oldID, newID) },
		"repository auto-merge": func(s *Store, oldID, newID string) error {
			if _, err := s.db.Exec(`UPDATE projects SET repo_remote = ? WHERE id = ?`, NormalizeRepoRemote(remote), newID); err != nil {
				return err
			}
			return s.EnsureProjectWithRepo(context.Background(), oldID, "/nonexistent/"+oldID, oldID, remote)
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			s, logged := setup(t, tokenOld, tokenNew)
			if err := run(s, tokenOld, tokenNew); err != nil {
				t.Fatal(err)
			}
			line := logged.String()
			if !strings.Contains(line, "merged duplicate project") || strings.Contains(line, tokenOld) || strings.Contains(line, tokenNew) || strings.Count(line, withheld) < 2 {
				t.Errorf("token case: %s", line)
			}
			s, logged = setup(t, "proj-old", "proj-new")
			if err := run(s, "proj-old", "proj-new"); err != nil {
				t.Fatal(err)
			}
			ordinary := logged.String()
			if !strings.Contains(ordinary, "merged duplicate project") || !strings.Contains(ordinary, "proj-old") || !strings.Contains(ordinary, "proj-new") {
				t.Errorf("ordinary: %s", ordinary)
			}
		})
	}
}
