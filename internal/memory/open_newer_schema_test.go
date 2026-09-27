package memory

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// issue #560: OpenDB ran initSQL — this build's whole DDL — before it read
// PRAGMA user_version and refused a database written by a newer ghost. The
// refusal is the point ("upgrade ghost before opening it"), and it happened
// after the build had already written into a database it says it cannot
// interpret.

// schemaObjects lists every table, index, trigger and view SQLite has recorded
// for a database, DDL text included, so a test can assert that an open left
// the schema exactly as it found it.
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT type, name, COALESCE(sql, '') FROM sqlite_master
		WHERE type IN ('table', 'index', 'trigger', 'view')
		ORDER BY type, name
	`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out = append(out, kind+" "+name+" "+ddl)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}

// TestOpenDBRefusesANewerSchemaWithoutWritingToIt pins the refusal as a
// no-write: initSQL is CREATE ... IF NOT EXISTS, so on a database that merely
// has MORE than this build knows it is nearly a no-op — which is exactly why
// the ordering went unnoticed. It stops being one for any shape difference:
// a trigger or index the newer build renamed, replaced or dropped is
// recreated here, in a database the same call then declares unreadable.
func TestOpenDBRefusesANewerSchemaWithoutWritingToIt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "newer-schema.sqlite")
	seed, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}

	// Shape a database as a newer ghost would have left it: stamped above this
	// build, and missing two objects this build's initSQL would recreate.
	for _, stmt := range []string{
		`DROP TRIGGER memories_ai`,
		`DROP INDEX idx_links_source`,
		fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1),
	} {
		if _, err := seed.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	before := schemaObjects(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	if _, err := OpenDB(dbPath); err == nil {
		t.Fatal("OpenDB accepted a database stamped newer than this build")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("OpenDB error = %v, want the newer-schema refusal", err)
	}

	readBack, err := OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = readBack.Close() }()
	after := schemaObjects(t, readBack)
	if !slices.Equal(after, before) {
		t.Errorf("schema changed under a refused open:\nbefore: %v\nafter:  %v", before, after)
	}
}

// TestOpenDBCreatesAndStampsAFreshDatabase guards the other side of the move:
// reading the version before the DDL must not stop a first run from creating
// the schema, stamping it, or leaving the FTS triggers working.
func TestOpenDBCreatesAndStampsAFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.sqlite")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB fresh: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d: a fresh database must be stamped", version, schemaVersion)
	}

	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/fresh", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	const content = "a fresh store indexes what is written into it"
	if _, _, _, err := s.Upsert(ctx, testProject, "fact", content, "mcp", 0.5, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// SearchFTS only answers if both the table and the FTS triggers initSQL
	// creates are in place.
	rows, err := s.SearchFTS(ctx, testProject, "fresh store indexes", 5)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("SearchFTS returned %d rows, want 1: initSQL did not leave a working FTS index", len(rows))
	}
}
