package reflection

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// roDSN builds a side-effect-free read-only DSN. immutable=1 makes SQLite read
// the main database file directly instead of going through the WAL index: no
// -shm or -wal file is created or touched next to the database. The path is
// percent-encoded the same way OpenDB does it (schema.go), so a '?' or '#' in
// the data-dir path is not parsed as a URI separator.
func roDSN(path string) string {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	return u.String() + "?mode=ro&immutable=1"
}

// openSnapshot opens a database snapshot for verification. It refuses a
// non-empty -wal first: immutable reads ignore the WAL, so certifying against a
// database with uncheckpointed frames would silently read stale contents. It
// also reports the schema version, because these queries name columns
// (pinned, resolved_at) that an older database may not have yet.
func openSnapshot(t *testing.T, path string) *sql.DB {
	t.Helper()
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("%s has a %d-byte WAL; immutable reads would ignore its committed frames. "+
			"Take a fresh snapshot (sqlite3 %s \".backup <dest>\") or checkpoint it before certifying.",
			path, fi.Size(), path)
	}
	db, err := sql.Open("sqlite", roDSN(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read schema version of %s: %v", path, err)
	}
	t.Logf("%s schema user_version=%d", path, v)
	return db
}

// TestVerifyLiveCopy is a manual verification harness, skipped unless the env
// vars below are set. It runs the REAL guarded-drop audit over a before/after
// pair of ghost databases so a reflect --apply can be proven faithful without
// touching live data:
//
//  1. snapshot the live DB:   sqlite3 <live.db> ".backup /tmp/verify/before.db"
//  2. copy it into a data dir: mkdir -p /tmp/verify/data/ghost
//     cp /tmp/verify/before.db /tmp/verify/data/ghost/ghost.db
//  3. run the apply against it:
//     XDG_DATA_HOME=/tmp/verify/data ghost reflect <project> --apply --source opencode
//     (on a clean exit SQLite checkpoints and truncates the WAL, so the copy is
//     a clean snapshot again)
//  4. verify:
//     GHOST_VERIFY_BEFORE=/tmp/verify/before.db GHOST_VERIFY_AFTER=/tmp/verify/data/ghost/ghost.db \
//     GHOST_VERIFY_PROJECT=<id> go test ./internal/reflection/ -run TestVerifyLiveCopy -v
//
// Survivors are only the memories the run could actually have produced: the
// project's replaceable rows (non-manual, unpinned, unresolved — the rows
// ReplaceNonManual replaces) and _global rows with the same filters, because the
// consolidator may legitimately promote a memory there and the apply path
// upserts promotions with source 'reflection'. Nothing else counts: a manual,
// pinned or resolved row was never touched by the pipeline, so letting it count
// would mask a genuine drop and certify a lossy copy as faithful.
func TestVerifyLiveCopy(t *testing.T) {
	beforePath := os.Getenv("GHOST_VERIFY_BEFORE")
	afterPath := os.Getenv("GHOST_VERIFY_AFTER")
	project := os.Getenv("GHOST_VERIFY_PROJECT")
	if beforePath == "" || afterPath == "" || project == "" {
		t.Skip("set GHOST_VERIFY_BEFORE, GHOST_VERIFY_AFTER and GHOST_VERIFY_PROJECT")
	}
	ctx := context.Background()

	bdb := openSnapshot(t, beforePath)
	var before []memory.Memory
	brows, err := bdb.QueryContext(ctx, `
		SELECT category, content FROM memories
		WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL
	`, project)
	if err != nil {
		t.Fatalf("query before (if this is 'no such column', %s predates the current schema — migrate a copy first): %v", beforePath, err)
	}
	for brows.Next() {
		var cat, content string
		if err := brows.Scan(&cat, &content); err != nil {
			brows.Close() //nolint:errcheck
			t.Fatalf("scan before: %v", err)
		}
		before = append(before, memory.Memory{Category: cat, Content: content})
	}
	berr := brows.Err()
	brows.Close() //nolint:errcheck
	if berr != nil {
		t.Fatalf("before rows: %v", berr)
	}

	adb := openSnapshot(t, afterPath)
	var result ReflectionResult
	arows, err := adb.QueryContext(ctx, `
		SELECT category, content FROM memories
		WHERE source != 'manual' AND pinned = 0 AND resolved_at IS NULL
		  AND (project_id = ? OR project_id = '_global')
	`, project)
	if err != nil {
		t.Fatalf("query after (if this is 'no such column', %s predates the current schema — migrate a copy first): %v", afterPath, err)
	}
	for arows.Next() {
		var cat, content string
		if err := arows.Scan(&cat, &content); err != nil {
			arows.Close() //nolint:errcheck
			t.Fatalf("scan after: %v", err)
		}
		result.Memories = append(result.Memories, ReflectMemory{Category: cat, Content: content})
	}
	aerr := arows.Err()
	arows.Close() //nolint:errcheck
	if aerr != nil {
		t.Fatalf("after rows: %v", aerr)
	}

	drops := AuditGuardedDrops(ReflectionInput{ExistingMemories: before}, result)
	t.Logf("before(live)=%d  survivors(project+_global)=%d  uncovered guarded=%d",
		len(before), len(result.Memories), len(drops))
	for _, d := range drops {
		t.Errorf("UNCOVERED [%s] %.90s", d.Category, d.Content)
	}
}
