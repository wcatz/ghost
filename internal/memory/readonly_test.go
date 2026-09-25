package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenDBReadOnlyNeverCreates — `ghost mcp status` reads the projects table
// through this opener. If it created the file, the first status run on a fresh
// install would leave a store behind and the next run would report a healthy
// database instead of "no Ghost database (run ghost first)": the diagnostic
// would make its own result untrue.
func TestOpenDBReadOnlyNeverCreates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")

	if _, err := OpenDBReadOnly(dbPath); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("err = %v, want ErrNoDatabase for a path that does not exist", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("the failed open created %s", dbPath)
	}

	// A real store can be read back without any write path being available.
	rw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := NewStore(rw, nil).EnsureProject(t.Context(), "infra", "", "infrastructure"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenDBReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenDBReadOnly: %v", err)
	}
	defer ro.Close() //nolint:errcheck

	projects, err := NewStore(ro, nil).ListUnboundProjects(t.Context())
	if err != nil {
		t.Fatalf("ListUnboundProjects on a read-only store: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != "infra" {
		t.Fatalf("got %+v, want the sentinel-path project", projects)
	}

	// A write through the read-only connection must fail rather than silently
	// opening the file read-write.
	if _, err := ro.ExecContext(t.Context(), `DELETE FROM projects`); err == nil {
		t.Error("a write through the read-only connection succeeded")
	}
}
