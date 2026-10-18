package memory

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestStoreLogsRenderAStoredProjectIDThroughTheTokenGuard drives the store's
// project lifecycle log lines with a project id that is shaped like a GitHub
// token. A stored project id is not token-checked on import, so an imported
// artifact can carry one, and these lines reach the MCP client's stderr. The
// snapshot restore line and the project delete line both name the project.
//
// Each run asserts both halves: the token-shaped id never appears, and an
// ordinary id still does, so a renderer that withheld everything fails the
// control.
func TestStoreLogsRenderAStoredProjectIDThroughTheTokenGuard(t *testing.T) {
	// Built from parts so this file holds no literal of the token's shape: "ghp_"
	// plus 36 characters, which the token guard names as a GitHub token.
	tokenID := "gh" + "p_" + strings.Repeat("a", 36)
	const withheld = "<project_id withheld: it holds a GitHub personal access token>"

	run := func(t *testing.T, projectID string) string {
		t.Helper()
		var logged bytes.Buffer
		db, err := OpenDB(":memory:")
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := NewStore(db, slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
		ctx := context.Background()

		// The project row goes in directly: the point is a STORED id, which is
		// what an import leaves behind whatever the live entry points ask.
		if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
			projectID, "/nonexistent/"+projectID, "logproject"); err != nil {
			t.Fatalf("insert the project: %v", err)
		}
		if _, err := db.Exec(`INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, projectID); err != nil {
			t.Fatalf("insert the state row: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO memory_snapshots
			    (snapshot_id, project_id, category, content, importance, source, created_at, memory_id, scope_captured)
			VALUES ('snap1', ?, 'fact', 'a snapshotted memory', 0.5, 'mcp', ?, 'snap-mem', 1)`,
			projectID, asOfStampSave); err != nil {
			t.Fatalf("insert the snapshot: %v", err)
		}
		if _, err := s.RestoreSnapshot(ctx, projectID); err != nil {
			t.Fatalf("RestoreSnapshot: %v", err)
		}
		if _, err := s.DeleteProject(ctx, projectID, true); err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}
		return logged.String()
	}

	for _, msg := range []string{"memories restored from snapshot", "deleted project"} {
		t.Run(msg, func(t *testing.T) {
			line := run(t, tokenID)
			if !strings.Contains(line, msg) {
				t.Fatalf("the run never logged %q: %q", msg, line)
			}
			if strings.Contains(line, tokenID) {
				t.Errorf("a log line repeats the token-shaped project id: %s", line)
			}
			if !strings.Contains(line, withheld) {
				t.Errorf("the log does not name the withheld project id and its format: %s", line)
			}

			ordinary := run(t, "proj-ordinary")
			if !strings.Contains(ordinary, msg) {
				t.Fatalf("the control run never logged %q: %q", msg, ordinary)
			}
			if !strings.Contains(ordinary, "proj-ordinary") {
				t.Errorf("an ordinary project id no longer appears in the log: %s", ordinary)
			}
		})
	}
}

// TestStoreMergeLogsRenderStoredProjectIDsThroughTheTokenGuard covers the two
// "merged duplicate project" lines, which name the outgoing and surviving
// project as old_id and new_id. One is written by MergeProject and the other by
// EnsureProjectWithRepo's repository auto-merge, so both entry points run with
// token-shaped ids on both sides, and an ordinary pair is the control.
func TestStoreMergeLogsRenderStoredProjectIDsThroughTheTokenGuard(t *testing.T) {
	// Built from parts so this file holds no literal of the token's shape.
	tokenOld := "gh" + "p_" + strings.Repeat("a", 36)
	tokenNew := "gh" + "p_" + strings.Repeat("b", 36)
	const withheld = "<project_id withheld: it holds a GitHub personal access token>"
	const remote = "git@github.com:wcatz/merge-log.git"

	setup := func(t *testing.T, oldID, newID string) (*Store, *bytes.Buffer) {
		t.Helper()
		var logged bytes.Buffer
		db, err := OpenDB(":memory:")
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s := NewStore(db, slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
		for _, id := range []string{oldID, newID} {
			if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, "/nonexistent/"+id, id); err != nil {
				t.Fatalf("insert project: %v", err)
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, id); err != nil {
				t.Fatalf("insert state: %v", err)
			}
		}
		return s, &logged
	}

	paths := map[string]func(t *testing.T, s *Store, oldID, newID string){
		"MergeProject": func(t *testing.T, s *Store, oldID, newID string) {
			if err := s.MergeProject(context.Background(), oldID, newID); err != nil {
				t.Fatalf("MergeProject: %v", err)
			}
		},
		"repository auto-merge": func(t *testing.T, s *Store, oldID, newID string) {
			if _, err := s.db.Exec(`UPDATE projects SET repo_remote = ? WHERE id = ?`, NormalizeRepoRemote(remote), newID); err != nil {
				t.Fatalf("set the owner's remote: %v", err)
			}
			if err := s.EnsureProjectWithRepo(context.Background(), oldID, "/nonexistent/"+oldID, oldID, remote); err != nil {
				t.Fatalf("EnsureProjectWithRepo: %v", err)
			}
		},
	}

	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			s, logged := setup(t, tokenOld, tokenNew)
			run(t, s, tokenOld, tokenNew)
			line := logged.String()
			if !strings.Contains(line, "merged duplicate project") {
				t.Fatalf("the run never logged the merge: %q", line)
			}
			for _, id := range []string{tokenOld, tokenNew} {
				if strings.Contains(line, id) {
					t.Errorf("the merge log repeats a token-shaped project id: %s", line)
				}
			}
			if n := strings.Count(line, withheld); n < 2 {
				t.Errorf("the merge log names %d withheld ids, want old_id and new_id: %s", n, line)
			}

			s, logged = setup(t, "proj-old", "proj-new")
			run(t, s, "proj-old", "proj-new")
			ordinary := logged.String()
			if !strings.Contains(ordinary, "merged duplicate project") {
				t.Fatalf("the control run never logged the merge: %q", ordinary)
			}
			for _, id := range []string{"proj-old", "proj-new"} {
				if !strings.Contains(ordinary, id) {
					t.Errorf("an ordinary project id %q no longer appears in the merge log: %s", id, ordinary)
				}
			}
		})
	}
}
