package memory

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestAPurgeLeavesNoCopyOfTheTextInTheDatabaseFile: the redaction path's one
// observable promise. `ghost history purge` and DeleteWithOptions(PurgeHistory)
// tell the caller the text is gone; before this test it survived in freed pages
// of the main database file and in the FTS5 segment blobs that still held its
// tokens. A row scan passes while a byte scan does not, which is why this reads
// the file itself. See #1043.
//
// Both purge directions are covered, because the delete-time purge reaches the
// memories row and its history while the already-deleted purge reaches only the
// tombstone: each is the one that leaves a different copy behind.
func TestAPurgeLeavesNoCopyOfTheTextInTheDatabaseFile(t *testing.T) {
	// One token, so the scan finds it in the FTS5 index too: FTS5 stores the
	// porter-stemmed tokens of the content, not the whole phrase, and a scan for
	// the full sentence would miss the index entirely.
	const canary = "zqxjkvbrmp"
	const original = "a fact about " + canary + " that a purge must erase"
	const revised = "the " + canary + " claim has been reworded"

	cases := []struct {
		name  string
		purge func(t *testing.T, s *Store, id string)
	}{
		{
			name: "delete-time purge",
			purge: func(t *testing.T, s *Store, id string) {
				t.Helper()
				if err := s.DeleteWithOptions(context.Background(), id,
					DeleteOptions{PurgeHistory: true}); err != nil {
					t.Fatalf("DeleteWithOptions: %v", err)
				}
			},
		},
		{
			name: "history-only purge of an already-deleted memory",
			purge: func(t *testing.T, s *Store, id string) {
				t.Helper()
				if err := s.Delete(context.Background(), id); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				if _, err := s.PurgeMemoryHistory(context.Background(), id); err != nil {
					t.Fatalf("PurgeMemoryHistory: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "ghost.db")
			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx := context.Background()
			if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}

			id, _, _, err := s.Upsert(ctx, testProject, "fact", original, "mcp", 0.5, nil)
			if err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if err := s.UpdateMemory(ctx, testProject, id, strPtr(revised), nil, nil, nil); err != nil {
				t.Fatalf("UpdateMemory: %v", err)
			}
			// The fixture proves nothing if the text is not really stored.
			if leaked, err := countOccurrences(t, s.db, original); err != nil {
				t.Fatalf("scan before purge: %v", err)
			} else if leaked == 0 {
				t.Fatal("the fixture never stored the canary text")
			}

			tc.purge(t, s, id)

			// No checkpoint here: the purge is what must fold the zeroed pages
			// back into the main file and truncate the WAL, or a byte scan of
			// either one finds the text in the frames the writes left there.
			for _, name := range []string{dbPath, dbPath + "-wal"} {
				body, err := os.ReadFile(name)
				if err != nil {
					if os.IsNotExist(err) {
						continue
					}
					t.Fatalf("read %s: %v", name, err)
				}
				if i := bytes.Index(body, []byte(canary)); i >= 0 {
					t.Errorf("%s still holds the purged canary at byte %d", filepath.Base(name), i)
				}
				if i := bytes.Index(body, []byte(original)); i >= 0 {
					t.Errorf("%s still holds the purged text at byte %d", filepath.Base(name), i)
				}
			}
		})
	}
}
