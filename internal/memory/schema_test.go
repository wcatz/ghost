package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenDBHostilePath: OpenDB must open the database at exactly the path it
// was given even when the path contains URI-special characters ('?' or '#',
// legal in $XDG_DATA_HOME/$HOME). A naive DSN concatenation parses those as
// query/fragment separators and silently opens a truncated path instead.
func TestOpenDBHostilePath(t *testing.T) {
	for _, dirName := range []string{"we?rd", "we#rd", "with space"} {
		t.Run(dirName, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), dirName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			dbPath := filepath.Join(dir, "ghost.db")

			db, err := OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB(%q): %v", dbPath, err)
			}
			store := NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			defer func() { _ = store.Close() }()

			ctx := context.Background()
			if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			id, err := store.Create(ctx, "p", Memory{Category: "fact", Content: "roundtrip", Importance: 0.5, Source: "mcp"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := store.GetByIDs(ctx, []string{id})
			if err != nil || len(got) != 1 || got[0].Content != "roundtrip" {
				t.Fatalf("roundtrip failed: %v %v", got, err)
			}

			// The database landed at the intended path — not a truncated one.
			if _, err := os.Stat(dbPath); err != nil {
				t.Errorf("database not at intended path %q: %v", dbPath, err)
			}
		})
	}
}

// TestOpenDBInMemory: the ":memory:" special path must keep its bare form —
// wrapping it in a file: URI would change its per-connection semantics.
func TestOpenDBInMemory(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB(:memory:): %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("schema not initialized on :memory:: %v", err)
	}
}
