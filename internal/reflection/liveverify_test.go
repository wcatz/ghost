package reflection

import (
	"context"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestVerifyLiveCopy is a manual verification harness, skipped unless the env
// vars below are set. It runs the REAL guarded-drop audit over a before/after
// pair of ghost databases so a live-DB copy can be proven faithful without
// touching live data:
//
//	copy the live DB, run `ghost reflect <project> --apply` against the copy, then:
//	GHOST_VERIFY_BEFORE=<live.db> GHOST_VERIFY_AFTER=<copy.db> \
//	GHOST_VERIFY_PROJECT=<id> go test ./internal/reflection/ -run TestVerifyLiveCopy -v
//
// Survivors are the memories the run could actually have produced: the
// project's post-apply rows (non-manual, unpinned, unresolved — the same rows
// ReplaceNonManual replaces) plus _global, because the consolidator may
// legitimately promote a memory there (git identity, machine policy) and that
// is a scope move, not a loss. Manual/pinned/resolved rows are excluded: the
// pipeline never touched them, so counting them as survivors would let them
// mask a genuine drop. The DSN goes through memory.OpenDB so a '?' or '#' in
// the data-dir path is escaped, not parsed as a URI separator.
func TestVerifyLiveCopy(t *testing.T) {
	beforePath := os.Getenv("GHOST_VERIFY_BEFORE")
	afterPath := os.Getenv("GHOST_VERIFY_AFTER")
	project := os.Getenv("GHOST_VERIFY_PROJECT")
	if beforePath == "" || afterPath == "" || project == "" {
		t.Skip("set GHOST_VERIFY_BEFORE, GHOST_VERIFY_AFTER and GHOST_VERIFY_PROJECT")
	}
	ctx := context.Background()

	bdb, err := memory.OpenDB(beforePath)
	if err != nil {
		t.Fatalf("open before %s: %v", beforePath, err)
	}
	defer bdb.Close() //nolint:errcheck
	all, err := memory.NewStore(bdb, nil).GetAll(ctx, project, -1)
	if err != nil {
		t.Fatalf("getall before %s: %v", beforePath, err)
	}
	// The consolidation input: non-manual, unpinned, unresolved.
	var before []memory.Memory
	for _, m := range all {
		if m.Source == "manual" || m.Pinned || m.ResolvedAt != nil {
			continue
		}
		before = append(before, m)
	}

	adb, err := memory.OpenDB(afterPath)
	if err != nil {
		t.Fatalf("open after %s: %v", afterPath, err)
	}
	defer adb.Close() //nolint:errcheck
	rows, err := adb.QueryContext(ctx, `
		SELECT category, content FROM memories
		WHERE project_id = '_global'
		   OR (project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL)
	`, project)
	if err != nil {
		t.Fatalf("query after: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var result ReflectionResult
	for rows.Next() {
		var cat, content string
		if err := rows.Scan(&cat, &content); err != nil {
			t.Fatalf("scan after: %v", err)
		}
		result.Memories = append(result.Memories, ReflectMemory{Category: cat, Content: content})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("after rows: %v", err)
	}

	drops := AuditGuardedDrops(ReflectionInput{ExistingMemories: before}, result)
	t.Logf("before(live)=%d  survivors(project+_global)=%d  uncovered guarded=%d",
		len(before), len(result.Memories), len(drops))
	for _, d := range drops {
		t.Errorf("UNCOVERED [%s] %.90s", d.Category, d.Content)
	}
}
