package reflection

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// roDSN builds a read-only file: DSN with the same percent-encoding OpenDB uses
// (schema.go): a '?' or '#' in the data-dir path would otherwise be parsed as
// the URI query/fragment separator. Read-only deliberately — this harness must
// not mutate the copy it is certifying (OpenDB would set journal_mode, run
// initSQL, and could even migrate).
func roDSN(path string) string {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath()}
	return u.String() + "?mode=ro"
}

// TestVerifyLiveCopy is a manual verification harness, skipped unless the env
// vars below are set. It runs the REAL guarded-drop audit over a before/after
// pair of ghost databases so a live-DB copy can be proven faithful without
// touching live data:
//
//	copy the live DB, run `ghost reflect <project> --apply` against the copy, then:
//	GHOST_VERIFY_BEFORE=<live.db> GHOST_VERIFY_AFTER=<copy.db> \
//	GHOST_VERIFY_PROJECT=<id> go test ./internal/reflection/ -run TestVerifyLiveCopy -v
//
// Survivors are only the memories the run could actually have produced: the
// project's replaceable rows (non-manual, unpinned, unresolved — the rows
// ReplaceNonManual replaces) plus _global rows with the same filters, because
// the consolidator may legitimately promote a memory there and the apply path
// upserts promotions as source 'reflection'. Nothing else counts: a manual,
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

	bdb, err := sql.Open("sqlite", roDSN(beforePath))
	if err != nil {
		t.Fatalf("open before %s: %v", beforePath, err)
	}
	defer bdb.Close() //nolint:errcheck
	brows, err := bdb.QueryContext(ctx, `
		SELECT category, content FROM memories
		WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL
	`, project)
	if err != nil {
		t.Fatalf("query before: %v", err)
	}
	var before []memory.Memory
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

	adb, err := sql.Open("sqlite", roDSN(afterPath))
	if err != nil {
		t.Fatalf("open after %s: %v", afterPath, err)
	}
	defer adb.Close() //nolint:errcheck
	arows, err := adb.QueryContext(ctx, `
		SELECT category, content FROM memories
		WHERE source != 'manual' AND pinned = 0 AND resolved_at IS NULL
		  AND (project_id = ? OR project_id = '_global')
	`, project)
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	defer arows.Close() //nolint:errcheck
	var result ReflectionResult
	for arows.Next() {
		var cat, content string
		if err := arows.Scan(&cat, &content); err != nil {
			t.Fatalf("scan after: %v", err)
		}
		result.Memories = append(result.Memories, ReflectMemory{Category: cat, Content: content})
	}
	if err := arows.Err(); err != nil {
		t.Fatalf("after rows: %v", err)
	}

	drops := AuditGuardedDrops(ReflectionInput{ExistingMemories: before}, result)
	t.Logf("before(live)=%d  survivors(project+_global)=%d  uncovered guarded=%d",
		len(before), len(result.Memories), len(drops))
	for _, d := range drops {
		t.Errorf("UNCOVERED [%s] %.90s", d.Category, d.Content)
	}
}
